package etcdcli

import (
	"context"
	"io"

	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

type fakeEtcdClient struct {
	members []*etcdserverpb.Member
}

func NewFakeEtcdClient(members []*etcdserverpb.Member) EtcdClient {
	return &fakeEtcdClient{members: members}
}

func (c *fakeEtcdClient) MemberList(ctx context.Context) (*clientv3.MemberListResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) MemberAdd(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) MemberAddAsLearner(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) MemberRemove(ctx context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) MemberUpdate(ctx context.Context, id uint64, peerAddrs []string) (*clientv3.MemberUpdateResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) MemberPromote(ctx context.Context, id uint64) (*clientv3.MemberPromoteResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) AlarmList(ctx context.Context) (*clientv3.AlarmResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) AlarmDisarm(ctx context.Context, m *clientv3.AlarmMember) (*clientv3.AlarmResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) Defragment(ctx context.Context, endpoint string) (*clientv3.DefragmentResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) HashKV(ctx context.Context, endpoint string, rev int64) (*clientv3.HashKVResponse, error) {
	return nil, nil
}

func (c *fakeEtcdClient) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func (c *fakeEtcdClient) MoveLeader(ctx context.Context, transfereeID uint64) (*clientv3.MoveLeaderResponse, error) {
	return nil, nil
}
