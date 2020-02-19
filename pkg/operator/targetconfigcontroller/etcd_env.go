package targetconfigcontroller

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
	getAllClusterMembers,
}

// getEtcdEnvVars returns the env vars that need to be set on the etcd static pods that will be rendered.
//   ALL_ETCD_ENDPOINTS - this is used to drive the ETCD_INITIAL_CLUSTER
//   ETCD_DATA_DIR
//   ETCDCTL_API
//   ETCD_QUOTA_BACKEND_BYTES
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
		"ETCDCTL_API":                "3",
		"ETCD_INITIAL_CLUSTER_STATE": "existing",
	}, nil
}

func getAllClusterMembers(envVarContext envVarContext) (map[string]string, error) {
	network, err := envVarContext.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	ret := map[string]string{}

	endpoints := []string{}
	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		node, err := envVarContext.nodeLister.Get(nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}

		endpointIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, fmt.Sprintf("https://%s:2379", endpointIP))
	}

	hostEtcdEndpoints, err := envVarContext.endpointLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
	if err != nil {
		return nil, err
	}
	for _, endpointAddress := range hostEtcdEndpoints.Subsets[0].Addresses {
		if endpointAddress.Hostname == "etcd-bootstrap" {

			isIPV4, err := dnshelpers.IsIPv4(endpointAddress.IP)
			if err != nil {
				return nil, err
			}
			if isIPV4 {
				endpoints = append(endpoints, "https://"+endpointAddress.IP+":2379")
			} else {
				endpoints = append(endpoints, "https://["+endpointAddress.IP+"]:2379")
			}

			break
		}
	}
	ret["ALL_ETCD_ENDPOINTS"] = strings.Join(endpoints, ",")

	return ret, nil
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
		ret[fmt.Sprintf("NODE_%s_IP", envVarSafe(nodeInfo.NodeName))] = "[" + escapedIPAddress + "]"
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

func envVarSafe(nodeName string) string {
	return strings.ReplaceAll(strings.ReplaceAll(nodeName, "-", "_"), ".", "_")
}
