package operator

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

// getPacemakerNodes returns a map of node name -> IP from the PacemakerCluster CR.
func (c *PacemakerLifecycleManager) getPacemakerNodes() (map[string]string, error) {
	// Check if pacemaker informer has synced
	if c.pacemakerInformer == nil || !c.pacemakerInformer.HasSynced() {
		return nil, fmt.Errorf("pacemakerInformer not synced yet")
	}

	// Get PacemakerCluster CR from informer cache
	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(pacemaker.PacemakerClusterResourceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get PacemakerCluster from cache: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("PacemakerCluster CR not found")
	}

	pacemakerCR, ok := item.(*pacmkrv1.PacemakerCluster)
	if !ok {
		return nil, fmt.Errorf("failed to convert to PacemakerCluster")
	}

	if pacemakerCR.Status.Nodes == nil {
		return nil, fmt.Errorf("PacemakerCluster CR has no nodes in status")
	}

	// Build map of nodeName -> IP (use first address as primary)
	pmNodes := make(map[string]string)
	for _, node := range *pacemakerCR.Status.Nodes {
		if len(node.Addresses) == 0 {
			klog.Warningf("Pacemaker node %q has no addresses in CR status - skipping (possible CR population race)", node.NodeName)
			continue
		}
		pmNodes[node.NodeName] = node.Addresses[0].Address
	}

	return pmNodes, nil
}

// getNodeNames extracts node names from a list of nodes.
func getNodeNames(nodes []*corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}
