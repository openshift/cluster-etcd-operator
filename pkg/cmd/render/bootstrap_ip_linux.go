//go:build linux

package render

import (
	"slices"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

func getAddrSlice() (addrSlice addrSlice, err error) {
	nlHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer nlHandle.Delete()

	links, err := nlHandle.LinkList()
	if err != nil {
		return nil, err
	}

	addrSlice = make([]LinkAddresses, 0, len(links))
	for _, link := range links {
		addresses, err := nlHandle.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, err
		}

		for _, address := range addresses {
			linkIndex := link.Attrs().Index
			existingIndex := slices.IndexFunc(addrSlice, func(la LinkAddresses) bool {
				return la.Link.Attrs().Index == linkIndex
			})

			if existingIndex != -1 {
				addrSlice[existingIndex].Addresses = append(addrSlice[existingIndex].Addresses, address)
			} else {
				addrSlice = append(addrSlice, LinkAddresses{
					Link:      link,
					Addresses: []netlink.Addr{address},
				})
			}
		}
	}
	klog.Infof("retrieved Address slice %+v", addrSlice)
	return addrSlice, nil
}

func getRouteSlice() (routeSlice routeSlice, err error) {
	nlHandle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer nlHandle.Delete()

	routes, err := nlHandle.RouteList(nil, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}

	// Group routes by link index while preserving order
	linkIndexMap := make(map[int]int) // map from linkIndex to slice index
	for _, route := range routes {
		if route.Protocol != unix.RTPROT_RA {
			klog.Infof("Ignoring route non Router advertisement route %+v", route)
			continue
		}

		sliceIndex, exists := linkIndexMap[route.LinkIndex]
		if exists {
			// Link index already exists, append to existing routes
			routeSlice[sliceIndex].Routes = append(routeSlice[sliceIndex].Routes, route)
		} else {
			// New link index, create new entry
			linkIndexMap[route.LinkIndex] = len(routeSlice)
			routeSlice = append(routeSlice, LinkRoutes{
				LinkIndex: route.LinkIndex,
				Routes:    []netlink.Route{route},
			})
		}
	}

	klog.Infof("Retrieved route slice %+v", routeSlice)

	return routeSlice, nil
}
