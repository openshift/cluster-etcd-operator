package ceohelpers

import (
	"fmt"
	"testing"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

var (
	defaultNamespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
	}

	namespaceWithDelayedHAEnabled = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: operatorclient.TargetNamespace,
			Annotations: map[string]string{
				DelayedHABootstrapScalingStrategyAnnotation: "",
			},
		},
	}

	defaultOperatorConfig = operatorv1.StaticPodOperatorSpec{}

	unsupportedOperatorConfig = operatorv1.StaticPodOperatorSpec{
		OperatorSpec: operatorv1.OperatorSpec{
			UnsupportedConfigOverrides: runtime.RawExtension{
				Raw: []byte(`useUnsupportedUnsafeNonHANonProductionUnstableEtcd: "true"`),
			},
		},
	}

	bootstrapComplete = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "kube-system"},
		Data:       map[string]string{"status": "complete"},
	}

	bootstrapProgressing = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "kube-system"},
		Data:       map[string]string{"status": "progressing"},
	}

	oneNodeAtCurrentRevision = []operatorv1.NodeStatus{
		{NodeName: "node-1", CurrentRevision: 1},
	}

	twoNodesAtCurrentRevision = []operatorv1.NodeStatus{
		{NodeName: "node-1", CurrentRevision: 1},
		{NodeName: "node-2", CurrentRevision: 1},
	}

	twoNodesProgressingTowardsCurrentRevision = []operatorv1.NodeStatus{
		{NodeName: "node-1", CurrentRevision: 1},
		{NodeName: "node-2", CurrentRevision: 0},
	}

	threeNodesAtCurrentRevision = []operatorv1.NodeStatus{
		{NodeName: "node-1", CurrentRevision: 1},
		{NodeName: "node-2", CurrentRevision: 1},
		{NodeName: "node-3", CurrentRevision: 1},
	}

	zeroNodesAtAnyRevision = []operatorv1.NodeStatus{}
)

func Test_GetBootstrapScalingStrategy(t *testing.T) {
	tests := map[string]struct {
		namespace      *corev1.Namespace
		operatorConfig operatorv1.StaticPodOperatorSpec
		expectStrategy BootstrapScalingStrategy
	}{
		"default should be HA": {
			namespace:      defaultNamespace,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: HAScalingStrategy,
		},
		"unsupported": {
			namespace:      defaultNamespace,
			operatorConfig: unsupportedOperatorConfig,
			expectStrategy: UnsafeScalingStrategy,
		},
		"delayed HA": {
			namespace:      namespaceWithDelayedHAEnabled,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: DelayedHAScalingStrategy,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if test.namespace != nil {
				if err := indexer.Add(test.namespace); err != nil {
					t.Fatal(err)
				}
			}
			fakeNamespaceMapLister := corev1listers.NewNamespaceLister(indexer)

			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(&test.operatorConfig, nil, nil, nil)

			actualStrategy, err := GetBootstrapScalingStrategy(fakeStaticPodClient, fakeNamespaceMapLister)
			if err != nil {
				t.Errorf("unexpected error: %s", err)
				return
			}
			if test.expectStrategy != actualStrategy {
				t.Errorf("expected stategy=%v, got %v", test.expectStrategy, actualStrategy)
			}
		})
	}
}

func Test_IsBootstrapComplete(t *testing.T) {
	tests := map[string]struct {
		bootstrapConfigMap *corev1.ConfigMap
		nodes              []operatorv1.NodeStatus
		expectComplete     bool
		expectError        error
	}{
		"bootstrap complete, nodes up to date": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              twoNodesAtCurrentRevision,
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap progressing, nodes up to date": {
			bootstrapConfigMap: bootstrapProgressing,
			nodes:              twoNodesAtCurrentRevision,
			expectComplete:     false,
			expectError:        nil,
		},
		"bootstrap configmap missing": {
			bootstrapConfigMap: nil,
			nodes:              twoNodesAtCurrentRevision,
			expectComplete:     false,
			expectError:        nil,
		},
		"bootstrap complete, no recorded revisions": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              zeroNodesAtAnyRevision,
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap complete, node progressing": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              twoNodesProgressingTowardsCurrentRevision,
			expectComplete:     false,
			expectError:        nil,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if test.bootstrapConfigMap != nil {
				if err := indexer.Add(test.bootstrapConfigMap); err != nil {
					t.Fatal(err)
				}
			}
			fakeConfigMapLister := corev1listers.NewConfigMapLister(indexer)

			operatorStatus := &operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 1,
				NodeStatuses:            test.nodes,
			}
			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(nil, operatorStatus, nil, nil)

			actualComplete, actualErr := IsBootstrapComplete(fakeConfigMapLister, fakeStaticPodClient)

			if test.expectComplete != actualComplete {
				t.Errorf("expected complete=%v, got %v", test.expectComplete, actualComplete)
			}
			if test.expectError != actualErr {
				t.Errorf("expected error=%v, got %v", test.expectError, actualErr)
			}
		})
	}
}

func Test_CheckSafeToScaleCluster(t *testing.T) {
	tests := map[string]struct {
		namespace          *corev1.Namespace
		bootstrapConfigMap *corev1.ConfigMap
		operatorConfig     operatorv1.StaticPodOperatorSpec
		nodes              []operatorv1.NodeStatus
		expectComplete     bool
		expectError        error
	}{
		"HA with sufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			expectError:        nil,
		},
		"HA with insufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			expectError:        fmt.Errorf("not enough nodes"),
		},
		"unsupported with sufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     unsupportedOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			expectError:        nil,
		},
		"unsupported with insufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     unsupportedOperatorConfig,
			nodes:              zeroNodesAtAnyRevision,
			expectError:        fmt.Errorf("not enough nodes"),
		},
		"delayed HA with sufficient nodes during bootstrap": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			expectError:        nil,
		},
		"delayed HA with insufficient nodes during bootstrap": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			expectError:        fmt.Errorf("not enough nodes"),
		},
		"delayed HA with sufficient nodes during steady state": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			expectError:        nil,
		},
		"delayed HA with insufficient nodes during steady state": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			expectError:        fmt.Errorf("not enough nodes"),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			namespaceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if test.namespace != nil {
				if err := namespaceIndexer.Add(test.namespace); err != nil {
					t.Fatal(err)
				}
			}
			fakeNamespaceMapLister := corev1listers.NewNamespaceLister(namespaceIndexer)

			configmapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if test.bootstrapConfigMap != nil {
				if err := configmapIndexer.Add(test.bootstrapConfigMap); err != nil {
					t.Fatal(err)
				}
			}
			fakeConfigMapLister := corev1listers.NewConfigMapLister(configmapIndexer)

			operatorStatus := &operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 1,
				NodeStatuses:            test.nodes,
			}
			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(&test.operatorConfig, operatorStatus, nil, nil)

			actualErr := CheckSafeToScaleCluster(fakeConfigMapLister, fakeStaticPodClient, fakeNamespaceMapLister)

			if test.expectError != nil && actualErr == nil {
				t.Errorf("expected error=%v, got %v", test.expectError, actualErr)
			}
			if test.expectError == nil && actualErr != nil {
				t.Errorf("expected error=%v, got %v", test.expectError, actualErr)
			}
		})
	}
}
