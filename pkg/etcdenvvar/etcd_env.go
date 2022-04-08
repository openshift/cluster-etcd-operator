package etcdenvvar

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	v1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

type envVarContext struct {
	spec   operatorv1.StaticPodOperatorSpec
	status operatorv1.StaticPodOperatorStatus

	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	configmapLister      corev1listers.ConfigMapLister
	targetImagePullSpec  string
}

var FixedEtcdEnvVars = map[string]string{
	"ETCD_DATA_DIR":                                    "/var/lib/etcd",
	"ETCD_QUOTA_BACKEND_BYTES":                         "8589934592", // 8 GB
	"ETCD_INITIAL_CLUSTER_STATE":                       "existing",
	"ETCD_ENABLE_PPROF":                                "true",
	"ETCD_EXPERIMENTAL_WATCH_PROGRESS_NOTIFY_INTERVAL": "5s",
	"ETCD_SOCKET_REUSE_ADDRESS":                        "true",
	"ETCD_EXPERIMENTAL_WARNING_APPLY_DURATION":         "200ms",
}

const (
	etcdEndpointName  = "etcd-endpoints"
	clusterConfigName = "cluster-config-v1"
	clusterConfigKey  = "install-config"
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

type envVarFunc func(envVarContext envVarContext) (map[string]string, error)

var envVarFns = []envVarFunc{
	getEscapedIPAddress,
	getEtcdURLHost,
	getFixedEtcdEnvVars,
	getEtcdName,
	getAllEtcdEndpoints,
	getEtcdctlEnvVars,
	getHeartbeatInterval,
	getElectionTimeout,
	getUnsupportedArch,
	getCipherSuites,
	getMaxLearners,
}

// getEtcdEnvVars returns the env vars that need to be set on the etcd static pods that will be rendered.
//   ALL_ETCD_ENDPOINTS - this is used to drive the ETCD_INITIAL_CLUSTER
//   ETCD_DATA_DIR
//   ETCDCTL_API
//   ETCD_QUOTA_BACKEND_BYTES
//   ETCD_HEARTBEAT_INTERVAL
//   ETCD_ELECTION_TIMEOUT
//   ETCD_INITIAL_CLUSTER_STATE
//   ETCD_UNSUPPORTED_ARCH
//   ETCD_CIPHER_SUITES
//   ETCD_EXPERIMENTAL_MAX_LEARNERS
//   NODE_%s_IP
//   NODE_%s_ETCD_URL_HOST
//   NODE_%s_ETCD_NAME
func getEtcdEnvVars(envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	for _, envVarFn := range envVarFns {
		newEnvVars, err := envVarFn(envVarContext)
		if err != nil {
			return nil, err
		}
		if newEnvVars == nil {
			continue
		}
		for k, v := range newEnvVars {
			if currV, ok := ret[k]; ok {
				return nil, fmt.Errorf("key %q already set to %q", k, currV)
			}
			ret[k] = v
		}
	}

	return ret, nil
}

func getFixedEtcdEnvVars(envVarContext envVarContext) (map[string]string, error) {
	return FixedEtcdEnvVars, nil
}

func getEtcdctlEnvVars(envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdEndpoints(envVarContext.configmapLister, true)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"ETCDCTL_API":       "3",
		"ETCDCTL_CACERT":    "/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt",
		"ETCDCTL_CERT":      "/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt",
		"ETCDCTL_KEY":       "/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key",
		"ETCDCTL_ENDPOINTS": endpoints,
		"ETCD_IMAGE":        envVarContext.targetImagePullSpec,
	}, nil
}

func getAllEtcdEndpoints(envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdEndpoints(envVarContext.configmapLister, false)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"ALL_ETCD_ENDPOINTS": endpoints,
	}, nil
}

func getEtcdName(envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		ret[fmt.Sprintf("NODE_%s_ETCD_NAME", envVarSafe(nodeInfo.NodeName))] = nodeInfo.NodeName
	}

	return ret, nil
}

func getEscapedIPAddress(envVarContext envVarContext) (map[string]string, error) {
	network, err := envVarContext.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	ret := map[string]string{}
	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		node, err := envVarContext.nodeLister.Get(nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}

		escapedIPAddress, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}
		ret[fmt.Sprintf("NODE_%s_IP", envVarSafe(nodeInfo.NodeName))] = escapedIPAddress
	}

	return ret, nil
}

func getEtcdURLHost(envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	network, err := envVarContext.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		node, err := envVarContext.nodeLister.Get(nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}
		etcdURLHost, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}

		ret[fmt.Sprintf("NODE_%s_ETCD_URL_HOST", envVarSafe(nodeInfo.NodeName))] = etcdURLHost
	}

	return ret, nil
}

func getHeartbeatInterval(envVarContext envVarContext) (map[string]string, error) {
	heartbeat := "100" // etcd default

	infrastructure, err := envVarContext.infrastructureLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	if status := infrastructure.Status.PlatformStatus; status != nil {
		switch {
		case status.Azure != nil:
			heartbeat = "500"
		case status.IBMCloud != nil:
			if infrastructure.Status.PlatformStatus.IBMCloud.ProviderType == v1.IBMCloudProviderTypeVPC {
				heartbeat = "500"
			}
		}
	}

	return map[string]string{
		"ETCD_HEARTBEAT_INTERVAL": heartbeat,
	}, nil
}

func getElectionTimeout(envVarContext envVarContext) (map[string]string, error) {
	timeout := "1000" // etcd default

	infrastructure, err := envVarContext.infrastructureLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	if status := infrastructure.Status.PlatformStatus; status != nil {
		switch {
		case status.Azure != nil:
			timeout = "2500"
		case status.IBMCloud != nil:
			if infrastructure.Status.PlatformStatus.IBMCloud.ProviderType == v1.IBMCloudProviderTypeVPC {
				timeout = "2500"
			}
		}
	}

	return map[string]string{
		"ETCD_ELECTION_TIMEOUT": timeout,
	}, nil
}

func envVarSafe(nodeName string) string {
	return strings.ReplaceAll(strings.ReplaceAll(nodeName, "-", "_"), ".", "_")
}

func getUnsupportedArch(envVarContext envVarContext) (map[string]string, error) {
	arch := runtime.GOARCH
	switch arch {
	case "arm64":
	case "s390x":
	default:
		// dont set unless it is defined.
		return nil, nil
	}
	return map[string]string{
		"ETCD_UNSUPPORTED_ARCH": arch,
	}, nil
}

func getCipherSuites(envVarContext envVarContext) (map[string]string, error) {
	var observedConfig map[string]interface{}
	if err := yaml.Unmarshal(envVarContext.spec.ObservedConfig.Raw, &observedConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal the observedConfig: %w", err)
	}
	observedCipherSuites, _, err := unstructured.NestedStringSlice(observedConfig, "servingInfo", "cipherSuites")
	if err != nil {
		return nil, fmt.Errorf("couldn't get cipherSuites from observedConfig: %w", err)
	}

	actualCipherSuites := tlshelpers.SupportedEtcdCiphers(observedCipherSuites)

	if len(actualCipherSuites) == 0 {
		return nil, fmt.Errorf("no supported cipherSuites not found in observedConfig")
	}

	return map[string]string{
		"ETCD_CIPHER_SUITES": strings.Join(actualCipherSuites, ","),
	}, nil
}

// getMaxLearners reads the control-plane replicas from the install-config. We tolerate max learners equal to the
// desired control-plane replicas so that the admin can surge up N x 2 in case of vertical scaling/replacement.
func getMaxLearners(envVarContext envVarContext) (map[string]string, error) {
	clusterConfig, err := envVarContext.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get(clusterConfigName)
	if err != nil {
		errMsg := fmt.Errorf("failed to get configmap %s/%s :%v", operatorclient.TargetNamespace, clusterConfigName, err)
		klog.Error(errMsg)
		return nil, errMsg
	}

	d := replicaCountDecoder{}
	if err := yaml.Unmarshal([]byte(clusterConfig.Data[clusterConfigKey]), &d); err != nil {
		errMsg := fmt.Errorf("%s key doesn't exist in configmap %s/%s :%w", clusterConfigKey, operatorclient.TargetNamespace, clusterConfigName, err)
		klog.Error(errMsg)
		return nil, errMsg
	}

	replicaCount, err := strconv.Atoi(d.ControlPlane.Replicas)
	if err != nil {
		errMsg := fmt.Errorf("failed to convert replica %s: %w", d.ControlPlane.Replicas, err)
		klog.Error(errMsg)
		return nil, errMsg
	}

	return map[string]string{
		"ETCD_EXPERIMENTAL_MAX_LEARNERS": fmt.Sprint(replicaCount),
	}, nil
}

func getEtcdEndpoints(configmapLister corev1listers.ConfigMapLister, skipBootstrap bool) (string, error) {
	etcdEndpoints, err := configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get(etcdEndpointName)
	if err != nil {
		return "", err
	}
	var etcdURLs []string
	bootstrapAddress := etcdEndpoints.Annotations["alpha.installer.openshift.io/etcd-bootstrap"]
	// In some cases we want to exclude the ephemeral bootstrap-etcd member from the endpoint list.
	if !skipBootstrap && len(bootstrapAddress) > 0 {
		ip, err := dnshelpers.GetURLHostForIP(bootstrapAddress)
		if err != nil {
			return "", fmt.Errorf("configmaps/%s contains invalid bootstrap ip address: %s: %v", etcdEndpointName, bootstrapAddress, err)
		}
		etcdURLs = append(etcdURLs, fmt.Sprintf("https://%s:2379", ip))
	}
	for k := range etcdEndpoints.Data {
		address := etcdEndpoints.Data[k]
		ip, err := dnshelpers.GetURLHostForIP(address)
		if err != nil {
			return "", fmt.Errorf("configmaps/%s contains invalid ip address: %s: %v", etcdEndpointName, address, err)
		}
		etcdURLs = append(etcdURLs, fmt.Sprintf("https://%s:2379", ip))
	}
	sort.Strings(etcdURLs)

	return strings.Join(etcdURLs, ","), nil
}
