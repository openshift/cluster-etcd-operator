package ceohelpers

import (
	"context"
	"fmt"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
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
