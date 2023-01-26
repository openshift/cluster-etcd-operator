package bootstrapteardown

import (
	"context"
	"fmt"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

var (
	bootstrapComplete = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "kube-system"},
		Data:       map[string]string{"status": "complete"},
	}

	conditionBootstrapAlreadyRemoved = operatorv1.OperatorCondition{
		Type:    "EtcdRunningInCluster",
		Status:  "True",
		Reason:  "BootstrapAlreadyRemoved",
		Message: "etcd-bootstrap member is already removed",
	}

	conditionBootstrapMemberRemoved = operatorv1.OperatorCondition{
		Type:    "EtcdBoostrapMemberRemoved",
		Status:  "True",
		Reason:  "BootstrapMemberRemoved",
		Message: "etcd bootstrap member is removed",
	}

	conditionWaitingForEtcdMembers = operatorv1.OperatorCondition{
		Type:    "EtcdRunningInCluster",
		Status:  "False",
		Reason:  "NotEnoughEtcdMembers",
		Message: "still waiting for three healthy etcd members",
	}

	conditionEnoughEtcdMembers = operatorv1.OperatorCondition{
		Type:    "EtcdRunningInCluster",
		Status:  "True",
		Reason:  "EnoughEtcdMembers",
		Message: "enough members found",
	}

	conditionEtcdMemberRemoved = operatorv1.OperatorCondition{
		Type:    "EtcdBoostrapMemberRemoved",
		Status:  "True",
		Reason:  "BootstrapMemberRemoved",
		Message: "etcd bootstrap member is removed",
	}

	bootstrapProgressing = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", Namespace: "kube-system"},
		Data:       map[string]string{"status": "progressing"},
	}
)

func TestCanRemoveEtcdBootstrap(t *testing.T) {

	defaultEtcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
		u.FakeEtcdMemberWithoutServer(3),
	}

	tests := map[string]struct {
		etcdMembers     []*etcdserverpb.Member
		clientFakeOpts  etcdcli.FakeClientOption
		scalingStrategy ceohelpers.BootstrapScalingStrategy
		safeToRemove    bool
		hasBootstrap    bool
		bootstrapId     uint64
	}{
		"default happy path no bootstrap": {
			etcdMembers:     defaultEtcdMembers,
			scalingStrategy: ceohelpers.HAScalingStrategy,
			safeToRemove:    false,
			hasBootstrap:    false,
			bootstrapId:     uint64(0),
		},
		"HA happy path with bootstrap": {
			etcdMembers:     append(defaultEtcdMembers, u.FakeEtcdBoostrapMember(0)),
			scalingStrategy: ceohelpers.HAScalingStrategy,
			safeToRemove:    true,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"HA happy path with bootstrap and not enough members": {
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdBoostrapMember(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			scalingStrategy: ceohelpers.HAScalingStrategy,
			safeToRemove:    false,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"HA with unhealthy member": {
			etcdMembers:     append(defaultEtcdMembers, u.FakeEtcdBoostrapMember(0)),
			clientFakeOpts:  etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Unhealthy: 1, Healthy: 3}),
			scalingStrategy: ceohelpers.HAScalingStrategy,
			safeToRemove:    false,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"HA with unhealthy bootstrap": {
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdBoostrapMember(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
				u.FakeEtcdMemberWithoutServer(3),
			},
			clientFakeOpts:  etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Unhealthy: 1, Healthy: 3}),
			scalingStrategy: ceohelpers.HAScalingStrategy,
			safeToRemove:    true,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"DelayedScaling happy path no bootstrap": {
			etcdMembers:     defaultEtcdMembers,
			scalingStrategy: ceohelpers.DelayedHAScalingStrategy,
			safeToRemove:    false,
			hasBootstrap:    false,
			bootstrapId:     uint64(0),
		},
		"DelayedScaling happy path with bootstrap": {
			etcdMembers:     append(defaultEtcdMembers, u.FakeEtcdBoostrapMember(0)),
			scalingStrategy: ceohelpers.DelayedHAScalingStrategy,
			safeToRemove:    true,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"DelayedScaling happy path with bootstrap and just enough members": {
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdBoostrapMember(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			scalingStrategy: ceohelpers.DelayedHAScalingStrategy,
			safeToRemove:    true,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"DelayedScaling happy path with bootstrap and not enough members": {
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdBoostrapMember(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			scalingStrategy: ceohelpers.DelayedHAScalingStrategy,
			safeToRemove:    false,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
		"UnsafeScaling happy path with bootstrap and enough members": {
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdBoostrapMember(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			scalingStrategy: ceohelpers.UnsafeScalingStrategy,
			safeToRemove:    true,
			hasBootstrap:    true,
			bootstrapId:     uint64(0),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.clientFakeOpts == nil {
				test.clientFakeOpts = etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Unhealthy: 0})
			}
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(test.etcdMembers, test.clientFakeOpts)
			require.NoError(t, err)

			c := &BootstrapTeardownController{
				etcdClient: fakeEtcdClient,
			}

			safeToRemoveBootstrap, hasBootstrap, bootstrapId, err := c.canRemoveEtcdBootstrap(context.TODO(), test.scalingStrategy)
			require.NoError(t, err)
			require.Equal(t, test.safeToRemove, safeToRemoveBootstrap, "safe to remove")
			require.Equal(t, test.hasBootstrap, hasBootstrap, "has bootstrap")
			require.Equal(t, test.bootstrapId, bootstrapId, "bootstrap id")
		})
	}
}

func TestRemoveBootstrap(t *testing.T) {

	tests := map[string]struct {
		safeToRemove       bool
		hasBootstrap       bool
		bootstrapId        uint64
		expectedConditions []operatorv1.OperatorCondition
		indexerObjs        []interface{}
		expectedErr        error
	}{
		"unsafe, no bootstrap": {
			safeToRemove: false,
			hasBootstrap: false,
			bootstrapId:  0,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionBootstrapMemberRemoved,
				conditionBootstrapAlreadyRemoved,
			},
		},
		"safe, no bootstrap": {
			safeToRemove: true,
			hasBootstrap: false,
			bootstrapId:  0,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionBootstrapMemberRemoved,
				conditionBootstrapAlreadyRemoved,
			},
		},
		"unsafe, has bootstrap": {
			safeToRemove: false,
			hasBootstrap: true,
			bootstrapId:  0,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionWaitingForEtcdMembers,
			},
		},
		"safe, has bootstrap, incomplete process": {
			safeToRemove: true,
			hasBootstrap: true,
			bootstrapId:  0,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionEnoughEtcdMembers,
			},
		},
		"safe, has bootstrap, incomplete process progressing": {
			safeToRemove: true,
			hasBootstrap: true,
			bootstrapId:  0,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionEnoughEtcdMembers,
			},
			indexerObjs: []interface{}{
				bootstrapProgressing,
			},
		},
		"safe, has bootstrap, complete process, fails removal": {
			safeToRemove: true,
			hasBootstrap: true,
			bootstrapId:  0,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionEnoughEtcdMembers,
			},
			indexerObjs: []interface{}{
				bootstrapComplete,
			},
			expectedErr: fmt.Errorf("error while removing bootstrap member [%x]: %w", 0, fmt.Errorf("member with the given ID: 0 doesn't exist")),
		},
		"safe, has bootstrap, complete process, successful removal": {
			safeToRemove: true,
			hasBootstrap: true,
			bootstrapId:  1,
			expectedConditions: []operatorv1.OperatorCondition{
				conditionEnoughEtcdMembers,
				conditionEtcdMemberRemoved,
			},
			indexerObjs: []interface{}{
				bootstrapComplete,
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {

			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range test.indexerObjs {
				require.NoError(t, indexer.Add(obj))
			}

			fakeNamespaceLister := corev1listers.NewNamespaceLister(indexer)
			fakeConfigmapLister := corev1listers.NewConfigMapLister(indexer)
			fakeInfraLister := configv1listers.NewInfrastructureLister(indexer)
			fakeStaticPodClient := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{}, nil, nil)
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient([]*etcdserverpb.Member{u.FakeEtcdBoostrapMember(1)})
			require.NoError(t, err)

			c := &BootstrapTeardownController{
				fakeStaticPodClient,
				fakeEtcdClient,
				fakeConfigmapLister,
				fakeNamespaceLister,
				fakeInfraLister,
			}

			err = c.removeBootstrap(context.TODO(), test.safeToRemove, test.hasBootstrap, test.bootstrapId)
			require.Equal(t, test.expectedErr, err)

			_, status, _, err := fakeStaticPodClient.GetStaticPodOperatorState()
			require.NoError(t, err)
			require.ElementsMatch(t, test.expectedConditions, removeTransitionTime(status.Conditions))
		})
	}
}

// removeTransitionTime will create a new list of operator conditions without the LastTransitionTime.
// We need to remove the time component to be able to match the structs in require.ElementsMatch
func removeTransitionTime(conditions []operatorv1.OperatorCondition) []operatorv1.OperatorCondition {
	var timelessConditions []operatorv1.OperatorCondition
	for _, c := range conditions {
		timelessConditions = append(timelessConditions, operatorv1.OperatorCondition{
			Type:    c.Type,
			Status:  c.Status,
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return timelessConditions
}
