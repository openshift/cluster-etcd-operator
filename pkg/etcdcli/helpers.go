package etcdcli

import (
	"context"
	"fmt"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeEtcdClient struct {
	members []*etcdserverpb.Member
	opts    *FakeClientOptions
}

func (f *fakeEtcdClient) GetObjectCounts(ctx context.Context, prefixes []string) (map[string]int64, error) {
	panic("implement me")
}

func (f *fakeEtcdClient) Defragment(ctx context.Context, member *etcdserverpb.Member) (*clientv3.DefragmentResponse, error) {
	// dramatic simplification
	f.opts.dbSize = f.opts.dbSizeInUse
	return nil, nil
}

func (f *fakeEtcdClient) Status(ctx context.Context, target string) (*clientv3.StatusResponse, error) {
	for _, member := range f.members {
		if member.ClientURLs[0] == target {
			for _, status := range f.opts.status {
				if status.Header.MemberId == member.ID {
					return status, nil
				}
			}
			return nil, fmt.Errorf("no status found for member %d matching target %q.", member.ID, target)
		}
	}
	return nil, fmt.Errorf("status failed no match for target: %q", target)
}

func (f *fakeEtcdClient) MemberAdd(ctx context.Context, peerURL string) error {
	panic("implement me")
}

func (f *fakeEtcdClient) MemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	return f.members, nil
}

func (f *fakeEtcdClient) MemberRemove(ctx context.Context, memberID uint64) error {
	var memberExists bool
	for _, m := range f.members {
		if m.ID == memberID {
			memberExists = true
			break
		}
	}
	if !memberExists {
		return fmt.Errorf("member with the given ID: %d doesn't exist", memberID)
	}

	var newMemberList []*etcdserverpb.Member
	for _, m := range f.members {
		if m.ID == memberID {
			continue
		}
		newMemberList = append(newMemberList, m)
	}
	f.members = newMemberList
	return nil
}

func (f *fakeEtcdClient) MemberHealth(ctx context.Context) (memberHealth, error) {
	var healthy, unhealthy int
	var memberHealth memberHealth
	for _, member := range f.members {
		healthCheck := healthCheck{
			Member: member,
		}
		switch {
		// if WithClusterHealth is not passed we default to all healthy
		case f.opts.healthyMember == 0 && f.opts.unhealthyMember == 0:
			healthCheck.Healthy = true
			break
		case f.opts.healthyMember > 0 && healthy < f.opts.healthyMember:
			healthCheck.Healthy = true
			healthy++
			break
		case f.opts.unhealthyMember > 0 && unhealthy < f.opts.unhealthyMember:
			healthCheck.Healthy = false
			unhealthy++
			break
		}
		memberHealth = append(memberHealth, healthCheck)
	}
	return memberHealth, nil
}

//IsMemberHealthy returns true if the number of etcd members equals the member of healthy members.
func (f *fakeEtcdClient) IsMemberHealthy(ctx context.Context, member *etcdserverpb.Member) (bool, error) {
	return len(f.members) == f.opts.healthyMember, nil
}

func (f *fakeEtcdClient) UnhealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if f.opts.unhealthyMember > 0 {
		// unheathy start from beginning
		return f.members[0:f.opts.unhealthyMember], nil
	}
	return []*etcdserverpb.Member{}, nil
}

func (f *fakeEtcdClient) HealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if f.opts.healthyMember > 0 {
		// healthy start from end
		return f.members[f.opts.unhealthyMember:], nil
	}
	return []*etcdserverpb.Member{}, nil
}

func (f *fakeEtcdClient) MemberStatus(ctx context.Context, member *etcdserverpb.Member) string {
	panic("implement me")
}

func (f *fakeEtcdClient) GetMember(ctx context.Context, name string) (*etcdserverpb.Member, error) {
	for _, m := range f.members {
		if m.Name == name {
			return m, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "etcd.operator.openshift.io", Resource: "etcdmembers"}, name)
}

func (f *fakeEtcdClient) MemberUpdatePeerURL(ctx context.Context, id uint64, peerURL []string) error {
	panic("implement me")
}

func NewFakeEtcdClient(members []*etcdserverpb.Member, opts ...FakeClientOption) (EtcdClient, error) {
	status := make([]*clientv3.StatusResponse, len(members))
	fakeEtcdClient := &fakeEtcdClient{
		members: members,
		opts: &FakeClientOptions{
			status: status,
		},
	}
	if opts != nil {
		fcOpts := newFakeClientOpts(opts...)
		switch {
		// validate WithClusterHealth
		case fcOpts.healthyMember > 0 || fcOpts.unhealthyMember > 0:
			if fcOpts.healthyMember+fcOpts.unhealthyMember != len(members) {
				return nil, fmt.Errorf("WithClusterHealth count must equal the numer of members: have %d, want %d ", fcOpts.unhealthyMember+fcOpts.healthyMember, len(members))
			}
		}
		fakeEtcdClient.opts = fcOpts
	}

	return fakeEtcdClient, nil
}

type FakeClientOptions struct {
	client          *clientv3.Client
	unhealthyMember int
	healthyMember   int
	status          []*clientv3.StatusResponse
	dbSize          int64
	dbSizeInUse     int64
}

func newFakeClientOpts(opts ...FakeClientOption) *FakeClientOptions {
	fcOpts := &FakeClientOptions{}
	fcOpts.applyFakeOpts(opts)
	fcOpts.validateFakeOpts(opts)
	return fcOpts
}

func (fo *FakeClientOptions) applyFakeOpts(opts []FakeClientOption) {
	for _, opt := range opts {
		opt(fo)
	}
}

func (fo *FakeClientOptions) validateFakeOpts(opts []FakeClientOption) {
	for _, opt := range opts {
		opt(fo)
	}
}

type FakeClientOption func(*FakeClientOptions)

type FakeMemberHealth struct {
	Healthy   int
	Unhealthy int
}

func WithFakeClusterHealth(members *FakeMemberHealth) FakeClientOption {
	return func(fo *FakeClientOptions) {
		fo.unhealthyMember = members.Unhealthy
		fo.healthyMember = members.Healthy
	}
}

func WithFakeStatus(status []*clientv3.StatusResponse) FakeClientOption {
	return func(fo *FakeClientOptions) {
		fo.status = status
	}
}
