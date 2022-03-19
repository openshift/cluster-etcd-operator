package ceohelpers

import (
	"fmt"

	"github.com/ghodss/yaml"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// ReadDesiredControlPlaneReplicasCount simply reds the desired Control Plane replica count from the cluster-config-v1 configmap in the kube-system namespace
func ReadDesiredControlPlaneReplicasCount(configMapListerForKubeSystemNamespace corev1listers.ConfigMapNamespaceLister) (int, error) {
	const clusterConfigConfigMapName = "cluster-config-v1"
	const installConfigKeyName = "install-config"

	clusterConfig, err := configMapListerForKubeSystemNamespace.Get(clusterConfigConfigMapName)
	if err != nil {
		return 0, err
	}

	rawInstallConfig, hasInstallConfig := clusterConfig.Data[installConfigKeyName]
	if !hasInstallConfig {
		return 0, fmt.Errorf("missing required key: %s for cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}

	var unstructuredInstallConfig map[string]interface{}
	if err := yaml.Unmarshal([]byte(rawInstallConfig), &unstructuredInstallConfig); err != nil {
		return 0, fmt.Errorf("failed to unmarshal key: %s to yaml from cm: %s/kube-system, err: %w", installConfigKeyName, clusterConfigConfigMapName, err)
	}

	unstructuredControlPlaneConfig, exists, err := unstructured.NestedMap(unstructuredInstallConfig, "controlPlane")
	if err != nil {
		return 0, fmt.Errorf("failed to extract field: %s.controlPlane from cm: %s/kube-system, err: %v", installConfigKeyName, clusterConfigConfigMapName, err)
	}
	if !exists {
		return 0, fmt.Errorf("required field: %s.controlPlane doesn't exist in cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}
	// unmarshalling JSON into an interface value always stores JSON number as a float64
	desiredReplicas, exists, err := unstructured.NestedFloat64(unstructuredControlPlaneConfig, "replicas")
	if err != nil {
		return 0, fmt.Errorf("failed to extract field: %s.controlPlane.replicas from cm: %s/kube-system, err: %v", installConfigKeyName, clusterConfigConfigMapName, err)
	}
	if !exists {
		return 0, fmt.Errorf("required field: %s.controlPlane.replicas doesn't exist in cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}

	return int(desiredReplicas), nil
}
