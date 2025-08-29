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

func TestFindLeader_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	mockClient := &mockMemberStatusChecker{
		memberStatuses: map[uint64]*clientv3.StatusResponse{
			1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 1},
		},
	}

	memberList := []*etcdserverpb.Member{
		{ID: 1, Name: "etcd-1", ClientURLs: []string{"https://10.0.0.1:2379"}},
	}

	// The function should handle context cancellation gracefully
	// Since our mock doesn't actually check context, this test verifies
	// that the function signature accepts context and passes it through
	leader, err := FindLeader(ctx, mockClient, memberList)
	require.NoError(t, err)
	require.NotNil(t, leader)
	assert.Equal(t, uint64(1), leader.ID)
}

func TestFindLeader_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("member with no client URLs", func(t *testing.T) {
		mockClient := &mockMemberStatusChecker{
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 1},
			},
		}

		memberList := []*etcdserverpb.Member{
			{ID: 1, Name: "etcd-1", ClientURLs: []string{}},
		}

		// This should work fine since our mock doesn't actually use ClientURLs
		leader, err := FindLeader(ctx, mockClient, memberList)
		require.NoError(t, err)
		require.NotNil(t, leader)
		assert.Equal(t, uint64(1), leader.ID)
	})

	t.Run("member with nil client URLs", func(t *testing.T) {
		mockClient := &mockMemberStatusChecker{
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				1: {Header: &etcdserverpb.ResponseHeader{MemberId: 1}, Leader: 1},
			},
		}

		memberList := []*etcdserverpb.Member{
			{ID: 1, Name: "etcd-1", ClientURLs: nil},
		}

		leader, err := FindLeader(ctx, mockClient, memberList)
		require.NoError(t, err)
		require.NotNil(t, leader)
		assert.Equal(t, uint64(1), leader.ID)
	})

	t.Run("member with zero ID", func(t *testing.T) {
		mockClient := &mockMemberStatusChecker{
			memberStatuses: map[uint64]*clientv3.StatusResponse{
				0: {Header: &etcdserverpb.ResponseHeader{MemberId: 0}, Leader: 0},
			},
		}

		memberList := []*etcdserverpb.Member{
			{ID: 0, Name: "etcd-0", ClientURLs: []string{"https://10.0.0.0:2379"}},
		}

		leader, err := FindLeader(ctx, mockClient, memberList)
		require.NoError(t, err)
		require.NotNil(t, leader)
		assert.Equal(t, uint64(0), leader.ID)
	})
}
