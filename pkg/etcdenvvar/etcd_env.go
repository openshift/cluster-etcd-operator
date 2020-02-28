package etcdenvvar

import (
	"fmt"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type envVarContext struct {
	spec   operatorv1.StaticPodOperatorSpec
	status operatorv1.StaticPodOperatorStatus

	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	endpointLister       corev1listers.EndpointsLister
}

type envVarFunc func(envVarContext envVarContext) (map[string]string, error)

var envVarFns = []envVarFunc{
	getEscapedIPAddress,
	getDNSName,
	getFixedEtcdEnvVars,
	getEtcdName,
	getAllEtcdEndpoints,
	getEtcdctlEnvVars,
	getHeartbeatInterval,
	getElectionTimeout,
}

// getEtcdEnvVars returns the env vars that need to be set on the etcd static pods that will be rendered.
//   ALL_ETCD_ENDPOINTS - this is used to drive the ETCD_INITIAL_CLUSTER
//   ETCD_DATA_DIR
//   ETCDCTL_API
//   ETCD_QUOTA_BACKEND_BYTES
//   ETCD_HEARTBEAT_INTERVAL
//   ETCD_ELECTION_TIMEOUT
//   ETCD_INITIAL_CLUSTER_STATE
//   NODE_%s_IP
//   NODE_%s_ETCD_DNS_NAME
//   NODE_%s_ETCD_NAME
func getEtcdEnvVars(envVarContext envVarContext) (map[string]string, error) {
	// TODO once we are past bootstrapping, this restriction shouldn't be needed anymore.
	//   we have it because the env vars were not getting set in the pod and the static pod operator started
	//   rolling out to another node, which caused a failure.
	if len(envVarContext.status.NodeStatuses) < 3 {
		return nil, fmt.Errorf("at least three nodes are required to have a valid configuration")
	}

	ret := map[string]string{}

	for _, envVarFn := range envVarFns {
		newEnvVars, err := envVarFn(envVarContext)
		if err != nil {
			return nil, err
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
	return map[string]string{
		"ETCD_DATA_DIR":              "/var/lib/etcd",
		"ETCD_QUOTA_BACKEND_BYTES":   "7516192768", // 7 gig
		"ETCD_INITIAL_CLUSTER_STATE": "existing",
	}, nil
}

func getEtcdctlEnvVars(envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdGrpcEndpoints(envVarContext)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"ETCDCTL_API":       "3",
		"ETCDCTL_CACERT":    "/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt",
		"ETCDCTL_CERT":      "/etc/kubernetes/static-pod-certs/secrets/etcd-all-peer/etcd-peer-NODE_NAME.crt",
		"ETCDCTL_KEY":       "/etc/kubernetes/static-pod-certs/secrets/etcd-all-peer/etcd-peer-NODE_NAME.key",
		"ETCDCTL_ENDPOINTS": endpoints,
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

	hostEtcdEndpoints, err := envVarContext.endpointLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd-2")
	if err != nil {
		return "", err
	}
	if bootstrapIP := hostEtcdEndpoints.Annotations["alpha.installer.openshift.io/etcd-bootstrap"]; len(bootstrapIP) > 0 {
		urlHost, err := dnshelpers.GetURLHostForIP(bootstrapIP)
		if err != nil {
			return "", err
		}
		endpoints = append(endpoints, "https://"+urlHost+":2379")
	}

	return strings.Join(endpoints, ","), nil
}

func getAllEtcdEndpoints(envVarContext envVarContext) (map[string]string, error) {
	endpoints, err := getEtcdGrpcEndpoints(envVarContext)
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

func getDNSName(envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	infrastructure, err := envVarContext.infrastructureLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	etcdDiscoveryDomain := infrastructure.Status.EtcdDiscoveryDomain
	if len(etcdDiscoveryDomain) == 0 {
		return nil, fmt.Errorf("infrastructures.config.openshit.io/cluster missing .status.etcdDiscoveryDomain")
	}

	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		node, err := envVarContext.nodeLister.Get(nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}

		ips, err := dnshelpers.GetInternalIPAddressesForNodeName(node)
		if err != nil {
			return nil, err
		}

		dnsName, err := dnshelpers.ReverseLookupFirstHit(etcdDiscoveryDomain, ips...)
		if err != nil {
			return nil, err
		}
		ret[fmt.Sprintf("NODE_%s_ETCD_DNS_NAME", envVarSafe(nodeInfo.NodeName))] = dnsName
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
