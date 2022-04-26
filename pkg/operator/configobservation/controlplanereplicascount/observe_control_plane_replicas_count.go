package controlplanereplicascount

import (
	"fmt"
	"github.com/ghodss/yaml"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

var ControlPlaneReplicasPath = []string{"controlPlane", "replicas"}

// ObserveControlPlaneReplicas sets the current control plane replicas count
func ObserveControlPlaneReplicas(genericListers configobserver.Listers, _ events.Recorder, existingConfig map[string]interface{}) (ret map[string]interface{}, errs []error) {
	defer func() {
		// Prune the observed config so that it only contains controlPlane replicas field.
		ret = configobserver.Pruned(ret, ControlPlaneReplicasPath)
	}()

	// read the observed value
	observedControlPlaneReplicasInt, err := readDesiredControlPlaneReplicas(genericListers.(configobservation.Listers).ConfigMapListerForKubeSystemNamespace)
	if err != nil {
		// we got an error, so we are unable to determine current replicas count
		return existingConfig, append(errs, err)
	}
	// since unmarshalling JSON into an interface value always stores JSON number as a float64 convert the observed value
	observedControlPlaneReplicas := float64(observedControlPlaneReplicasInt)

	// always set the observed value
	observedConfig := map[string]interface{}{}
	if err = unstructured.SetNestedField(observedConfig, observedControlPlaneReplicas, ControlPlaneReplicasPath...); err != nil {
		return existingConfig, append(errs, err)
	}
	return observedConfig, errs
}

// readDesiredControlPlaneReplicas simply reds the desired Control Plane replica count from the cluster-config-v1 configmap in the kube-system namespace
func readDesiredControlPlaneReplicas(configMapListerForKubeSystemNamespace corev1listers.ConfigMapNamespaceLister) (int, error) {
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

	// unmarshalling JSON into an interface value always stores JSON number as a float64
	desiredReplicas, exists, err := unstructured.NestedFloat64(unstructuredInstallConfig, "controlPlane", "replicas")
	if err != nil {
		return 0, fmt.Errorf("failed to extract field: %s.controlPlane.replicas from cm: %s/kube-system, err: %v", installConfigKeyName, clusterConfigConfigMapName, err)
	}
	if !exists {
		return 0, fmt.Errorf("required field: %s.controlPlane.replicas doesn't exist in cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}

	return int(desiredReplicas), nil
}
