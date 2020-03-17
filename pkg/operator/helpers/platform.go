package helpers

import (
	"fmt"

	"github.com/ghodss/yaml"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
)

const defaultEtcdSize = 3

// GetExpectedEtcdSize reads configmap kube-system/install-config-v1
// and returns the controlPlane.replicas
// For platforms that don't have the configmap, it will default to 3.
func GetExpectedEtcdSize(kubeSystemConfigmapLister corev1listers.ConfigMapLister) (int, error) {
	cm, err := kubeSystemConfigmapLister.ConfigMaps("kube-system").Get("cluster-config-v1")
	switch {
	case apierrors.IsNotFound(err):
		klog.Info("configmap kube-system/cluster-config-v1 not found, defaulting control plane replicas to 3")
		return defaultEtcdSize, nil
	case err != nil:
		return 0, err
	default:
		// handled below
	}
	yamlData, ok := cm.Data["install-config"]
	if !ok {
		return 0, fmt.Errorf("install-config key does not exist in configmap kube-system/cluster-config-v1: %#v", cm)
	}
	var installConfig map[string]interface{}
	err = yaml.Unmarshal([]byte(yamlData), &installConfig)
	if err != nil {
		return 0, err
	}
	expectedSize, found, err := unstructured.NestedFloat64(installConfig, "controlPlane", "replicas")
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("int64 not found in install-config in configmap kube-system/cluster-config-v1: %#v", cm)
	}
	return int(expectedSize), nil
}
