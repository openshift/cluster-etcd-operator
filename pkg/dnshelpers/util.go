package dnshelpers

import (
	"fmt"
	"net"
	"net/url"

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
					return currAddress.Address, ipFamily, nil
				}
			case "tcp6":
				isIPv4, err := IsIPv4(currAddress.Address)
				if err != nil {
					return "", "", err
				}
				if !isIPv4 {
					return currAddress.Address, ipFamily, nil
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

func GetInternalIPAddressesForNodeName(node *corev1.Node) ([]string, error) {
	addresses := []string{}
	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalIP {
			addresses = append(addresses, currAddress.Address)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("node/%s missing %s", node.Name, corev1.NodeInternalIP)
	}

	return addresses, nil
}

// GetIPFromAddress takes a client or peer address and returns the IP address (unescaped if IPv6).
func GetIPFromAddress(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("failed to parse address: %s: %w", address, err)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", fmt.Errorf("failed to split host port: %s: %w", u.Host, err)
	}

	return host, nil
}
