package ceohelpers

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
)

// CanonicalAddress wraps a NodeAddress and provides canonical IP access.
// This ensures consistent IP comparisons throughout the codebase by
// automatically handling IPv6 compression and IPv4-mapped IPv6 conversion.
type CanonicalAddress struct {
	Type             corev1.NodeAddressType
	Address          string // Original address as stored in the resource
	CanonicalAddress string // Canonicalized form for comparisons
}

// NewCanonicalAddress creates a CanonicalAddress from a NodeAddress
func NewCanonicalAddress(addr corev1.NodeAddress) CanonicalAddress {
	return CanonicalAddress{
		Type:             addr.Type,
		Address:          addr.Address,
		CanonicalAddress: dnshelpers.CanonicalizeIP(addr.Address),
	}
}

// GetCanonicalInternalIPs extracts and canonicalizes all internal IP addresses from a Node.
// Returns a slice of canonical IP strings for easy comparison.
func GetCanonicalInternalIPs(node *corev1.Node) []string {
	var ips []string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ips = append(ips, dnshelpers.CanonicalizeIP(addr.Address))
		}
	}
	return ips
}

// GetCanonicalInternalIPsFromMachine extracts and canonicalizes all internal IP addresses from a Machine.
// Returns a slice of canonical IP strings for easy comparison.
func GetCanonicalInternalIPsFromMachine(addresses []corev1.NodeAddress) []string {
	var ips []string
	for _, addr := range addresses {
		if addr.Type == corev1.NodeInternalIP {
			ips = append(ips, dnshelpers.CanonicalizeIP(addr.Address))
		}
	}
	return ips
}

// GetCanonicalAddresses converts a slice of NodeAddresses to CanonicalAddresses.
// Useful when you need to preserve both the original and canonical forms.
func GetCanonicalAddresses(addresses []corev1.NodeAddress) []CanonicalAddress {
	canonical := make([]CanonicalAddress, len(addresses))
	for i, addr := range addresses {
		canonical[i] = NewCanonicalAddress(addr)
	}
	return canonical
}

// HasCanonicalInternalIP checks if a node has a specific canonical internal IP.
// Both the node's addresses and the provided IP are canonicalized before comparison.
func HasCanonicalInternalIP(node *corev1.Node, ip string) bool {
	canonicalIP := dnshelpers.CanonicalizeIP(ip)
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			if dnshelpers.CanonicalizeIP(addr.Address) == canonicalIP {
				return true
			}
		}
	}
	return false
}

// HasCanonicalInternalIPInMachine checks if a machine has a specific canonical internal IP.
// Both the machine's addresses and the provided IP are canonicalized before comparison.
func HasCanonicalInternalIPInMachine(addresses []corev1.NodeAddress, ip string) bool {
	canonicalIP := dnshelpers.CanonicalizeIP(ip)
	for _, addr := range addresses {
		if addr.Type == corev1.NodeInternalIP {
			if dnshelpers.CanonicalizeIP(addr.Address) == canonicalIP {
				return true
			}
		}
	}
	return false
}

// IPsEqual compares two IP addresses in canonical form.
// Handles IPv6 compression and IPv4-mapped IPv6 conversion automatically.
func IPsEqual(ip1, ip2 string) bool {
	return dnshelpers.CanonicalizeIP(ip1) == dnshelpers.CanonicalizeIP(ip2)
}
