package targetconfigcontroller

import (
	"fmt"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type envVarContext struct {
	spec   operatorv1.StaticPodOperatorSpec
	status operatorv1.StaticPodOperatorStatus

	nodeLister     corev1listers.NodeLister
	endpointLister corev1listers.EndpointsLister
}

type envVarFunc func(envVarContext envVarContext) (map[string]string, error)

var envVarFns = []envVarFunc{
	getEscapedIPAddress,
	getPeerURLHost,
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
//   NODE_%s_ETCD_PEER_URL_HOST
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
	ret := map[string]string{}

	endpoints := []string{}
	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		endpoint, err := getInternalIPAddressForNodeName(envVarContext, nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, fmt.Sprintf("https://%s:2379", endpoint))
	}

	hostEtcdEndpoints, err := envVarContext.endpointLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
	if err != nil {
		return nil, err
	}
	for _, endpointAddress := range hostEtcdEndpoints.Subsets[0].Addresses {
		if endpointAddress.Hostname == "etcd-bootstrap" {
			endpoints = append(endpoints, "https://"+endpointAddress.IP+":2379")
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
	ret := map[string]string{}
	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		address, err := getInternalIPAddressForNodeName(envVarContext, nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}
		ret[fmt.Sprintf("NODE_%s_IP", envVarSafe(nodeInfo.NodeName))] = address
	}

	return ret, nil
}

func getInternalIPAddressForNodeName(envVarContext envVarContext, nodeName string) (string, error) {
	node, err := envVarContext.nodeLister.Get(nodeName)
	if err != nil {
		return "", err
	}

	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalIP {
			return currAddress.Address, nil
		}
	}
	return "", fmt.Errorf("node/%s missing %s", node.Name, corev1.NodeInternalIP)
}

func getPeerURLHost(envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		ip, err := getInternalIPAddressForNodeName(envVarContext, nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}

		ret[fmt.Sprintf("NODE_%s_ETCD_PEER_URL_HOST", envVarSafe(nodeInfo.NodeName))] = ip
	}

	return ret, nil
}

func envVarSafe(nodeName string) string {
	return strings.ReplaceAll(strings.ReplaceAll(nodeName, "-", "_"), ".", "_")
}
