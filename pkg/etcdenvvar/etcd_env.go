package etcdenvvar

import (
	"fmt"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"
	v1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/hwspeedhelpers"
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
	etcdLister           operatorv1listers.EtcdLister
	featureGateAccessor  featuregates.FeatureGateAccess
}

var FixedEtcdEnvVars = map[string]string{
	"ETCD_DATA_DIR":              "/var/lib/etcd",
	"ETCD_INITIAL_CLUSTER_STATE": "existing",
	"ETCD_ENABLE_PPROF":          "true",
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
	getHardwareSpeedValues,
	getEtcdDBSize,
	getUnsupportedArch,
	getCipherSuites,
	getMaxLearners,
}

// getEtcdEnvVars returns the env vars that need to be set on the etcd static pods that will be rendered.
//
//	ALL_ETCD_ENDPOINTS - this is used to drive the ETCD_INITIAL_CLUSTER
//	ETCD_DATA_DIR
//	ETCDCTL_API
//	ETCD_QUOTA_BACKEND_BYTES
//	ETCD_HEARTBEAT_INTERVAL
//	ETCD_ELECTION_TIMEOUT
//	ETCD_INITIAL_CLUSTER_STATE
//	ETCD_UNSUPPORTED_ARCH
//	ETCD_CIPHER_SUITES
//	ETCD_EXPERIMENTAL_MAX_LEARNERS
//	NODE_%s_IP
//	NODE_%s_ETCD_URL_HOST
//	NODE_%s_ETCD_NAME
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

func getFixedEtcdEnvVars(_ envVarContext) (map[string]string, error) {
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

	if envVarContext.status.NodeStatuses == nil || len(envVarContext.status.NodeStatuses) == 0 {
		return nil, fmt.Errorf("empty NodeStatuses, can't generate environment for getEtcdName")
	}

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

	if envVarContext.status.NodeStatuses == nil || len(envVarContext.status.NodeStatuses) == 0 {
		return nil, fmt.Errorf("empty NodeStatuses, can't generate environment for getEscapedIPAddress")
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

	if envVarContext.status.NodeStatuses == nil || len(envVarContext.status.NodeStatuses) == 0 {
		return nil, fmt.Errorf("empty NodeStatuses, can't generate environment for getEtcdURLHost")
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

func getHardwareSpeedValues(envVarContext envVarContext) (map[string]string, error) {
	etcd, err := envVarContext.etcdLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	speed := etcd.Spec.HardwareSpeed

	// If the speed from the api is empty, then the CEO gets to decide what to set the values to.
	//	Allows for upgrades from before this api field was added.
	if speed == "" {
		envs := hwspeedhelpers.StandardHardwareSpeed()
		infrastructure, err := envVarContext.infrastructureLister.Get("cluster")
		if err != nil {
			return nil, err
		}
		if status := infrastructure.Status.PlatformStatus; status != nil {
			switch {
			case status.Azure != nil:
				envs = hwspeedhelpers.SlowerHardwareSpeed()
			case status.IBMCloud != nil:
				if infrastructure.Status.PlatformStatus.IBMCloud.ProviderType == v1.IBMCloudProviderTypeVPC {
					envs = hwspeedhelpers.SlowerHardwareSpeed()
				}
			}
		}
		return envs, err
	}

	return hwspeedhelpers.HardwareSpeedToEnvMap(speed)
}

func getEtcdDBSize(envVarContext envVarContext) (map[string]string, error) {
	etcd, err := envVarContext.etcdLister.Get("cluster")
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve etcd CR: %v", err)
	}

	if etcd.Spec.BackendQuotaGiB == 0 {
		return map[string]string{
			"ETCD_QUOTA_BACKEND_BYTES": gibibytesToBytesString(8),
		}, nil
	}

	etcdDBSize := etcd.Spec.BackendQuotaGiB
	return map[string]string{
		"ETCD_QUOTA_BACKEND_BYTES": gibibytesToBytesString(etcdDBSize),
	}, nil
}

func envVarSafe(nodeName string) string {
	return strings.ReplaceAll(strings.ReplaceAll(nodeName, "-", "_"), ".", "_")
}

func getUnsupportedArch(_ envVarContext) (map[string]string, error) {
	arch := runtime.GOARCH
	switch arch {
	case "arm64":
	case "s390x":
	default:
		// don't set unless it is defined.
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
		if ip := net.ParseIP(bootstrapAddress); ip == nil {
			return "", fmt.Errorf("configmaps/%s contains invalid bootstrap ip address: %s: %v", etcdEndpointName, bootstrapAddress, err)
		}
		etcdURLs = append(etcdURLs, fmt.Sprintf("https://%s", net.JoinHostPort(bootstrapAddress, "2379")))
	}
	for k := range etcdEndpoints.Data {
		address := etcdEndpoints.Data[k]
		if ip := net.ParseIP(address); ip == nil {
			return "", fmt.Errorf("configmaps/%s contains invalid ip address: %s: %v", etcdEndpointName, address, err)
		}
		etcdURLs = append(etcdURLs, fmt.Sprintf("https://%s", net.JoinHostPort(address, "2379")))
	}
	sort.Strings(etcdURLs)

	return strings.Join(etcdURLs, ","), nil
}

func gibibytesToBytesString(dbSize int64) string {
	etcdDBSize := dbSize * 1024 * 1024 * 1024
	return strconv.FormatInt(etcdDBSize, 10)
}
