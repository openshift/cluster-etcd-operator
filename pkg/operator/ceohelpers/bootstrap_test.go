package ceohelpers

import (
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/assert"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

var (
	defaultNamespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
	}

	defaultInfra = &configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: InfrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
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
	singleNode := &configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: InfrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.SingleReplicaTopologyMode},
	}

	tests := map[string]struct {
		namespace      *corev1.Namespace
		operatorConfig operatorv1.StaticPodOperatorSpec
		expectStrategy BootstrapScalingStrategy
		infraObj       *configv1.Infrastructure
	}{
		"default should be HA": {
			namespace:      defaultNamespace,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: HAScalingStrategy,
			infraObj:       defaultInfra,
		},
		"unsupported": {
			namespace:      defaultNamespace,
			operatorConfig: unsupportedOperatorConfig,
			expectStrategy: UnsafeScalingStrategy,
			infraObj:       defaultInfra,
		},
		"single_node": {
			namespace:      defaultNamespace,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: UnsafeScalingStrategy,
			infraObj:       singleNode,
		},
		"delayed HA": {
			namespace:      namespaceWithDelayedHAEnabled,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: DelayedHAScalingStrategy,
			infraObj:       defaultInfra,
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

			fakeInfraIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if test.infraObj != nil {
				if err := fakeInfraIndexer.Add(test.infraObj); err != nil {
					t.Fatal(err)
				}
			}
			fakeInfraStructure := configv1listers.NewInfrastructureLister(fakeInfraIndexer)

			actualStrategy, err := GetBootstrapScalingStrategy(fakeStaticPodClient, fakeNamespaceMapLister, fakeInfraStructure)
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
		etcdMembers        []*etcdserverpb.Member
		expectComplete     bool
		expectError        error
	}{
		"bootstrap complete, nodes up to date": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap progressing, nodes up to date": {
			bootstrapConfigMap: bootstrapProgressing,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     false,
			expectError:        nil,
		},
		"bootstrap configmap missing": {
			bootstrapConfigMap: nil,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     false,
			expectError:        nil,
		},
		"bootstrap complete, no recorded revisions": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              zeroNodesAtAnyRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap complete, etcd-bootstrap removed": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap complete, etcd-bootstrap exists": {
			bootstrapConfigMap: bootstrapComplete,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        append(u.DefaultEtcdMembers(), u.FakeEtcdBootstrapMember(0)),
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
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 1},
				NodeStatuses:   test.nodes,
			}
			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(nil, operatorStatus, nil, nil)

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(test.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}

			actualComplete, actualErr := IsBootstrapComplete(fakeConfigMapLister, fakeStaticPodClient, fakeEtcdClient)

			assert.Equal(t, test.expectComplete, actualComplete)
			assert.Equal(t, test.expectError, actualErr)
		})
	}
}

func Test_CheckSafeToScaleCluster(t *testing.T) {
	tests := map[string]struct {
		namespace          *corev1.Namespace
		bootstrapConfigMap *corev1.ConfigMap
		operatorConfig     operatorv1.StaticPodOperatorSpec
		nodes              []operatorv1.NodeStatus
		etcdMembers        []*etcdserverpb.Member
		expectComplete     bool
		expectError        error
		infraObj           *configv1.Infrastructure
	}{
		"HA with sufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			expectError:        nil,
		},
		"unsupported with sufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     unsupportedOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			expectError:        nil,
		},
		"unsupported with insufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     unsupportedOperatorConfig,
			nodes:              zeroNodesAtAnyRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			expectError:        nil,
		},
		"delayed HA with sufficient nodes during bootstrap": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			expectError:        nil,
		},
		"delayed HA with insufficient nodes during bootstrap should succeed": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			expectError:        nil,
		},
		"delayed HA with sufficient nodes during steady state": {
			namespace:          namespaceWithDelayedHAEnabled,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			expectError:        nil,
		},
		"HA with insufficient etcd members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			infraObj:    defaultInfra,
			expectError: fmt.Errorf("etcd cluster has quorum of 2 which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.2:2380\" clientURLs:\"https://10.0.0.2:2907\"  Healthy:true Took: Error:<nil>}]"),
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
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 1},
				NodeStatuses:   test.nodes,
			}
			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(&test.operatorConfig, operatorStatus, nil, nil)

			fakeInfraIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if test.infraObj != nil {
				if err := fakeInfraIndexer.Add(test.infraObj); err != nil {
					t.Fatal(err)
				}
			}
			fakeInfraStructure := configv1listers.NewInfrastructureLister(fakeInfraIndexer)
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(test.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}

			actualErr := CheckSafeToScaleCluster(
				fakeConfigMapLister,
				fakeStaticPodClient,
				fakeNamespaceMapLister,
				fakeInfraStructure,
				fakeEtcdClient)

			assert.Equal(t, test.expectError, actualErr)
		})
	}
}
