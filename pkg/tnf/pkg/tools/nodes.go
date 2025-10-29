package tools

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
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

	// find internal ip
	for _, addr := range addresses {
		switch addr.Type {
		case corev1.NodeInternalIP:
			ip := net.ParseIP(addr.Address)
			if ip != nil {
				return ip.String(), nil
			}
		}
	}

	// fallback
	return addresses[0].Address, nil
}
