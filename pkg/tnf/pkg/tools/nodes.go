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

// GetNodeNames extracts node names from a slice of nodes.
func GetNodeNames(nodes []*corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

// ipAddressesEqual compares two IP address strings for equality.
// Handles both IPv4 and IPv6, normalizing them before comparison.
func ipAddressesEqual(ip1, ip2 string) bool {
	parsedIP1 := net.ParseIP(ip1)
	parsedIP2 := net.ParseIP(ip2)

	// If either fails to parse, fall back to string comparison
	if parsedIP1 == nil || parsedIP2 == nil {
		return ip1 == ip2
	}

	return parsedIP1.Equal(parsedIP2)
}

// DetermineReconciliationActions compares desired and current node membership to determine
// which nodes to add/remove. Desired state is the source of truth (K8s nodes).
// Compares both name AND IP to detect replacements (same name, different IP).
//
// Parameters:
//   - desiredNodes: map of node names to IPs that should be in the cluster (from K8s API or ConfigMap)
//   - currentNodes: map of node names to IPs currently in the cluster (from PacemakerCluster CR or pacemaker directly)
//
// Returns:
//   - nodesToRemove: nodes in current but not in desired, or with IP mismatch
//   - nodesToAdd: nodes in desired but not in current, or with IP mismatch
func DetermineReconciliationActions(desiredNodes, currentNodes map[string]string) (nodesToRemove, nodesToAdd []string) {
	// Remove: current nodes not in desired OR with mismatched IPs
	for nodeName, currentIP := range currentNodes {
		desiredIP, existsInDesired := desiredNodes[nodeName]
		if !existsInDesired {
			// Node deleted from K8s
			nodesToRemove = append(nodesToRemove, nodeName)
		} else if !ipAddressesEqual(desiredIP, currentIP) {
			// Node exists in both but IP changed - remove old, add new
			nodesToRemove = append(nodesToRemove, nodeName)
			nodesToAdd = append(nodesToAdd, nodeName)
		}
	}

	// Add: desired nodes not in current at all
	for nodeName := range desiredNodes {
		if _, exists := currentNodes[nodeName]; !exists {
			nodesToAdd = append(nodesToAdd, nodeName)
		}
	}

	return nodesToRemove, nodesToAdd
}
