//go:build !linux
// +build !linux

package render

import (
	"fmt"
)

func getAddrMap() (addrMap addrMap, err error) {
	return nil, fmt.Errorf("not implemented")
}

func getRouteMap() (routeMap routeMap, err error) {
	return nil, fmt.Errorf("not implemented")
}
