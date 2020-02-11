package targetconfigcontroller

import (
	"fmt"
	"net"
	"strings"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
)

type envVarContext struct {
	spec   operatorv1.StaticPodOperatorSpec
	status operatorv1.StaticPodOperatorStatus

	endpointLister corev1listers.EndpointsLister
	nodeLister     corev1listers.NodeLister
	dynamicClient  dynamic.Interface
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
		nodeDNSName, err := getInternalIPDNSNodeName(envVarContext, nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}
		ret[fmt.Sprintf("NODE_%s_ETCD_NAME", envVarSafe(nodeInfo.NodeName))] = nodeDNSName
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

func getInternalIPDNSNodeName(envVarContext envVarContext, nodeName string) (string, error) {
	node, err := envVarContext.nodeLister.Get(nodeName)
	if err != nil {
		return "", err
	}

	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalDNS {
			return currAddress.Address, nil
		}
	}
	return "", fmt.Errorf("node/%s missing %s", node.Name, corev1.NodeInternalDNS)
}

func getDNSName(envVarContext envVarContext) (map[string]string, error) {
	ret := map[string]string{}

	controllerConfig, err := envVarContext.dynamicClient.
		Resource(schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "controllerconfigs"}).
		Get("machine-config-controller", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	etcdDiscoveryDomain, ok, err := unstructured.NestedString(controllerConfig.Object, "spec", "etcdDiscoveryDomain")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("controllerconfigs/machine-config-controller missing .spec.etcdDiscoveryDomain")
	}

	for _, nodeInfo := range envVarContext.status.NodeStatuses {
		ip, err := getInternalIPAddressForNodeName(envVarContext, nodeInfo.NodeName)
		if err != nil {
			return nil, err
		}

		dnsName, err := reverseLookup("etcd-server-ssl", "tcp", etcdDiscoveryDomain, ip)
		if err != nil {
			return nil, err
		}
		ret[fmt.Sprintf("NODE_%s_ETCD_DNS_NAME", envVarSafe(nodeInfo.NodeName))] = dnsName
	}

	return ret, nil
}

// returns the target from the SRV record that resolves to ip.
func reverseLookup(service, proto, name, ip string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == ip {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}

func envVarSafe(nodeName string) string {
	return strings.ReplaceAll(strings.ReplaceAll(nodeName, "-", "_"), ".", "_")
}
