package tools

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	ControlPlaneNodeLabelSelector = "node-role.kubernetes.io/control-plane"
)

// IsNodeReady checks if a node is in Ready state.
// Returns true only if the node has a Ready condition with status True.
func IsNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// GetNodeIPForPacemaker returns the internal ip address of the node for use
// in pacemaker configuration.
// If no internal ip is found, it returns the first ip address as a fallback.
func GetNodeIPForPacemaker(node corev1.Node) (string, error) {
	addresses := node.Status.Addresses
	if len(addresses) == 0 {
		return "", fmt.Errorf("node %q has no configured address", node.Name)
	}

	for _, addr := range addresses {
		switch addr.Type {
		case corev1.NodeInternalIP:
			ip := net.ParseIP(addr.Address)
			if ip != nil {
				return ip.String(), nil
			}
		}
	}

	return addresses[0].Address, nil
}

func GetNodeNames(nodes []*corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

// StringSlicesEqual checks if two string slices are equal (same order).
func StringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ListNodesFromInformer returns all nodes from the informer.
// Returns only nodes matching the informer's filter (e.g., controlPlaneNodeInformer).
func ListNodesFromInformer(informer cache.SharedIndexInformer) ([]*corev1.Node, error) {
	if informer == nil {
		return nil, fmt.Errorf("informer is nil")
	}

	lister := corev1listers.NewNodeLister(informer.GetIndexer())
	return lister.List(labels.Everything())
}
