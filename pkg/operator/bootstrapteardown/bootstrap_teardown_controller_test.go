package bootstrapteardown

import (
	"context"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
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
