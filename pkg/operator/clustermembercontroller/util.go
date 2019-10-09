package clustermembercontroller

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

func GetScaleAnnotationName(configMap *corev1.ConfigMap) (string, error) {
	scaling := &ceoapi.EtcdScaling{}
	data, ok := configMap.Annotations[EtcdScalingAnnotationKey]
	if !ok {
		return "", nil
	}
	if data == "" {
		return "", nil
	}
	if err := json.Unmarshal([]byte(data), scaling); err != nil {
		klog.Infof("unable to unmarshal scaling data %#v\n", err)
		return "", err
	}
	return scaling.Metadata.Name, nil
}

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
