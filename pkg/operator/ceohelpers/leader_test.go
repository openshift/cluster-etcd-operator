package ceohelpers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// mockMemberStatusChecker implements MemberStatusChecker for testing
type mockMemberStatusChecker struct {
	memberStatuses map[uint64]*clientv3.StatusResponse
	errors         map[uint64]error
}

func (m *mockMemberStatusChecker) MemberStatus(ctx context.Context, member *etcdserverpb.Member) (*clientv3.StatusResponse, error) {
	if err, exists := m.errors[member.ID]; exists {
		return nil, err
	}
	if status, exists := m.memberStatuses[member.ID]; exists {
		return status, nil
	}
	return nil, errors.New("member status not found")
}

// mockLeaderMover implements LeaderMover for testing
type mockLeaderMover struct {
	moveLeaderErrors map[uint64]error
}

func (m *mockLeaderMover) MoveLeader(ctx context.Context, toMember uint64) error {
	if err, exists := m.moveLeaderErrors[toMember]; exists {
		return err
	}
	return nil
}

func TestFindLeader(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		memberList     []*etcdserverpb.Member
		memberStatuses map[uint64]*clientv3.StatusResponse
		errors         map[uint64]error
		expectedLeader *etcdserverpb.Member
		expectedError  string
	}{
		{
			name: "successfully find leader",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
				{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}},
			},
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 2},
				2: {Header: &etcdserverpb.ResponseHeader{MemberId: 2}, Leader: 2},
				3: {Header: &etcdserverpb.ResponseHeader{MemberId: 3}, Leader: 2},
			},
			expectedLeader: &etcdserverpb.Member{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
		},
		{
			name: "leader is first member",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
			},
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 1},
				2: {Header: &etcdserverpb.ResponseHeader{MemberId: 2}, Leader: 1},
			},
			expectedLeader: &etcdserverpb.Member{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
		},
		{
			name: "leader is last member",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
				{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}},
			},
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 3},
				2: {Header: &etcdserverpb.ResponseHeader{MemberId: 2}, Leader: 3},
				3: {Header: &etcdserverpb.ResponseHeader{MemberId: 3}, Leader: 3},
			},
			expectedLeader: &etcdserverpb.Member{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}},
		},
		{
			name: "single member cluster",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
			},
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 1},
			},
			expectedLeader: &etcdserverpb.Member{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
		},
		{
			name: "member status error",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
			},
			errors: map[uint64]error{
				1: errors.New("connection failed"),
			},
			expectedError: "failed to get member status while finding leader: connection failed",
		},
		{
			name: "inconsistent leader reported",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
				{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}},
			},
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 2},
				2: {Header: &etcdserverpb.ResponseHeader{MemberId: 2}, Leader: 2},
				3: {Header: &etcdserverpb.ResponseHeader{MemberId: 3}, Leader: 3},
			},
			expectedError: "inconsistent leader reported by different members: [2] vs. [3]",
		},
		{
			name: "leader not in member list",
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}},
			},
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 3},
				2: {Header: &etcdserverpb.ResponseHeader{MemberId: 2}, Leader: 3},
			},
			expectedLeader: nil, // Leader ID 3 is not in the member list
		},
		{
			name:           "empty member list",
			memberList:     []*etcdserverpb.Member{},
			expectedLeader: nil,
		},
		{
			name:           "nil member list",
			memberList:     nil,
			expectedLeader: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockMemberStatusChecker{
				memberStatuses: tt.memberStatuses,
				errors:         tt.errors,
			}

			leader, err := FindLeader(ctx, mockClient, tt.memberList)

			if tt.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
				return
			}

			require.NoError(t, err)
			if tt.expectedLeader == nil {
				assert.Nil(t, leader)
			} else {
				require.NotNil(t, leader)
				assert.Equal(t, tt.expectedLeader.ID, leader.ID)
				assert.Equal(t, tt.expectedLeader.Name, leader.Name)
			}
		})
	}
}

func TestMoveLeaderToAnotherMember(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name             string
		leader           *etcdserverpb.Member
		memberList       []*etcdserverpb.Member
		moveLeaderErrors map[uint64]error
		expectedSuccess  bool
		expectedError    string
	}{
		{
			name: "successfully move leader to another member",
			leader: &etcdserverpb.Member{
				ID:         1,
				Name:       "etcd-1",
				ClientURLs: []string{"https://10.0.0.1:2379"},
				PeerURLs:   []string{"https://10.0.0.1:2380"},
			},
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}, PeerURLs: []string{"https://10.0.0.2:2380"}},
				{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}, PeerURLs: []string{"https://10.0.0.3:2380"}},
			},
			expectedSuccess: true,
		},
		{
			name: "successfully move leader when leader is in middle of list",
			leader: &etcdserverpb.Member{
				ID:         2,
				Name:       "etcd-2",
				ClientURLs: []string{"https://10.0.0.2:2379"},
				PeerURLs:   []string{"https://10.0.0.2:2380"},
			},
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}, PeerURLs: []string{"https://10.0.0.2:2380"}},
				{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}, PeerURLs: []string{"https://10.0.0.3:2380"}},
			},
			expectedSuccess: true,
		},
		{
			name: "successfully move leader when leader is last in list",
			leader: &etcdserverpb.Member{
				ID:         3,
				Name:       "etcd-3",
				ClientURLs: []string{"https://10.0.0.3:2379"},
				PeerURLs:   []string{"https://10.0.0.3:2380"},
			},
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}, PeerURLs: []string{"https://10.0.0.2:2380"}},
				{ID: 3, Name: "etcd-3", ClientURLs: []string{"https://10.0.0.3:2379"}, PeerURLs: []string{"https://10.0.0.3:2380"}},
			},
			expectedSuccess: true,
		},
		{
			name: "move leader fails with error",
			leader: &etcdserverpb.Member{
				ID:         1,
				Name:       "etcd-1",
				ClientURLs: []string{"https://10.0.0.1:2379"},
				PeerURLs:   []string{"https://10.0.0.1:2380"},
			},
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
				{ID: 2, Name: "etcd-2", ClientURLs: []string{"https://10.0.0.2:2379"}, PeerURLs: []string{"https://10.0.0.2:2380"}},
			},
			moveLeaderErrors: map[uint64]error{
				2: errors.New("move leader failed: connection timeout"),
			},
			expectedSuccess: false,
			expectedError:   "move leader failed: connection timeout",
		},
		{
			name: "no follower member found - single member cluster",
			leader: &etcdserverpb.Member{
				ID:         1,
				Name:       "etcd-1",
				ClientURLs: []string{"https://10.0.0.1:2379"},
				PeerURLs:   []string{"https://10.0.0.1:2380"},
			},
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
			},
			expectedSuccess: false,
			expectedError:   "no follower member found for the members: [ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2379\" ]",
		},
		{
			name: "no follower member found - all members have same ID",
			leader: &etcdserverpb.Member{
				ID:         1,
				Name:       "etcd-1",
				ClientURLs: []string{"https://10.0.0.1:2379"},
				PeerURLs:   []string{"https://10.0.0.1:2380"},
			},
			memberList: []*etcdserverpb.Member{
				{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
				{ID: 1, Name: "etcd-1-copy", ClientURLs: []string{"https://10.0.0.1:2379"}, PeerURLs: []string{"https://10.0.0.1:2380"}},
			},
			expectedSuccess: false,
			expectedError:   "no follower member found for the members: [ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2379\"  ID:1 name:\"etcd-1-copy\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2379\" ]",
		},
		{
			name: "empty member list",
			leader: &etcdserverpb.Member{
				ID:         1,
				Name:       "etcd-1",
				ClientURLs: []string{"https://10.0.0.1:2379"},
				PeerURLs:   []string{"https://10.0.0.1:2380"},
			},
			memberList:      []*etcdserverpb.Member{},
			expectedSuccess: false,
			expectedError:   "no follower member found for the members: []",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockLeaderMover{
				moveLeaderErrors: tt.moveLeaderErrors,
			}

			success, err := MoveLeaderToAnotherMember(ctx, mockClient, tt.leader, tt.memberList)

			if tt.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
				assert.False(t, success)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedSuccess, success)
		})
	}
}
