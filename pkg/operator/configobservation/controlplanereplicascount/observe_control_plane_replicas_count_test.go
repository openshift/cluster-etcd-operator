package controlplanereplicascount

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestObserveControlPlaneReplicasCount(t *testing.T) {
	scenarios := []struct {
		name                       string
		installConfigPayload       string
		existingEtcdOperatorConfig map[string]interface{}
		expectedEtcdOperatorConfig map[string]interface{}
	}{
		{
			name:                       "replica count taken from the install config",
			installConfigPayload:       validInstallConfigYaml,
			expectedEtcdOperatorConfig: map[string]interface{}{"controlPlane": map[string]interface{}{"replicas": float64(3)}},
		},

		{
			name:                       "noop, all set",
			installConfigPayload:       validInstallConfigYaml,
			existingEtcdOperatorConfig: map[string]interface{}{"controlPlane": map[string]interface{}{"replicas": float64(3)}},
			expectedEtcdOperatorConfig: map[string]interface{}{"controlPlane": map[string]interface{}{"replicas": float64(3)}},
		},

		{
			name:                       "replica count updated",
			installConfigPayload:       validInstallConfigYaml,
			existingEtcdOperatorConfig: map[string]interface{}{"controlPlane": map[string]interface{}{"replicas": float64(6)}},
			expectedEtcdOperatorConfig: map[string]interface{}{"controlPlane": map[string]interface{}{"replicas": float64(3)}},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// set up
			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			clusterConfig := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-config-v1", Namespace: "kube-system"},
			}
			if len(scenario.installConfigPayload) > 0 {
				clusterConfig.Data = map[string]string{"install-config": scenario.installConfigPayload}
			}
			configMapIndexer.Add(clusterConfig)
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("kube-system")
			listers := configobservation.Listers{
				ConfigMapListerForKubeSystemNamespace: configMapLister,
			}
			// act
			observedConfig, err := ObserveControlPlaneReplicas(listers, nil, scenario.existingEtcdOperatorConfig)
			// validate
			if len(err) > 0 {
				t.Fatal(err)
			}
			if !cmp.Equal(scenario.expectedEtcdOperatorConfig, observedConfig) {
				t.Fatalf("unexpected configuraiton, diff = %v", cmp.Diff(scenario.expectedEtcdOperatorConfig, observedConfig))
			}
		})
	}
}

func TestReadDesiredControlPlaneReplicaCount(t *testing.T) {
	scenarios := []struct {
		name                             string
		installConfigPayload             string
		expectedControlPlaneReplicaCount int
		expectedError                    error
	}{
		// scenario 1
		{
			name:          "no install-config in cluster-config-1/kube-system",
			expectedError: fmt.Errorf("missing required key: install-config for cm: cluster-config-v1/kube-system"),
		},

		// scenario 2
		{
			name:                 "no install-config.controlPlane field in cluster-config-1/kube-system",
			installConfigPayload: emptyInstallConfigYaml,
			expectedError:        fmt.Errorf("required field: install-config.controlPlane.replicas doesn't exist in cm: cluster-config-v1/kube-system"),
		},

		// scenario 3
		{
			name:                 "no install-config.controlPlane.replicas field in cluster-config-1/kube-system",
			installConfigPayload: installConfigWithEmptyControlPlaneYaml,
			expectedError:        fmt.Errorf("required field: install-config.controlPlane.replicas doesn't exist in cm: cluster-config-v1/kube-system"),
		},

		// scenario 4
		{
			name:                 "invalid type of install-config.controlPlane.replicas field in cluster-config-1/kube-system",
			installConfigPayload: installConfigControlPlaneInvalidReplicasYaml,
			expectedError:        fmt.Errorf("failed to extract field: install-config.controlPlane.replicas from cm: cluster-config-v1/kube-system, err: .controlPlane.replicas accessor error: 3 is of the type string, expected float64"),
		},

		// scenario 5
		{
			name:                             "happy path, found 3 replicas in install-config.controlPlane.replicas field",
			installConfigPayload:             validInstallConfigYaml,
			expectedControlPlaneReplicaCount: 3,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			clusterConfig := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-config-v1", Namespace: "kube-system"},
			}
			if len(scenario.installConfigPayload) > 0 {
				clusterConfig.Data = map[string]string{"install-config": scenario.installConfigPayload}
			}
			configMapIndexer.Add(clusterConfig)
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("kube-system")

			// act
			actualReplicaCount, err := readDesiredControlPlaneReplicas(configMapLister)

			// validate
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from readDesiredControlPlaneReplicas function")
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatal(err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if actualReplicaCount != scenario.expectedControlPlaneReplicaCount {
				t.Fatalf("unexpected control plance replicat count: %d, expected: %d", actualReplicaCount, scenario.expectedControlPlaneReplicaCount)
			}
		})
	}
}

var emptyInstallConfigYaml = `
`

var installConfigWithEmptyControlPlaneYaml = `
controlPlane:
  architecture: amd64
`

var installConfigControlPlaneInvalidReplicasYaml = `
controlPlane:
  architecture: amd64
  hyperthreading: Enabled
  replicas: "3"
`

var validInstallConfigYaml = `
controlPlane:
  architecture: amd64
  hyperthreading: Enabled
  replicas: 3
`
