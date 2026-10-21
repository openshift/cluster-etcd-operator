package operator

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// pacemakerCRStalenessThreshold is how long before a PacemakerCluster CR status is considered stale.
	// Status collector runs every minute, so 5 minutes means we've missed ~5 consecutive updates.
	pacemakerCRStalenessThreshold = 5 * time.Minute
)

// getActivePacemakerNodes returns nodes eligible for job scheduling.
// Returns K8s ∩ Pacemaker intersection (ready nodes only) if CR is fresh.
// Falls back to all ready control plane nodes if CR unavailable or stale.
func (c *pacemakerLifecycleManager) getActivePacemakerNodes() ([]*corev1.Node, error) {
	if c.controlPlaneNodeInformer == nil || !c.controlPlaneNodeInformer.HasSynced() {
		return nil, fmt.Errorf("node informer not synced yet")
	}

	k8sNodes, err := tools.ListNodesFromInformer(c.controlPlaneNodeInformer)
	if err != nil {
		return nil, fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	readyNodes := []*corev1.Node{}
	for _, node := range k8sNodes {
		if tools.IsNodeReady(node) {
			readyNodes = append(readyNodes, node)
		}
	}

	if len(readyNodes) == 0 {
		return nil, fmt.Errorf("no ready control plane nodes found")
	}

	pacemakerNodes, pacemakerCR, err := c.getPacemakerNodesWithCR()
	pacemakerNodesAvailable := (err == nil && !isPacemakerCRStale(pacemakerCR))

	if pacemakerNodesAvailable {
		intersection := c.getIntersection(readyNodes, pacemakerNodes)
		if len(intersection) > 0 {
			klog.V(4).Infof("Valid targets (intersection, ready only): %v", tools.GetNodeNames(intersection))
			return intersection, nil
		}
		klog.V(4).Infof("No nodes in intersection (K8s ∩ pacemaker) - using all ready nodes")
	} else {
		if err != nil {
			klog.V(4).Infof("PacemakerCluster CR not available: %v - using all ready nodes", err)
		} else {
			klog.V(4).Infof("PacemakerCluster CR status is stale (last updated %v) - using all ready nodes", pacemakerCR.Status.LastUpdated)
		}
	}

	// Sort for deterministic ordering (consistent with intersection path)
	sort.Slice(readyNodes, func(i, j int) bool {
		return readyNodes[i].Name < readyNodes[j].Name
	})

	klog.V(4).Infof("Valid targets (all ready nodes): %v", tools.GetNodeNames(readyNodes))
	return readyNodes, nil
}

// getPacemakerNodesWithCR retrieves pacemaker node information from the PacemakerCluster CR.
// Returns a map of nodeName -> IP address and the CR itself.
func (c *pacemakerLifecycleManager) getPacemakerNodesWithCR() (map[string]string, *pacmkrv1.PacemakerCluster, error) {
	if c.pacemakerInformer == nil {
		return nil, nil, fmt.Errorf("pacemakerInformer is nil")
	}

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

// getIntersection returns nodes that exist in BOTH K8s and pacemaker.
// Returns nodes sorted by name for deterministic ordering.
func (c *pacemakerLifecycleManager) getIntersection(k8sNodes []*corev1.Node, pacemakerNodes map[string]string) []*corev1.Node {
	intersection := []*corev1.Node{}
	for _, k8sNode := range k8sNodes {
		if _, exists := pacemakerNodes[k8sNode.Name]; exists {
			intersection = append(intersection, k8sNode)
		}
	}
	// Sort by name for deterministic target selection
	sort.Slice(intersection, func(i, j int) bool {
		return intersection[i].Name < intersection[j].Name
	})
	return intersection
}

// isPacemakerCRStale checks if the PacemakerCluster CR status is stale (hasn't been updated recently).
// A stale CR indicates the status collector isn't running or pacemaker isn't responding.
func isPacemakerCRStale(cr *pacmkrv1.PacemakerCluster) bool {
	if cr == nil {
		return true
	}

	timeSinceUpdate := time.Since(cr.Status.LastUpdated.Time)
	return timeSinceUpdate > pacemakerCRStalenessThreshold
}
