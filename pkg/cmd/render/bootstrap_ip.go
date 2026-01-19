package render

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

var defaultBootstrapIPLocator BootstrapIPLocator = NetlinkBootstrapIPLocator()

// NetlinkBootstrapIPLocator the routable bootstrap node IP using the native
// netlink library.
func NetlinkBootstrapIPLocator() *bootstrapIPLocator {
	return &bootstrapIPLocator{
		getIPAddresses: ipAddrs,
		getAddrMap:     getAddrMap,
		getRouteMap:    getRouteMap,
	}
}

// BootstrapIPLocator tries to find the bootstrap IP for the machine. It should
// go through the effort of identifying the IP based on its inclusion in the machine
// network CIDR, routability, etc. and fall back to using the first listed IP
// as a last resort (and for compatibility with old behavior).
type BootstrapIPLocator interface {
	getBootstrapIP(ipv6 bool, machineCIDR string, excludedIPs []string) (net.IP, error)
}

type bootstrapIPLocator struct {
	getIPAddresses func() ([]net.IP, error)
	getAddrMap     func() (addrMap addrMap, err error)
	getRouteMap    func() (routeMap routeMap, err error)
}

func (l *bootstrapIPLocator) getBootstrapIP(ipv6 bool, machineCIDR string, excludedIPs []string) (net.IP, error) {
	ips, err := l.getIPAddresses()
	if err != nil {
		return nil, err
	}
	addrMap, err := l.getAddrMap()
	if err != nil {
		return nil, err
	}
	routeMap, err := l.getRouteMap()
	if err != nil {
		return nil, err
	}

	addressFilter := AddressFilters(
		NonDeprecatedAddress,
		ContainedByCIDR(machineCIDR),
		AddressNotIn(excludedIPs...),
	)
	discoveredAddresses, err := routableAddresses(addrMap, routeMap, ips, addressFilter, NonDefaultRoute)
	if err != nil {
		return nil, err
	}
	if len(discoveredAddresses) > 1 {
		klog.Warningf("found multiple candidate bootstrap IPs; only the first one will be considered: %+v", discoveredAddresses)
	}

	findIP := func(addresses []net.IP) (net.IP, bool) {
		for _, ip := range addresses {
			// IPv6
			if ipv6 && ip.To4() == nil {
				return ip, true
			}
			// IPv4
			if !ipv6 && ip.To4() != nil {
				return ip, true
			}
		}
		return nil, false
	}

	var bootstrapIP net.IP
	if ip, found := findIP(discoveredAddresses); found {
		bootstrapIP = ip
	} else {
		klog.Warningf("couldn't detect the bootstrap IP automatically, falling back to the first listed address")
		if ip, found := findIP(ips); found {
			bootstrapIP = ip
		}
	}

	if bootstrapIP == nil {
		return nil, fmt.Errorf("couldn't find a suitable bootstrap node IP from candidates\nall: %+v\ndiscovered: %+v", ips, discoveredAddresses)
	}

	return bootstrapIP, nil
}

// AddressFilter is a function type to filter addresses
type AddressFilter func(netlink.Addr) bool

// RouteFilter is a function type to filter routes
type RouteFilter func(netlink.Route) bool

// NonDeprecatedAddress returns true if the address is IPv6 and has a preferred lifetime of 0
func NonDeprecatedAddress(addr netlink.Addr) bool {
	return !(addr.IP.To4() == nil && addr.PreferedLft == 0)
}

// NonDefaultRoute returns whether the passed Route is the default
func NonDefaultRoute(route netlink.Route) bool {
	return route.Dst != nil
}

func ContainedByCIDR(cidr string) AddressFilter {
	return func(addr netlink.Addr) bool {
		_, parsedNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		return parsedNet.Contains(addr.IP)
	}
}

func AddressNotIn(ips ...string) AddressFilter {
	return func(addr netlink.Addr) bool {
		for _, ip := range ips {
			if addr.IP.String() == ip {
				return false
			}
		}
		return true
	}
}

func AddressFilters(filters ...AddressFilter) AddressFilter {
	return func(addr netlink.Addr) bool {
		for _, include := range filters {
			if !include(addr) {
				return false
			}
		}
		return true
	}
}

type addrMap map[netlink.Link][]netlink.Addr
type routeMap map[int][]netlink.Route

// routableAddresses takes a slice of Virtual IPs and returns a slice of
// configured addresses in the current network namespace that directly route to
// those vips. You can optionally pass an AddressFilter and/or RouteFilter to
// further filter down which addresses are considered.
//
// This is ported from https://github.com/openshift/baremetal-runtimecfg/blob/master/pkg/utils/utils.go.
func routableAddresses(addrMap addrMap, routeMap routeMap, vips []net.IP, af AddressFilter, rf RouteFilter) ([]net.IP, error) {
	matches := map[string]net.IP{}
	for link, addresses := range addrMap {
		for _, address := range addresses {
			maskPrefix, maskBits := address.Mask.Size()
			if !af(address) {
				klog.Infof("Filtered address %+v", address)
				continue
			}
			if address.IP.To4() == nil && maskPrefix == maskBits {
				routes, ok := routeMap[link.Attrs().Index]
				if !ok {
					continue
				}
				for _, route := range routes {
					if !rf(route) {
						klog.Infof("Filtered route %+v for address %+v", route, address)
						continue
					}
					routePrefix, _ := route.Dst.Mask.Size()
					klog.Infof("Checking route %+v (mask %s) for address %+v", route, route.Dst.Mask, address)
					if routePrefix == 0 {
						continue
					}
					containmentNet := net.IPNet{IP: address.IP, Mask: route.Dst.Mask}
					for _, vip := range vips {
						klog.Infof("Checking whether address %s with route %s contains VIP %s", address, route, vip)
						if containmentNet.Contains(vip) {
							klog.Infof("Address %s with route %s contains VIP %s", address, route, vip)
							matches[address.IP.String()] = address.IP
						}
					}
				}
			} else {
				for _, vip := range vips {
					klog.Infof("Checking whether address %s contains VIP %s", address, vip)
					if address.Contains(vip) {
						klog.Infof("Address %s contains VIP %s", address, vip)
						matches[address.IP.String()] = address.IP
					}
				}
			}
		}
	}
	ips := []net.IP{}
	for _, ip := range matches {
		ips = append(ips, ip)
	}
	klog.Infof("Found routable IPs %+v", ips)
	return ips, nil
}

func ipAddrs() ([]net.IP, error) {
	ips := []net.IP{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips, err
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		if !ip.IsGlobalUnicast() {
			continue // we only want global unicast address
		}
		ips = append(ips, ip)
	}
	return ips, nil
}
