package etcdcli

import (
	"context"

	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeEtcdClient struct {
	members []*etcdserverpb.Member
}

func (f *fakeEtcdClient) EndpointStatus(ctx context.Context, member *etcdserverpb.Member) (*clientv3.StatusResponse, error) {
	panic("implement me")
}

func (f *fakeEtcdClient) MemberAdd(peerURL string) error {
	panic("implement me")
}

func (f *fakeEtcdClient) MemberList() ([]*etcdserverpb.Member, error) {
	return f.members, nil
}

func (f *fakeEtcdClient) MemberRemove(member string) error {
	panic("implement me")
}

func (f *fakeEtcdClient) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	return []*etcdserverpb.Member{}, nil
}

func (f *fakeEtcdClient) MemberStatus(member *etcdserverpb.Member) string {
	panic("implement me")
}

func (f *fakeEtcdClient) GetMember(name string) (*etcdserverpb.Member, error) {
	for _, m := range f.members {
		if m.Name == name {
			return m, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "etcd.operator.openshift.io", Resource: "etcdmembers"}, name)
}

func (f *fakeEtcdClient) MemberUpdatePeerURL(id uint64, peerURL []string) error {
	panic("implement me")
}

func NewFakeEtcdClient(members []*etcdserverpb.Member) EtcdClient {
	return &fakeEtcdClient{members: members}
}
