package etcdenvvar

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/crypto"
	"go.etcd.io/etcd/pkg/tlsutil"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
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
	"ETCD_QUOTA_BACKEND_BYTES":                         "7516192768", // 7 gig
	"ETCD_INITIAL_CLUSTER_STATE":                       "existing",
	"ETCD_ENABLE_PPROF":                                "true",
	"ETCD_CIPHER_SUITES":                               getDefaultCipherSuites(),
	"ETCD_EXPERIMENTAL_WATCH_PROGRESS_NOTIFY_INTERVAL": "5s",
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
	endpoints, err := getEtcdGrpcEndpoints(envVarContext)
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

func getEtcdGrpcEndpoints(envVarContext envVarContext) (string, error) {
	network, err := envVarContext.networkLister.Get("cluster")
	if err != nil {
		return "", err
	}

	endpoints := []string{}
	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		node, err := envVarContext.nodeLister.Get(nodeInfo.NodeName)
		if err != nil {
			return "", err
		}

		endpointIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return "", err
		}
		endpoints = append(endpoints, fmt.Sprintf("https://%s:2379", endpointIP))
	}

	return strings.Join(endpoints, ","), nil
}

func getAllEtcdEndpoints(envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdGrpcEndpoints(envVarContext)
	if err != nil {
		return nil, err
	}

	etcdEndpoints, err := envVarContext.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
	if err != nil {
		return nil, err
	}
	if bootstrapIP := etcdEndpoints.Annotations["alpha.installer.openshift.io/etcd-bootstrap"]; len(bootstrapIP) > 0 {
		urlHost, err := dnshelpers.GetURLHostForIP(bootstrapIP)
		if err != nil {
			return nil, err
		}
		endpoints += ",https://" + urlHost + ":2379"
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

//TODO replace with TLS security policy observer
func getDefaultCipherSuites() string {
	profileSpec := configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	// whitelist ciphers for use with etcd
	cipherSuites := whitelistEtcdCipherSuites(crypto.OpenSSLToIANACipherSuites(profileSpec.Ciphers))
	return strings.Join(cipherSuites, ",")
}

// whitelistEtcdCipherSuites ensures ciphers are valid for use with etcd.
// TODO move upstream
func whitelistEtcdCipherSuites(cipherSuites []string) []string {
	whitelist := []string{}
	for _, cipher := range cipherSuites {
		_, ok := tlsutil.GetCipherSuite(cipher)
		if !ok {
			// skip and log unsupported ciphers
			klog.Warningf("cipher is not supported for use with etcd: %q", cipher)
			continue
		}
		whitelist = append(whitelist, cipher)
	}
	return whitelist
}
