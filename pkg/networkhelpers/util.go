package networkhelpers

import (
	"net"
)

// IsNetworkContainIp checks if an IP adress is part of a network cidr
func IsNetworkContainIp(network, ipAddress string) (bool, error) {
	_, parsedNet, err := net.ParseCIDR(network)
	if err != nil {
		return false, err
	}
	if parsedNet.Contains(net.ParseIP(ipAddress)) {
		return true, nil
	}
	return false, nil
}

// IPAddrs returns a slice of string containing all IPv4 and IPv6 globsl addresses
// on all interfaces.
func IpAddrs() ([]string, error) {
	var ips []string
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
		ips = append(ips, ip.String())
	}
	return ips, nil
}
