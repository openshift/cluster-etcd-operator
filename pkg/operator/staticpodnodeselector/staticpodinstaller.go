package staticpodinstaller

import (
	"context"
	"fmt"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

func GetStaticPodInstallerNodeSelectorMap(nodeLister corev1listers.NodeLister, configmapLister corev1listers.ConfigMapLister) func(ctx context.Context) (map[string]bool, error) {
	return func(ctx context.Context) (map[string]bool, error) {
		nodes, err := nodeLister.List(labels.SelectorFromSet(labels.Set{"node-role.kubernetes.io/master": ""}))
		if err != nil {
			return nil, fmt.Errorf("could not list nodes: %w", err)
		}

		etcdEndpoints, err := configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
		if err != nil {
			return nil, fmt.Errorf("could not get configmap/etcd-endpoints: %w", err)
		}
		endPointMap := make(map[string]struct{}, len(etcdEndpoints.Data))
		for k := range etcdEndpoints.Data {
			address := etcdEndpoints.Data[k]
			endPointMap[address] = struct{}{}
		}

		nodeMap := make(map[string]bool, len(nodes))
		for _, node := range nodes {
			if isEndpointNodeAddress(node, endPointMap) {
				nodeMap[node.Name] = true
				continue
			}
			nodeMap[node.Name] = false
		}

		return nodeMap, nil
	}
}

func isEndpoint(addressMap map[string]struct{}, address string) bool {
	_, ok := addressMap[address]
	return ok
}
func isEndpointNodeAddress(node *corev1.Node, addressMap map[string]struct{}) bool {
	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalIP && isEndpoint(addressMap, currAddress.Address) {
			return true
		}
	}
	return false
}