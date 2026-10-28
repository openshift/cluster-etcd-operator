package ceohelpers

import (
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
)

// HasCanonicalInternalIP checks if addresses, as reported by a node or a machine,
// contain a specific internal IP. Both the addresses and the provided IP are
// canonicalized before comparison.
func HasCanonicalInternalIP(addresses []corev1.NodeAddress, ip string) bool {
	return slices.Contains(dnshelpers.GetCanonicalInternalIPAddresses(addresses), dnshelpers.TryCanonicalizeIP(ip))
}

// CanonicalIPsEqual compares two IP addresses in canonical form.
// Handles IPv6 compression and IPv4-mapped IPv6 conversion automatically.
func CanonicalIPsEqual(ip1, ip2 string) bool {
	return dnshelpers.TryCanonicalizeIP(ip1) == dnshelpers.TryCanonicalizeIP(ip2)
}
