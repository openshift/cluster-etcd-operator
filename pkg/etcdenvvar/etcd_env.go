package etcdenvvar

import (
	"context"
	"fmt"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"net"
	"net/url"
	"runtime"
	"sort"
	"strings"

	"github.com/ghodss/yaml"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

const etcdEndpointName = "etcd-endpoints"

type envVarContext struct {
	spec   operatorv1.StaticPodOperatorSpec
	status operatorv1.StaticPodOperatorStatus

	etcdClient           etcdcli.EtcdClient
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

type envVarFunc func(ctx context.Context, envVarContext envVarContext) (map[string]string, error)

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
	getInitialCluster,
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
//   ETCD_INITIAL_CLUSTER
//   NODE_%s_IP
//   NODE_%s_ETCD_URL_HOST
//   NODE_%s_ETCD_NAME
func getEtcdEnvVars(ctx context.Context, envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	for _, envVarFn := range envVarFns {
		newEnvVars, err := envVarFn(ctx, envVarContext)
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

func getFixedEtcdEnvVars(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
	return FixedEtcdEnvVars, nil
}

func getEtcdctlEnvVars(ctx context.Context, envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdGrpcEndpoints(ctx, envVarContext)
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

func getEtcdGrpcEndpoints(ctx context.Context, envVarContext envVarContext) (string, error) {
	members, err := envVarContext.etcdClient.MemberList(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get member list: %w", err)
	}

	var endpoints []string
	var errs []error
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			continue
		}
		u, err := url.Parse(member.PeerURLs[0])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		endpoints = append(endpoints, fmt.Sprintf("https://%s:2379", host))
	}

	if len(errs) > 0 {
		return "", utilerrors.NewAggregate(errs)
	}

	sort.Strings(endpoints)

	return strings.Join(endpoints, ","), nil
}

func getAllEtcdEndpoints(ctx context.Context, envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdGrpcEndpoints(ctx, envVarContext)
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

func getEtcdName(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		ret[fmt.Sprintf("NODE_%s_ETCD_NAME", envVarSafe(nodeInfo.NodeName))] = nodeInfo.NodeName
	}

	return ret, nil
}

func getEscapedIPAddress(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
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

func getEtcdURLHost(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
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

func getHeartbeatInterval(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
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

func getElectionTimeout(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
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

func getUnsupportedArch(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
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

func getCipherSuites(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
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

func getInitialCluster(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
	network, err := envVarContext.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	etcdEndpoints, err := envVarContext.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
	if err != nil {
		return nil, fmt.Errorf("could not get configmap/etcd-endpoints: %w", err)
	}

	nodes, err := envVarContext.nodeLister.List(labels.SelectorFromSet(labels.Set{"node-role.kubernetes.io/master": ""}))
	if err != nil {
		return nil, fmt.Errorf("could not list nodes: %w", err)
	}

	nodeMap := make(map[string]string, len(nodes))
	for _, node := range nodes {
		internalIp, _, err := dnshelpers.GetPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}
		nodeMap[internalIp] = node.Name
	}

	var initialCluster sort.StringSlice
	if bootstrapIP := etcdEndpoints.Annotations["alpha.installer.openshift.io/etcd-bootstrap"]; len(bootstrapIP) > 0 {
		urlHost, err := dnshelpers.GetURLHostForIP(bootstrapIP)
		if err != nil {
			return nil, err
		}
		initialCluster = append(initialCluster, fmt.Sprintf("bootstrap-etcd=https://%s:2380", urlHost))
	}

	for k := range etcdEndpoints.Data {
		address := etcdEndpoints.Data[k]
		// Verify node exists for proposed member
		hostname, found := nodeMap[address]
		if !found {
			return nil, fmt.Errorf("configmap/%s in namespace/%s endpoint address: %s not mapped to a control-plane node", etcdEndpoints, operatorclient.TargetNamespace, address)
		}
		urlHost, err := dnshelpers.GetURLHostForIP(address)
		if err != nil {
			return nil, err
		}
		initialCluster = append(initialCluster, fmt.Sprintf("%s=https://%s:2380", hostname, urlHost))
	}

	if len(initialCluster) == 0 {
		return nil, fmt.Errorf("configmap/%s in namespace/%s does not contain any endpoint data", etcdEndpoints, operatorclient.TargetNamespace)
	}

	// ensure uniform order
	initialCluster.Sort()
	return map[string]string{
		"ETCD_INITIAL_CLUSTER": strings.Join(initialCluster, ","),
	}, nil
}

//func getInitialCluster(_ context.Context, envVarContext envVarContext) (map[string]string, error) {
//	network, err := envVarContext.networkLister.Get("cluster")
//	if err != nil {
//		return nil, err
//	}
//
//	etcdEndpoints, err := envVarContext.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
//	if err != nil {
//		return nil, err
//	}
//	var initialCluster []string
//	if bootstrapIP := etcdEndpoints.Annotations["alpha.installer.openshift.io/etcd-bootstrap"]; len(bootstrapIP) > 0 {
//		urlHost, err := dnshelpers.GetURLHostForIP(bootstrapIP)
//		if err != nil {
//			return nil, err
//		}
//		initialCluster = append(initialCluster, fmt.Sprintf("bootstrap-etcd=https://%s:2380", urlHost))
//	}
//
//	for _, nodeInfo := range envVarContext.status.NodeStatuses {
//		node, err := envVarContext.nodeLister.Get(nodeInfo.NodeName)
//		if err != nil {
//			return nil, err
//		}
//		etcdURLHost, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
//		if err != nil {
//			return nil, err
//		}
//		initialCluster = append(initialCluster, fmt.Sprintf("%s=https://%s:2380", nodeInfo.NodeName, etcdURLHost))
//	}
//	// ensure order is always the same
//	sort.Strings(initialCluster)
//
//	return map[string]string{
//		"ETCD_INITIAL_CLUSTER": strings.Join(initialCluster, ","),
//	}, nil
//}
