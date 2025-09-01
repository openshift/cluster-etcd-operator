package ceohelpers

import (
	"context"
	"fmt"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"k8s.io/klog/v2"
)

func FindLeader(ctx context.Context, client etcdcli.MemberStatusChecker, memberList []*etcdserverpb.Member) (*etcdserverpb.Member, error) {
	var leaderId uint64
	var mx *etcdserverpb.Member
	for _, member := range memberList {
		status, err := client.MemberStatus(ctx, member)
		if err != nil {
			return nil, fmt.Errorf("failed to get member status while finding leader: %w", err)
		}
		if leaderId != 0 && leaderId != status.Leader {
			return nil, fmt.Errorf("inconsistent leader reported by different members: [%x] vs. [%x]", leaderId, status.Leader)
		}
		leaderId = status.Leader
		if member.ID == leaderId {
			mx = member
		}
	}
	return mx, nil
}

func MoveLeaderToAnotherMember(ctx context.Context, client etcdcli.LeaderMover, leader *etcdserverpb.Member,
	memberList []*etcdserverpb.Member) (bool, error) {
	var otherMember *etcdserverpb.Member
	for _, member := range memberList {
		if member.ID != leader.ID {
			otherMember = member
			break
		}
	}

	if otherMember == nil {
		return false, fmt.Errorf("no follower member found for leadership transfer: %v", memberList)
	}

	err := client.MoveLeader(ctx, otherMember.ID)
	if err != nil {
		return false, err
	}

	klog.Warningf("Moved lead from member [%x] (%s) to [%x] (%s) successfully!", leader.ID, leader.GetClientURLs()[0], otherMember.ID, otherMember.GetClientURLs()[0])
	return true, nil
}
