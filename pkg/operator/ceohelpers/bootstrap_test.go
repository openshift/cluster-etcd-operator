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

	twoNodeInfra = &configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: InfrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.DualReplicaTopologyMode},
	}

	namespaceWithLegacyDelayedHAEnabled = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: operatorclient.TargetNamespace,
			Annotations: map[string]string{
				DelayedHABootstrapScalingStrategyAnnotation: "",
			},
		},
	}

	namespaceWithDelayedEnabled = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: operatorclient.TargetNamespace,
			Annotations: map[string]string{
				DelayedBootstrapScalingStrategyAnnotation: "",
			},
		},
	}

	oneEtcdMember = []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
	}

	twoEtcdMembers = []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
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

	oneHealthyNode         = etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 1, Unhealthy: 0})
	twoHealthyNodes        = etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 2, Unhealthy: 0})
	twoHealthyOfThreeNodes = etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 2, Unhealthy: 1})
	threeHealthyNodes      = etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 0})
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
		"two_node": {
			namespace:      defaultNamespace,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: TwoNodeScalingStrategy,
			infraObj:       twoNodeInfra,
		},
		"delayed HA": {
			namespace:      namespaceWithLegacyDelayedHAEnabled,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: DelayedHAScalingStrategy,
			infraObj:       defaultInfra,
		},
		"delayed two_node": {
			namespace:      namespaceWithDelayedEnabled,
			operatorConfig: defaultOperatorConfig,
			expectStrategy: DelayedTwoNodeScalingStrategy,
			infraObj:       twoNodeInfra,
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
		etcdMembers        []*etcdserverpb.Member
		expectComplete     bool
		expectError        error
	}{
		"bootstrap complete, configmap status is complete": {
			bootstrapConfigMap: bootstrapComplete,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap progressing, configmap status is progressing": {
			bootstrapConfigMap: bootstrapProgressing,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     false,
			expectError:        nil,
		},
		"bootstrap configmap missing": {
			bootstrapConfigMap: nil,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     false,
			expectError:        nil,
		},
		"bootstrap complete, etcd-bootstrap removed": {
			bootstrapConfigMap: bootstrapComplete,
			etcdMembers:        u.DefaultEtcdMembers(),
			expectComplete:     true,
			expectError:        nil,
		},
		"bootstrap complete, etcd-bootstrap exists": {
			bootstrapConfigMap: bootstrapComplete,
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

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(test.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}

			actualComplete, actualErr := IsBootstrapComplete(fakeConfigMapLister, fakeEtcdClient)

			assert.Equal(t, test.expectComplete, actualComplete)
			assert.Equal(t, test.expectError, actualErr)
		})
	}
}

func Test_IsRevisionStable(t *testing.T) {
	tests := map[string]struct {
		nodes             []operatorv1.NodeStatus
		expectedStability bool
		expectedError     error
	}{
		"is revision stable, node progressing": {
			nodes:             twoNodesProgressingTowardsCurrentRevision,
			expectedStability: false,
			expectedError:     nil,
		},
		"is revision stable, nodes up to date": {
			nodes:             twoNodesAtCurrentRevision,
			expectedStability: true,
			expectedError:     nil,
		},
		"bootstrap complete, no recorded revisions": {
			nodes:             zeroNodesAtAnyRevision,
			expectedStability: true,
			expectedError:     nil,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			operatorStatus := &operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 1},
				NodeStatuses:   test.nodes,
			}
			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(nil, operatorStatus, nil, nil)

			actualStability, actualErr := IsRevisionStable(fakeStaticPodClient)

			assert.Equal(t, test.expectedStability, actualStability)
			assert.Equal(t, test.expectedError, actualErr)

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
		etcdClientOps      etcdcli.FakeClientOption
		infraObj           *configv1.Infrastructure
	}{
		"HA with sufficient members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			etcdClientOps:      threeHealthyNodes,
			expectError:        nil,
		},
		"HA with insufficient etcd members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        twoEtcdMembers,
			infraObj:           defaultInfra,
			etcdClientOps:      twoHealthyNodes,
			expectError:        fmt.Errorf("CheckSafeToScaleCluster found 2 healthy member(s) out of the 3 required by the HAScalingStrategy"),
		},
		"HA with two out of three healthy members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			etcdClientOps:      twoHealthyOfThreeNodes,
			expectError:        fmt.Errorf("CheckSafeToScaleCluster found 2 healthy member(s) out of the 3 required by the HAScalingStrategy"),
		},
		"HA with three out of five healthy members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes: []operatorv1.NodeStatus{
				{NodeName: "node-1", CurrentRevision: 1},
				{NodeName: "node-2", CurrentRevision: 1},
				{NodeName: "node-3", CurrentRevision: 1},
				{NodeName: "node-4", CurrentRevision: 1},
				{NodeName: "node-5", CurrentRevision: 1},
			},
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
				u.FakeEtcdMemberWithoutServer(3),
				u.FakeEtcdMemberWithoutServer(4),
			},
			infraObj:      defaultInfra,
			etcdClientOps: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 2}),
			expectError:   fmt.Errorf("etcd cluster has quorum of 3 and 3 healthy members which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.2:2380\" clientURLs:\"https://10.0.0.2:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:2 name:\"etcd-2\" peerURLs:\"https://10.0.0.3:2380\" clientURLs:\"https://10.0.0.3:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:3 name:\"etcd-3\" peerURLs:\"https://10.0.0.4:2380\" clientURLs:\"https://10.0.0.4:2907\"  Healthy:false Took: Error:<nil>} {Member:ID:4 name:\"etcd-4\" peerURLs:\"https://10.0.0.5:2380\" clientURLs:\"https://10.0.0.5:2907\"  Healthy:false Took: Error:<nil>}]"),
		},
		"two node with sufficient members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        twoEtcdMembers,
			infraObj:           twoNodeInfra,
			etcdClientOps:      twoHealthyNodes,
			expectError:        nil,
		},
		"two node with insufficient etcd members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			etcdMembers:        oneEtcdMember,
			infraObj:           twoNodeInfra,
			etcdClientOps:      oneHealthyNode,
			expectError:        fmt.Errorf("CheckSafeToScaleCluster found 1 healthy member(s) out of the 2 required by the TwoNodeScalingStrategy"),
		},
		"two node with insufficient healthy etcd members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        twoEtcdMembers,
			infraObj:           twoNodeInfra,
			etcdClientOps:      etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 1, Unhealthy: 1}),
			expectError:        fmt.Errorf("CheckSafeToScaleCluster found 1 healthy member(s) out of the 2 required by the TwoNodeScalingStrategy"),
		},
		// A two node cluster with 3 etcd members is undefined in its behavioral expectations.
		// The relevant case for this would be when replacing a broken control plane node.
		// In the situation, the expectation would be that the user removes the unhealthy control
		// plane node first, allowing pacemaker to remove that node from the membership list to restore
		// quorum as a cluster of a single member. Once this is complete, the replacement control plane node
		// should be started and admitted to the cluster as a learner.
		//
		// This test ensures that we default to restrictive scaling in a state where we have an unexpected number
		// of members in a TNF cluster.
		"two node with two out of three healthy members": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           twoNodeInfra,
			etcdClientOps:      twoHealthyOfThreeNodes,
			expectError:        fmt.Errorf("etcd cluster has quorum of 2 and 2 healthy members which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.2:2380\" clientURLs:\"https://10.0.0.2:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:2 name:\"etcd-2\" peerURLs:\"https://10.0.0.3:2380\" clientURLs:\"https://10.0.0.3:2907\"  Healthy:false Took: Error:<nil>}]"),
		},
		"delayed Two Node with sufficient nodes during bootstrap": {
			namespace:          namespaceWithDelayedEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        twoEtcdMembers,
			infraObj:           twoNodeInfra,
			etcdClientOps:      twoHealthyNodes,
			expectError:        nil,
		},
		"delayed Two Node with insufficient nodes during bootstrap should succeed": {
			namespace:          namespaceWithDelayedEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			etcdMembers:        twoEtcdMembers,
			infraObj:           twoNodeInfra,
			etcdClientOps:      twoHealthyNodes,
			expectError:        nil,
		},
		"delayed Two Node with sufficient nodes during steady state": {
			namespace:          namespaceWithDelayedEnabled,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        twoEtcdMembers,
			infraObj:           twoNodeInfra,
			etcdClientOps:      twoHealthyNodes,
			expectError:        nil,
		},
		"unsupported with sufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     unsupportedOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			etcdClientOps:      threeHealthyNodes,
			expectError:        nil,
		},
		"unsupported with insufficient nodes": {
			namespace:          defaultNamespace,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     unsupportedOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			etcdMembers:        oneEtcdMember,
			infraObj:           defaultInfra,
			etcdClientOps:      oneHealthyNode,
			expectError:        nil,
		},
		"delayed HA with sufficient nodes during bootstrap": {
			namespace:          namespaceWithLegacyDelayedHAEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              twoNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			etcdClientOps:      threeHealthyNodes,
			expectError:        nil,
		},
		"delayed HA with insufficient nodes during bootstrap should succeed": {
			namespace:          namespaceWithLegacyDelayedHAEnabled,
			bootstrapConfigMap: bootstrapProgressing,
			operatorConfig:     defaultOperatorConfig,
			nodes:              oneNodeAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			etcdClientOps:      threeHealthyNodes,
			expectError:        nil,
		},
		"delayed HA with sufficient nodes during steady state": {
			namespace:          namespaceWithLegacyDelayedHAEnabled,
			bootstrapConfigMap: bootstrapComplete,
			operatorConfig:     defaultOperatorConfig,
			nodes:              threeNodesAtCurrentRevision,
			etcdMembers:        u.DefaultEtcdMembers(),
			infraObj:           defaultInfra,
			etcdClientOps:      threeHealthyNodes,
			expectError:        nil,
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
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(test.etcdMembers, test.etcdClientOps)
			if err != nil {
				t.Fatal(err)
			}

			actualErr := CheckSafeToScaleCluster(
				fakeStaticPodClient,
				fakeNamespaceMapLister,
				fakeInfraStructure,
				fakeEtcdClient)

			assert.Equal(t, test.expectError, actualErr)
		})
	}
}
