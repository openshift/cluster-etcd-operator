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
	nodes, _, err := c.getPacemakerNodesWithCR()
	return nodes, err
}

// getPacemakerNodesWithCR returns both the node map and the full CR from the PacemakerCluster CR.
// Returns (nodes, CR, error). Useful when caller needs to check CR metadata like LastUpdated.
func (c *PacemakerLifecycleManager) getPacemakerNodesWithCR() (map[string]string, *pacmkrv1.PacemakerCluster, error) {
	// Check if informer exists
	if c.pacemakerInformer == nil {
		return nil, nil, fmt.Errorf("pacemakerInformer is nil")
	}

	// Get PacemakerCluster CR from informer cache
	// Note: We don't check HasSynced() here because the watch stream sometimes fails with decode errors
	// even though the cache is populated via List. The cache will be refreshed on resync interval.
	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(pacemaker.PacemakerClusterResourceName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get PacemakerCluster from cache: %w", err)
	}
	if !exists {
		return nil, nil, fmt.Errorf("PacemakerCluster CR not found")
	}

	pacemakerCR, ok := item.(*pacmkrv1.PacemakerCluster)
	if !ok {
		return nil, nil, fmt.Errorf("failed to convert to PacemakerCluster")
	}

	if pacemakerCR.Status.Nodes == nil {
		return nil, pacemakerCR, fmt.Errorf("PacemakerCluster CR has no nodes in status")
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

	return pmNodes, pacemakerCR, nil
}
