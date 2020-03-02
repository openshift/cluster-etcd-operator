package dnshelpers

import (
	"fmt"
	"net"
	"strings"

	configv1 "github.com/openshift/api/config/v1"

	"k8s.io/apimachinery/pkg/util/sets"

	corev1 "k8s.io/api/core/v1"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/klog"
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

func GetURLHostForIP(ip string) (string, error) {
	isIPV4, err := IsIPv4(ip)
	if err != nil {
		return "", err
	}
	if isIPV4 {
		return ip, nil
	}

	return "[" + ip + "]", nil
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

func GetPreferredIPFamily(network *configv1.Network) (string, error) {
	if len(network.Status.ServiceNetwork) == 0 || len(network.Status.ServiceNetwork[0]) == 0 {
		return "", fmt.Errorf("networks.%s/cluster: status.serviceNetwork not found", configv1.GroupName)
	}

	serviceCIDR := network.Status.ServiceNetwork[0]
	if len(serviceCIDR) == 0 {
		return "", fmt.Errorf("networks.%s/cluster: status.serviceNetwork[0] is empty", configv1.GroupName)
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

func ReverseLookupFirstHit(discoveryDomain string, ips ...string) (string, error) {
	errs := []error{}
	for _, ip := range ips {
		ret, err := reverseLookupForOneIP(discoveryDomain, ip)
		if err == nil {
			return ret, nil
		}
		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return "", fmt.Errorf("something weird happened for %q, %#v", discoveryDomain, ips)
	}
	return "", utilerrors.NewAggregate(errs)
}

func ReverseLookupAllHits(discoveryDomain string, ips ...string) ([]string, error) {
	ret := []string{}
	found := sets.NewString()
	errs := []error{}
	for _, ip := range ips {
		curr, err := reverseLookupForOneIP(discoveryDomain, ip)
		if err != nil {
			errs = append(errs, err)
		} else if !found.Has(curr) {
			ret = append(ret, curr)
			found.Insert(curr)
		}
	}

	switch {
	case len(ret) > 0:
		//ignore errors
		return ret, nil
	case len(errs) == 0:
		// we got no result and no error
		return nil, fmt.Errorf("something weird happened for %q, %#v", discoveryDomain, ips)
	default:
		// we got errors
		return nil, utilerrors.NewAggregate(errs)
	}
}
func reverseLookupForOneIP(discoveryDomain, ipAddress string) (string, error) {
	service := "etcd-server-ssl"
	proto := "tcp"

	_, srvs, err := net.LookupSRV(service, proto, discoveryDomain)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q: %v", srv.Target, err)
		}

		for _, addr := range addrs {
			if addr == ipAddress {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}
