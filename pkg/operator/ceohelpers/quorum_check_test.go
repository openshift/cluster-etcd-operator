package ceohelpers

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestQuorumCheck_IsSafeToUpdateRevision(t *testing.T) {

	defaultEtcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
	}

	// this is largely the same as in boostrap_test.go
	defaultObjects := []runtime.Object{
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		&configv1.Infrastructure{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name: InfrastructureClusterName,
			},
			Status: configv1.InfrastructureStatus{
				ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
		},
	}

	scenarios := []struct {
		name            string
		objects         []runtime.Object
		staticPodStatus *operatorv1.StaticPodOperatorStatus
		etcdMembers     []*etcdserverpb.Member
		endpointsString string

		safe        bool
		expectedErr error
	}{
		{
			name:    "HappyPath",
			objects: []runtime.Object{},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: defaultEtcdMembers,
			safe:        true,
		},
		{
			name:    "Incomplete Quorum",
			objects: []runtime.Object{},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			safe:        false,
			expectedErr: fmt.Errorf("CheckSafeToScaleCluster found 2 healthy member(s) out of the 3 required by the HAScalingStrategy"),
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				scenario.staticPodStatus,
				nil,
				nil,
			)

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range defaultObjects {
				require.NoError(t, indexer.Add(obj))
			}

			for _, obj := range scenario.objects {
				require.NoError(t, indexer.Add(obj))
			}

			quorumChecker := NewQuorumChecker(
				corev1listers.NewNamespaceLister(indexer),
				configv1listers.NewInfrastructureLister(indexer),
				fakeOperatorClient,
				fakeEtcdClient,
				corev1listers.NewNodeLister(indexer))

			safe, err := quorumChecker.IsSafeToUpdateRevision()
			assert.Equal(t, scenario.expectedErr, err)
			assert.Equal(t, scenario.safe, safe)
		})
	}
}

func TestQuorumCheck_IsSafeToRestartMember(t *testing.T) {
	defaultEtcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
	}

	defaultObjects := []runtime.Object{
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		&configv1.Infrastructure{
			ObjectMeta: metav1.ObjectMeta{Name: InfrastructureClusterName},
			Status: configv1.InfrastructureStatus{
				ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
		},
	}

	node := func(name string, ready bool, unschedulable bool) *corev1.Node {
		status := corev1.ConditionTrue
		if !ready {
			status = corev1.ConditionFalse
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: status},
			}},
		}
	}

	scenarios := []struct {
		name        string
		nodes       []*corev1.Node
		etcdMembers []*etcdserverpb.Member
		targetNode  string

		safe           bool
		reasonContains string
	}{
		{
			name:        "all nodes ready and schedulable",
			nodes:       []*corev1.Node{node("master-0", true, false), node("master-1", true, false), node("master-2", true, false)},
			etcdMembers: defaultEtcdMembers,
			targetNode:  "master-1",
			safe:        true,
		},
		{
			name:        "another master cordoned",
			nodes:       []*corev1.Node{node("master-0", true, true), node("master-1", true, false), node("master-2", true, false)},
			etcdMembers: defaultEtcdMembers,
			targetNode:  "master-1",
			safe:        false, reasonContains: "cordoned",
		},
		{
			name:        "target itself cordoned is allowed",
			nodes:       []*corev1.Node{node("master-0", true, false), node("master-1", true, true), node("master-2", true, false)},
			etcdMembers: defaultEtcdMembers,
			targetNode:  "master-1",
			safe:        true,
		},
		{
			name:        "another master not ready",
			nodes:       []*corev1.Node{node("master-0", false, false), node("master-1", true, false), node("master-2", true, false)},
			etcdMembers: defaultEtcdMembers,
			targetNode:  "master-1",
			safe:        false, reasonContains: "not ready",
		},
		{
			name:  "quorum not fault tolerant",
			nodes: []*corev1.Node{node("master-0", true, false), node("master-1", true, false), node("master-2", true, false)},
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			targetNode: "master-1",
			safe:       false, reasonContains: "quorum",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed},
				},
				u.StaticPodOperatorStatus(
					u.WithLatestRevision(3),
					u.WithNodeStatusAtCurrentRevision(3),
					u.WithNodeStatusAtCurrentRevision(3),
					u.WithNodeStatusAtCurrentRevision(3),
				),
				nil,
				nil,
			)
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			require.NoError(t, err)

			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range defaultObjects {
				require.NoError(t, indexer.Add(obj))
			}
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, n := range scenario.nodes {
				require.NoError(t, nodeIndexer.Add(n))
			}

			quorumChecker := NewQuorumChecker(
				corev1listers.NewNamespaceLister(indexer),
				configv1listers.NewInfrastructureLister(indexer),
				fakeOperatorClient,
				fakeEtcdClient,
				corev1listers.NewNodeLister(nodeIndexer))

			safe, reason, err := quorumChecker.IsSafeToRestartMember(context.TODO(), scenario.targetNode)
			require.NoError(t, err)
			assert.Equal(t, scenario.safe, safe)
			if scenario.reasonContains != "" {
				assert.Contains(t, reason, scenario.reasonContains)
			}
		})
	}
}
