package render

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestBootstrapIPLocator(t *testing.T) {
	scenarios := []struct {
		name string

		ips         []net.IP
		addrMap     addrMap
		routeMap    routeMap
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
			addrMap: newAddrMap(
				withDevice(0, "192.168.125.1"),
			),
			routeMap: map[int][]netlink.Route{},
			exclude:  []string{},
			expect:   net.ParseIP("192.168.125.1"),
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
			addrMap: newAddrMap(
				withDevice(0, "192.168.124.100"),
				withDevice(1, "192.168.125.112", "192.168.125.5"),
				withDevice(2, "10.88.0.1"),
				withDevice(3, "172.17.0.1"),
			),
			routeMap: map[int][]netlink.Route{},
			exclude:  []string{"192.168.125.5"},
			expect:   net.ParseIP("192.168.125.112"),
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			locator := &bootstrapIPLocator{
				getIPAddresses: func() ([]net.IP, error) { return scenario.ips, nil },
				getAddrMap:     func() (addrMap addrMap, err error) { return scenario.addrMap, nil },
				getRouteMap:    func() (routeMap routeMap, err error) { return scenario.routeMap, nil },
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

func newAddrMap(configs ...func(addrMap)) addrMap {
	m := map[netlink.Link][]netlink.Addr{}
	for _, config := range configs {
		config(m)
	}
	return m
}

func withDevice(index int, ips ...string) func(addrMap) {
	return func(addrMap addrMap) {
		dev := &netlink.Device{}
		dev.Index = index
		addrs := []netlink.Addr{}
		for _, ip := range ips {
			addr := netlink.Addr{
				IPNet: netlink.NewIPNet(net.ParseIP(ip)),
			}
			addrs = append(addrs, addr)
		}
		addrMap[dev] = addrs
	}
}

func newIPs(ips ...string) []net.IP {
	list := []net.IP{}
	for _, ip := range ips {
		list = append(list, net.ParseIP(ip))
	}
	return list
}
