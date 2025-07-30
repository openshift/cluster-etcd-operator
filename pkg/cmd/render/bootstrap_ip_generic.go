//go:build !linux
// +build !linux

package render

import (
	"fmt"
)

func getAddrSlice() (addrSlice addrSlice, err error) {
	return nil, fmt.Errorf("not implemented")
}

func getRouteSlice() (routeSlice routeSlice, err error) {
	return nil, fmt.Errorf("not implemented")
}
