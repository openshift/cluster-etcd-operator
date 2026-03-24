package render

import (
	"fmt"
	"net"
	"slices"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

var defaultBootstrapIPLocator BootstrapIPLocator = NetlinkBootstrapIPLocator()

// NetlinkBootstrapIPLocator the routable bootstrap node IP using the native
// netlink library.
func NetlinkBootstrapIPLocator() *bootstrapIPLocator {
	return &bootstrapIPLocator{
		getIPAddresses: ipAddrs,
		getAddrSlice:   getAddrSlice,
		getRouteSlice:  getRouteSlice,
	}
}

// BootstrapIPLocator tries to find the bootstrap IP for the machine. It should
// go through the effort of identifying the IP based on its inclusion in the machine
// network CIDR, routability, etc. and fall back to using the first listed IP
// as a last resort (and for compatibility with old behavior).
type BootstrapIPLocator interface {
	getBootstrapIP(ipv6 bool, machineCIDR string, excludedIPs []string) (net.IP, error)
}

// LinkAddresses represents a link and its associated addresses, preserving order
type LinkAddresses struct {
	Link      netlink.Link
	Addresses []netlink.Addr
}

// LinkRoutes represents a link index and its associated routes, preserving order
type LinkRoutes struct {
	LinkIndex int
	Routes    []netlink.Route
}

// addrSlice is a slice of LinkAddresses that preserves ordering
type addrSlice []LinkAddresses

// routeSlice is a slice of LinkRoutes that preserves ordering
type routeSlice []LinkRoutes

type bootstrapIPLocator struct {
	getIPAddresses func() ([]net.IP, error)
	getAddrSlice   func() (addrSlice addrSlice, err error)
	getRouteSlice  func() (routeSlice routeSlice, err error)
}

func (l *bootstrapIPLocator) getBootstrapIP(ipv6 bool, machineCIDR string, excludedIPs []string) (net.IP, error) {
	ips, err := l.getIPAddresses()
	if err != nil {
		return nil, err
	}
	addrSlice, err := l.getAddrSlice()
	if err != nil {
		return nil, err
	}
	routeSlice, err := l.getRouteSlice()
	if err != nil {
		return nil, err
	}

	addressFilter := AddressFilters(
		NonDeprecatedAddress,
		ContainedByCIDR(machineCIDR),
		AddressNotIn(excludedIPs...),
	)
	discoveredAddresses, err := routableAddresses(addrSlice, routeSlice, ips, addressFilter, NonDefaultRoute)
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

// normalizeIPv4 removes leading zeros from IPv4 addresses.
// For example, "192.000.002.001" becomes "192.0.2.1".
// Returns the normalized string if it's a valid IPv4, otherwise returns the original.
func normalizeIPv4(ip string) string {
	// Try parsing as-is first
	if parsed := net.ParseIP(ip); parsed != nil {
		return ip
	}

	// Check if it looks like an IPv4 with potential leading zeros
	parts := make([]string, 0, 4)
	octet := ""
	dotCount := 0

	for _, ch := range ip {
		if ch == '.' {
			if octet == "" {
				return ip // Invalid: empty octet
			}
			// Remove leading zeros from octet
			normalized := octet
			for len(normalized) > 1 && normalized[0] == '0' {
				normalized = normalized[1:]
			}
			parts = append(parts, normalized)
			octet = ""
			dotCount++
		} else if ch >= '0' && ch <= '9' {
			octet += string(ch)
		} else {
			return ip // Invalid character
		}
	}

	// Add the last octet
	if octet == "" || dotCount != 3 {
		return ip // Invalid format
	}
	normalized := octet
	for len(normalized) > 1 && normalized[0] == '0' {
		normalized = normalized[1:]
	}
	parts = append(parts, normalized)

	result := fmt.Sprintf("%s.%s.%s.%s", parts[0], parts[1], parts[2], parts[3])

	// Verify it's valid
	if net.ParseIP(result) == nil {
		return ip // Return original if normalization failed
	}

	return result
}

func AddressNotIn(ips ...string) AddressFilter {
	return func(addr netlink.Addr) bool {
		canonicalAddr := addr.IP.String()
		for _, ip := range ips {
			normalizedIP := normalizeIPv4(ip)
			parsedIP := net.ParseIP(normalizedIP)
			if parsedIP != nil && canonicalAddr == parsedIP.String() {
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

// routableAddresses takes a slice of Virtual IPs and returns a slice of
// configured addresses in the current network namespace that directly route to
// those vips. You can optionally pass an AddressFilter and/or RouteFilter to
// further filter down which addresses are considered.
//
// This is ported from https://github.com/openshift/baremetal-runtimecfg/blob/master/pkg/utils/utils.go and augmented by AI.
func routableAddresses(addrSlice addrSlice, routeSlice routeSlice, vips []net.IP, af AddressFilter, rf RouteFilter) ([]net.IP, error) {
	var matches []net.IP
	seen := map[string]struct{}{}
	for _, linkAddresses := range addrSlice {
		for _, address := range linkAddresses.Addresses {
			maskPrefix, maskBits := address.Mask.Size()
			if !af(address) {
				klog.Infof("Filtered address %+v", address)
				continue
			}
			if address.IP.To4() == nil && maskPrefix == maskBits {
				routesIdx := slices.IndexFunc(routeSlice, func(routes LinkRoutes) bool {
					return routes.LinkIndex == linkAddresses.Link.Attrs().Index
				})
				if routesIdx == -1 {
					continue
				}

				for _, route := range routeSlice[routesIdx].Routes {
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
							ipStr := address.IP.String()
							if _, found := seen[ipStr]; !found {
								seen[ipStr] = struct{}{}
								matches = append(matches, address.IP)
							}
						}
					}
				}
			} else {
				for _, vip := range vips {
					klog.Infof("Checking whether address %s contains VIP %s", address, vip)
					if address.Contains(vip) {
						klog.Infof("Address %s contains VIP %s", address, vip)
						ipStr := address.IP.String()
						if _, found := seen[ipStr]; !found {
							seen[ipStr] = struct{}{}
							matches = append(matches, address.IP)
						}
					}
				}
			}
		}
	}
	klog.Infof("Found routable IPs %+v", matches)
	return matches, nil
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
