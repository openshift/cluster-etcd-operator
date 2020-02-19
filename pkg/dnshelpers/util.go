package dnshelpers

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	corev1 "k8s.io/api/core/v1"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"k8s.io/klog"
)

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
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
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
