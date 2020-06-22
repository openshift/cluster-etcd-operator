package render

import (
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog"
)

func getAddrMap() (addrMap addrMap, err error) {
	nlHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer nlHandle.Delete()

	links, err := nlHandle.LinkList()
	if err != nil {
		return nil, err
	}

	addrMap = make(map[netlink.Link][]netlink.Addr)
	for _, link := range links {
		addresses, err := nlHandle.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if _, ok := addrMap[link]; ok {
				addrMap[link] = append(addrMap[link], address)
			} else {
				addrMap[link] = []netlink.Addr{address}
			}
		}
	}
	klog.Infof("retrieved Address map %+v", addrMap)
	return addrMap, nil
}

func getRouteMap() (routeMap routeMap, err error) {
	nlHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer nlHandle.Delete()

	routes, err := nlHandle.RouteList(nil, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}

	routeMap = make(map[int][]netlink.Route)
	for _, route := range routes {
		if route.Protocol != unix.RTPROT_RA {
			klog.Infof("Ignoring route non Router advertisement route %+v", route)
			continue
		}
		if _, ok := routeMap[route.LinkIndex]; ok {
			routeMap[route.LinkIndex] = append(routeMap[route.LinkIndex], route)
		} else {
			routeMap[route.LinkIndex] = []netlink.Route{route}
		}
	}

	klog.Infof("Retrieved route map %+v", routeMap)

	return routeMap, nil
}
