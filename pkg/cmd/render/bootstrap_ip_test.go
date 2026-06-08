//go:build linux

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

func TestAddressNotIn(t *testing.T) {
	scenarios := []struct {
		name        string
		excludedIPs []string
		testAddr    string
		expected    bool
	}{
		{
			name:        "address not in empty exclusion list",
			excludedIPs: []string{},
			testAddr:    "192.0.2.1",
			expected:    true,
		},
		{
			name:        "address not in exclusion list",
			excludedIPs: []string{"192.0.2.2", "192.0.2.3"},
			testAddr:    "192.0.2.1",
			expected:    true,
		},
		{
			name:        "address in exclusion list",
			excludedIPs: []string{"192.0.2.1", "192.0.2.2"},
			testAddr:    "192.0.2.1",
			expected:    false,
		},
		{
			name:        "IPv4 canonicalization - leading zeros",
			excludedIPs: []string{"192.000.002.001"},
			testAddr:    "192.0.2.1",
			expected:    false,
		},
		{
			name:        "IPv6 address not in exclusion list",
			excludedIPs: []string{"2001:db8::2", "2001:db8::3"},
			testAddr:    "2001:db8::1",
			expected:    true,
		},
		{
			name:        "IPv6 address in exclusion list",
			excludedIPs: []string{"2001:db8::1", "2001:db8::2"},
			testAddr:    "2001:db8::1",
			expected:    false,
		},
		{
			name:        "IPv6 canonicalization - expanded form",
			excludedIPs: []string{"2001:0db8:0000:0000:0000:0000:0000:0001"},
			testAddr:    "2001:db8::1",
			expected:    false,
		},
		{
			name:        "IPv6 canonicalization - compressed form matches expanded",
			excludedIPs: []string{"2001:db8::1"},
			testAddr:    "2001:0db8:0000:0000:0000:0000:0000:0001",
			expected:    false,
		},
		{
			name:        "invalid IP in exclusion list is ignored",
			excludedIPs: []string{"not-an-ip", "192.0.2.2"},
			testAddr:    "192.0.2.1",
			expected:    true,
		},
		{
			name:        "multiple exclusions with mixed valid and invalid",
			excludedIPs: []string{"invalid", "192.0.2.1", "also-invalid"},
			testAddr:    "192.0.2.1",
			expected:    false,
		},
		{
			name:        "IPv4-mapped IPv6 address",
			excludedIPs: []string{"::ffff:192.0.2.1"},
			testAddr:    "::ffff:192.0.2.1",
			expected:    false,
		},
		{
			name:        "single IP exclusion",
			excludedIPs: []string{"10.0.0.1"},
			testAddr:    "10.0.0.1",
			expected:    false,
		},
		{
			name:        "many exclusions, address not present",
			excludedIPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"},
			testAddr:    "10.0.0.100",
			expected:    true,
		},
		{
			name:        "many exclusions, address present at end",
			excludedIPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.100"},
			testAddr:    "10.0.0.100",
			expected:    false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			filter := AddressNotIn(scenario.excludedIPs...)
			testNetlinkAddr := netlink.Addr{
				IPNet: netlink.NewIPNet(net.ParseIP(scenario.testAddr)),
			}
			result := filter(testNetlinkAddr)
			if result != scenario.expected {
				t.Errorf("expected %v, got %v for address %s with exclusions %v",
					scenario.expected, result, scenario.testAddr, scenario.excludedIPs)
			}
		})
	}
}

func TestNormalizeIPv4(t *testing.T) {
	scenarios := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "IPv4 with leading zeros",
			input:    "192.000.002.001",
			expected: "192.0.2.1",
		},
		{
			name:     "IPv4 without leading zeros",
			input:    "192.0.2.1",
			expected: "192.0.2.1",
		},
		{
			name:     "IPv4 with mixed leading zeros",
			input:    "010.000.002.100",
			expected: "10.0.2.100",
		},
		{
			name:     "IPv4 all zeros",
			input:    "000.000.000.000",
			expected: "0.0.0.0",
		},
		{
			name:     "IPv4 with all leading zeros in last octet",
			input:    "192.168.1.001",
			expected: "192.168.1.1",
		},
		{
			name:     "IPv6 address - should return as-is",
			input:    "2001:db8::1",
			expected: "2001:db8::1",
		},
		{
			name:     "IPv6 with leading zeros - should return as-is",
			input:    "2001:0db8:0000:0000:0000:0000:0000:0001",
			expected: "2001:0db8:0000:0000:0000:0000:0000:0001",
		},
		{
			name:     "invalid IP - should return as-is",
			input:    "not-an-ip",
			expected: "not-an-ip",
		},
		{
			name:     "empty string - should return as-is",
			input:    "",
			expected: "",
		},
		{
			name:     "IPv4 with too few octets - should return as-is",
			input:    "192.168.1",
			expected: "192.168.1",
		},
		{
			name:     "IPv4 with too many octets - should return as-is",
			input:    "192.168.1.1.1",
			expected: "192.168.1.1.1",
		},
		{
			name:     "IPv4 with invalid characters - should return as-is",
			input:    "192.168.1.a",
			expected: "192.168.1.a",
		},
		{
			name:     "IPv4 with out-of-range octet after normalization - should return as-is",
			input:    "192.168.1.999",
			expected: "192.168.1.999",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := normalizeIPv4(scenario.input)
			if result != scenario.expected {
				t.Errorf("normalizeIPv4(%q) = %q, expected %q", scenario.input, result, scenario.expected)
			}
		})
	}
}
