package render

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestBootstrapIPLocator(t *testing.T) {
	scenarios := []struct {
		name string

		ips         []net.IP
		addrSlice   addrSlice
		routeSlice  routeSlice
		ipv6        bool
		machineCIDR string
		exclude     []string

		expect net.IP
	}{
		{
			name:        "simple",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"192.168.125.1",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "192.168.125.1"),
			},
			routeSlice: []LinkRoutes{},
			exclude:    []string{},
			expect:     net.ParseIP("192.168.125.1"),
		},
		{
			name:        "deep location with exclusion",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"192.168.124.100",
				"192.168.125.112",
				"192.168.125.5",
				"10.88.0.1",
				"172.17.0.1",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "192.168.124.100"),
				withDevice(1, "192.168.125.112", "192.168.125.5"),
				withDevice(2, "10.88.0.1"),
				withDevice(3, "172.17.0.1"),
			},
			routeSlice: []LinkRoutes{},
			exclude:    []string{"192.168.125.5"},
			expect:     net.ParseIP("192.168.125.112"),
		},
		{
			name:        "fallback to first IP",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"10.88.0.1",
				"172.17.0.1",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "10.88.0.1"),
				withDevice(1, "172.17.0.1"),
			},
			routeSlice: []LinkRoutes{},
			exclude:    []string{},
			expect:     net.ParseIP("10.88.0.1"),
		},
		{
			name:        "ordering preservation - first matching IP should be selected",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"192.168.125.10",
				"192.168.125.20",
				"192.168.125.30",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "192.168.125.30"),
				withDevice(1, "192.168.125.10"),
				withDevice(2, "192.168.125.20"),
			},
			routeSlice: []LinkRoutes{},
			exclude:    []string{},
			expect:     net.ParseIP("192.168.125.30"), // Should select first in slice order, not IP order
		},
		{
			name:        "duplicate link handling - addresses should be accumulated",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"192.168.125.100",
				"192.168.125.101",
				"192.168.125.102",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "192.168.125.100", "192.168.125.101", "192.168.125.102"),
			},
			routeSlice: []LinkRoutes{},
			exclude:    []string{},
			expect:     net.ParseIP("192.168.125.100"), // Should get first address from accumulated list
		},
		{
			name:        "IPv6 with routes",
			machineCIDR: "2001:db8::/32",
			ipv6:        true,
			ips: newIPs(
				"2001:db8::1",
				"2001:db8::2",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "2001:db8::1"),
				withDevice(1, "2001:db8::2"),
			},
			routeSlice: []LinkRoutes{
				withRoute(0, "2001:db8::/64", unix.RTPROT_RA),
			},
			exclude: []string{},
			expect:  net.ParseIP("2001:db8::1"),
		},
		{
			name:        "complex scenario - ordering, duplicates, and routes",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"192.168.125.10",
				"192.168.125.20",
				"192.168.125.30",
				"10.0.0.1",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "192.168.125.20", "192.168.125.30"), // Device 1 gets first IP
				withDevice(1, "192.168.125.10"),                   // Device 2 gets second IP
				withDevice(2, "10.0.0.1"),                         // Device 3 gets IP outside CIDR
			},
			routeSlice: []LinkRoutes{
				withRoute(0, "192.168.125.0/24", unix.RTPROT_RA),
			},
			exclude: []string{"192.168.125.20"},    // Exclude first IP from device 0
			expect:  net.ParseIP("192.168.125.30"), // Should select device 0's second IP (first device in slice order)
		},
		{
			name:        "multiple exclusions preserve order",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"192.168.125.1",
				"192.168.125.2",
				"192.168.125.3",
				"192.168.125.4",
				"192.168.125.5",
			),
			addrSlice: []LinkAddresses{
				withDevice(0, "192.168.125.1"),
				withDevice(1, "192.168.125.2"),
				withDevice(2, "192.168.125.3"),
				withDevice(3, "192.168.125.4"),
				withDevice(4, "192.168.125.5"),
			},
			routeSlice: []LinkRoutes{},
			exclude:    []string{"192.168.125.1", "192.168.125.2"},
			expect:     net.ParseIP("192.168.125.3"), // Should select first non-excluded IP
		},
		{
			name:        "empty slice - no matches",
			machineCIDR: "192.168.125.0/24",
			ips: newIPs(
				"10.0.0.1",
			),
			addrSlice:  []LinkAddresses{},
			routeSlice: []LinkRoutes{},
			exclude:    []string{},
			expect:     net.ParseIP("10.0.0.1"), // Should fallback to first IP
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			locator := &bootstrapIPLocator{
				getIPAddresses: func() ([]net.IP, error) { return scenario.ips, nil },
				getAddrSlice:   func() (addrSlice addrSlice, err error) { return scenario.addrSlice, nil },
				getRouteSlice:  func() (routeSlice routeSlice, err error) { return scenario.routeSlice, nil },
			}
			ip, err := locator.getBootstrapIP(scenario.ipv6, scenario.machineCIDR, scenario.exclude)
			if err != nil {
				t.Fatal(err)
			}
			if e, a := scenario.expect.String(), ip.String(); e != a {
				t.Fatalf("expected ip %s, got %s", e, a)
			}
		})
	}
}

func withDevice(index int, ips ...string) LinkAddresses {
	dev := &netlink.Device{}
	dev.Index = index
	var addrs []netlink.Addr
	for _, ip := range ips {
		addr := netlink.Addr{
			IPNet: netlink.NewIPNet(net.ParseIP(ip)),
		}
		addrs = append(addrs, addr)
	}

	linkAddresses := LinkAddresses{
		Link:      dev,
		Addresses: addrs,
	}
	return linkAddresses
}

func newIPs(ips ...string) []net.IP {
	var list []net.IP
	for _, ip := range ips {
		list = append(list, net.ParseIP(ip))
	}
	return list
}

func withRoute(linkIndex int, dst string, protocol int) LinkRoutes {
	_, ipNet, err := net.ParseCIDR(dst)
	if err != nil {
		panic(err)
	}
	return LinkRoutes{
		LinkIndex: linkIndex,
		Routes: []netlink.Route{
			{
				Dst:      ipNet,
				Protocol: protocol,
			},
		},
	}
}
