package clustermembercontroller

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog"
)

func ReverseLookupSelf(service, proto, name, self string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == self {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}
