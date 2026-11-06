package dnshelpers

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// GetEscapedPreferredInternalIPAddressForNodeName returns the first internal ip address of the correct family with escaping
// for ipv6.
func GetEscapedPreferredInternalIPAddressForNodeName(network *configv1.Network, node *corev1.Node) (string, error) {
	address, family, err := GetPreferredInternalIPAddressForNodeName(network, node)
	if err != nil {
		return "", err
	}
	if family == "tcp6" {
		return "[" + address + "]", nil
	} else {
		return address, nil
	}
}

// GetPreferredInternalIPAddressForNodeName returns the first internal ip address of the correct family and the family
func GetPreferredInternalIPAddressForNodeName(network *configv1.Network, node *corev1.Node) (string, string, error) {
	ipFamily, err := GetPreferredIPFamily(network)
	if err != nil {
		return "", "", err
	}

	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalIP {
			switch ipFamily {
			case "tcp4":
				isIPv4, err := IsIPv4(currAddress.Address)
				if err != nil {
					return "", "", err
				}
				if isIPv4 {
					return TryCanonicalizeIP(currAddress.Address), ipFamily, nil
				}
			case "tcp6":
				isIPv4, err := IsIPv4(currAddress.Address)
				if err != nil {
					return "", "", err
				}
				if !isIPv4 {
					return TryCanonicalizeIP(currAddress.Address), ipFamily, nil
				}
			default:
				return "", "", fmt.Errorf("unexpected ip family: %q", ipFamily)
			}
		}
	}

	return "", "", fmt.Errorf("no matches found for ip family %q for node %q", ipFamily, node.Name)
}

// GetPreferredIPFamily checks network status for service CIDR to conclude IP family. If status is not yet populated
// fallback to spec.
func GetPreferredIPFamily(network *configv1.Network) (string, error) {
	var serviceCIDR string
	switch {
	case len(network.Status.ServiceNetwork) != 0:
		serviceCIDR = network.Status.ServiceNetwork[0]
		if len(serviceCIDR) == 0 {
			return "", fmt.Errorf("networks.%s/cluster: status.serviceNetwork[0] is empty", configv1.GroupName)
		}
		break
	case len(network.Spec.ServiceNetwork) != 0:
		klog.Warningf("networks.%s/cluster: status.serviceNetwork not found falling back to spec.serviceNetwork", configv1.GroupName)
		serviceCIDR = network.Spec.ServiceNetwork[0]
		if len(serviceCIDR) == 0 {
			return "", fmt.Errorf("networks.%s/cluster: spec.serviceNetwork[0] is empty", configv1.GroupName)
		}
		break
	default:
		return "", fmt.Errorf("networks.%s/cluster: status|spec.serviceNetwork not found", configv1.GroupName)
	}

	ip, _, err := net.ParseCIDR(serviceCIDR)

	switch {
	case err != nil:
		return "", err
	case ip.To4() == nil:
		return "tcp6", nil
	default:
		return "tcp4", nil
	}
}

func IsIPv4(ipString string) (bool, error) {
	ip := net.ParseIP(ipString)

	switch {
	case ip == nil:
		return false, fmt.Errorf("not an IP")
	case ip.To4() == nil:
		return false, nil
	default:
		return true, nil
	}
}

// GetInternalIPAddresses returns the internal IP addresses found in addresses, in the
// form they are stored in the resource. Use GetCanonicalInternalIPAddresses when the
// result is compared against another address rather than written out verbatim.
func GetInternalIPAddresses(addresses []corev1.NodeAddress) []string {
	ips := []string{}
	for _, currAddress := range addresses {
		if currAddress.Type == corev1.NodeInternalIP && currAddress.Address != "" {
			ips = append(ips, currAddress.Address)
		}
	}
	return ips
}

// GetCanonicalInternalIPAddresses returns the internal IP addresses found in addresses
// in canonical form, see TryCanonicalizeIP.
func GetCanonicalInternalIPAddresses(addresses []corev1.NodeAddress) []string {
	ips := GetInternalIPAddresses(addresses)
	for i, ip := range ips {
		ips[i] = TryCanonicalizeIP(ip)
	}
	return ips
}

func GetInternalIPAddressesForNodeName(node *corev1.Node) ([]string, error) {
	// Note that these are intentionally not canonicalized: callers such as the serving
	// certificate hostnames need the address exactly as the node reports it.
	addresses := GetInternalIPAddresses(node.Status.Addresses)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("node/%s missing %s", node.Name, corev1.NodeInternalIP)
	}

	return addresses, nil
}

// TryCanonicalizeIP returns the canonical string representation of an IP address.
// This ensures consistent IP comparisons by:
// - Normalizing IPv6 addresses to their compressed form (e.g., "2001:0db8::1" → "2001:db8::1")
// - Converting IPv4-mapped IPv6 addresses to IPv4 (e.g., "::ffff:192.0.2.1" → "192.0.2.1")
// - Stripping leading zeros from IPv4 octets (e.g., "192.000.002.001" → "192.0.2.1")
// The canonicalization is best effort: an input that is not a valid IP address is
// returned unchanged, so callers that have to reject one should check the result
// with net.ParseIP.
func TryCanonicalizeIP(ip string) string {
	if parsed := net.ParseIP(ip); parsed != nil {
		return parsed.String()
	}

	// net.ParseIP rejects IPv4 octets with leading zeros, retry with them stripped.
	normalized, err := normalizeIPv4(ip)
	if err != nil {
		return ip
	}
	if parsed := net.ParseIP(normalized); parsed != nil {
		return parsed.String()
	}
	return ip
}

// normalizeIPv4 removes leading zeros from the octets of a dotted-quad IPv4
// address, for example "192.000.002.001" becomes "192.0.2.1". net.ParseIP
// rejects octets with leading zeros since go1.17, so they have to be stripped
// before the address can be parsed. An error is returned if ip is not a valid
// dotted-quad IPv4 address.
func normalizeIPv4(ip string) (string, error) {
	octets := make([]string, 0, 4)
	for octet := range strings.SplitSeq(ip, ".") {
		// ParseUint with a bit size of 8 rejects both non-numeric octets and
		// octets outside of the 0-255 range.
		parsed, err := strconv.ParseUint(octet, 10, 8)
		if err != nil {
			return "", fmt.Errorf("invalid IPv4 octet %q: %w", octet, err)
		}
		octets = append(octets, strconv.FormatUint(parsed, 10))
	}
	if len(octets) != 4 {
		return "", fmt.Errorf("expected 4 IPv4 octets, got %d", len(octets))
	}

	return strings.Join(octets, "."), nil
}

// GetIPFromAddress takes a client or peer address and returns the canonical IP address (unescaped if IPv6).
// The returned IP is in canonical form for consistent comparisons.
func GetIPFromAddress(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("failed to parse address: %s: %w", address, err)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", fmt.Errorf("failed to split host port: %s: %w", u.Host, err)
	}

	// Addresses are expected to be IP based, but a host that is not an IP is passed
	// through unchanged rather than rejected, as it was before canonicalization.
	return TryCanonicalizeIP(host), nil
}
