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
	scaling, err := GetScalingAnnotation(configMap)
	if err != nil {
		return "", err
	}
	if scaling == nil {
		return "", err
	}
	return scaling.Metadata.Name, nil
}

func GetScalingAnnotation(configMap *corev1.ConfigMap) (*ceoapi.EtcdScaling, error) {
	scaling := &ceoapi.EtcdScaling{}
	data, ok := configMap.Annotations[EtcdScalingAnnotationKey]
	if !ok {
		return nil, nil
	}
	if data == "" {
		return nil, nil
	}
	err := json.Unmarshal([]byte(data), scaling)
	if err != nil {
		klog.Infof("unable to unmarshal scaling data %#v\n", err)
		return nil, err
	}
	return scaling, nil
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
