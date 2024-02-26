package etcdcli

import (
	"context"
	"testing"
	"time"

	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/stretchr/testify/require"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
)

func TestCachedMembers_refresh(t *testing.T) {
	client, err := NewFakeEtcdClient(defaultEtcdMembers(), WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}))
	require.NoError(t, err)
	memberCache := newMemberCache(client, 1*time.Second)
	currentMembers, err := memberCache.MemberList(context.Background())
	require.Equal(t, defaultEtcdMembers(), currentMembers)

	err = client.MemberRemove(context.Background(), uint64(2))
	require.NoError(t, err)
	time.Sleep(1 * time.Second)
	currentMembers, err = memberCache.MemberList(context.Background())
	require.Equal(t, fakeEtcdMembers(1, 3), currentMembers)
}

func TestCachedMembers_MemberList(t *testing.T) {
	testCases := []struct {
		name            string
		etcdMembersList []*etcdserverpb.Member
		clientOpts      FakeClientOption
		expEtcdMembers  []*etcdserverpb.Member
	}{
		{
			"all healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			defaultEtcdMembers(),
		},
		{
			"one unhealthy member and two healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			defaultEtcdMembers(),
		},
		{
			"all unhealthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 0, Unhealthy: 3}),
			defaultEtcdMembers(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			currentMembers, err := memberCache.MemberList(context.Background())
			require.Equal(t, tc.expEtcdMembers, currentMembers)
		})
	}
}

func TestCachedMembers_VotingMemberList(t *testing.T) {
	testCases := []struct {
		name            string
		etcdMembersList []*etcdserverpb.Member
		clientOpts      FakeClientOption
		expEtcdMembers  []*etcdserverpb.Member
	}{
		{
			"all healthy voting members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			defaultEtcdMembers(),
		},
		{
			"one unhealthy non voting member and two healthy voting members",
			asLearner(defaultEtcdMembers(), 3),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			fakeEtcdMembers(1, 2),
		},
		{
			"one healthy non voting member and two unhealthy voting members",
			asLearner(defaultEtcdMembers(), 1),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 1, Unhealthy: 2}),
			fakeEtcdMembers(2, 3),
		},
		{
			"all unhealthy voting members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 0, Unhealthy: 3}),
			defaultEtcdMembers(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			currentMembers, err := memberCache.VotingMemberList(context.Background())
			require.Equal(t, tc.expEtcdMembers, currentMembers)
		})
	}
}

func TestCachedMembers_HealthyMembers(t *testing.T) {
	testCases := []struct {
		name            string
		etcdMembersList []*etcdserverpb.Member
		clientOpts      FakeClientOption
		expEtcdMembers  []*etcdserverpb.Member
	}{
		{
			"all healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			defaultEtcdMembers(),
		},
		{
			"one unhealthy member and two healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			fakeEtcdMembers(1, 2),
		},
		{
			"all unhealthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 0, Unhealthy: 3}),
			emptyEtcdMembers(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			currentMembers, err := memberCache.HealthyMembers(context.Background())
			require.Equal(t, tc.expEtcdMembers, currentMembers)
		})
	}
}

func TestCachedMembers_HealthyVotingMembers(t *testing.T) {
	testCases := []struct {
		name            string
		etcdMembersList []*etcdserverpb.Member
		clientOpts      FakeClientOption
		expEtcdMembers  []*etcdserverpb.Member
	}{
		{
			"all healthy voting members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			defaultEtcdMembers(),
		},
		{
			"all healthy and one non voting members",
			asLearner(defaultEtcdMembers(), 2),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			fakeEtcdMembers(1, 3),
		},
		{
			"one unhealthy non voting member and two healthy voting members",
			asLearner(defaultEtcdMembers(), 3),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			fakeEtcdMembers(1, 2),
		},
		{
			"all unhealthy voting members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 0, Unhealthy: 3}),
			emptyEtcdMembers(),
		},
		{
			"all unhealthy non voting members",
			asLearner(defaultEtcdMembers(), 1, 2, 3),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 0, Unhealthy: 3}),
			emptyEtcdMembers(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			currentMembers, err := memberCache.HealthyVotingMembers(context.Background())
			require.Equal(t, tc.expEtcdMembers, currentMembers)
		})
	}
}

func TestCachedMembers_UnhealthyMembers(t *testing.T) {
	testCases := []struct {
		name            string
		etcdMembersList []*etcdserverpb.Member
		clientOpts      FakeClientOption
		expEtcdMembers  []*etcdserverpb.Member
	}{
		{
			"all healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			emptyEtcdMembers(),
		},
		{
			"one unhealthy member and two healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			fakeEtcdMembers(3),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			currentMembers, err := memberCache.UnhealthyMembers(context.Background())
			require.Equal(t, tc.expEtcdMembers, currentMembers)
		})
	}
}

func TestCachedMembers_UnhealthyVotingMembers(t *testing.T) {
	testCases := []struct {
		name            string
		etcdMembersList []*etcdserverpb.Member
		clientOpts      FakeClientOption
		expEtcdMembers  []*etcdserverpb.Member
	}{
		{
			"all healthy voting members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			emptyEtcdMembers(),
		},
		{
			"one unhealthy voting member and two healthy voting members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			fakeEtcdMembers(3),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			currentMembers, err := memberCache.UnhealthyMembers(context.Background())
			require.Equal(t, tc.expEtcdMembers, currentMembers)
		})
	}
}

func TestCachedMembers_MemberHealth(t *testing.T) {
	testCases := []struct {
		name                string
		etcdMembersList     []*etcdserverpb.Member
		clientOpts          FakeClientOption
		expHealthyMembers   int
		expUnhealthyMembers int
	}{
		{
			"all healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			3,
			0,
		},
		{
			"one unhealthy member and two healthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			2,
			1,
		},
		{
			"all unhealthy members",
			defaultEtcdMembers(),
			WithFakeClusterHealth(&FakeMemberHealth{Healthy: 0, Unhealthy: 3}),
			0,
			3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewFakeEtcdClient(tc.etcdMembersList, tc.clientOpts)
			require.NoError(t, err)
			memberCache := newMemberCache(client, 1*time.Millisecond)

			health, err := memberCache.MemberHealth(context.Background())
			assertMemberHealth(t, health, tc.expHealthyMembers, tc.expUnhealthyMembers)
		})
	}
}

func defaultEtcdMembers() []*etcdserverpb.Member {
	return fakeEtcdMembers(1, 2, 3)
}

func emptyEtcdMembers() []*etcdserverpb.Member {
	return fakeEtcdMembers()
}

func fakeEtcdMembers(ids ...int) []*etcdserverpb.Member {
	var members []*etcdserverpb.Member
	for _, id := range ids {
		members = append(members, u.FakeEtcdMemberWithoutServer(id))
	}
	return members
}

func asLearner(members []*etcdserverpb.Member, ids ...int) []*etcdserverpb.Member {
	for _, id := range ids {
		members[id-1].IsLearner = true
	}
	return members
}

func assertMemberHealth(t testing.TB, health memberHealth, expHealthy, expUnhealthy int) {
	t.Helper()

	actualHealthy, actualUnhealthy := 0, 0
	for _, h := range health {
		if h.Healthy {
			actualHealthy++
		} else {
			actualUnhealthy++
		}
	}

	require.Equal(t, expHealthy, actualHealthy)
	require.Equal(t, expUnhealthy, actualUnhealthy)
}
