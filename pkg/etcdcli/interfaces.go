package etcdcli

import (
	"context"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	EtcdMemberStatusAvailable  = "EtcdMemberAvailable"
	EtcdMemberStatusNotStarted = "EtcdMemberNotStarted"
	EtcdMemberStatusUnhealthy  = "EtcdMemberUnhealthy"
	EtcdMemberStatusUnknown    = "EtcdMemberUnknown"
)

type EtcdClient interface {
	Defragment
	MemberAdder
	MemberHealth
	IsMemberHealthy
	MemberLister
	MemberRemover
	HealthyMemberLister
	UnhealthyMemberLister
	MemberStatusChecker
	Status

	GetMember(ctx context.Context, name string) (*etcdserverpb.Member, error)
	MemberUpdatePeerURL(ctx context.Context, id uint64, peerURL []string) error
}

type Defragment interface {
	Defragment(ctx context.Context, member *etcdserverpb.Member) (*clientv3.DefragmentResponse, error)
}

type Status interface {
	Status(ctx context.Context, target string) (*clientv3.StatusResponse, error)
}

type MemberAdder interface {
	MemberAdd(ctx context.Context, peerURL string) error
}

type MemberHealth interface {
	MemberHealth(ctx context.Context) (memberHealth, error)
}
type IsMemberHealthy interface {
	IsMemberHealthy(member *etcdserverpb.Member) (bool, error)
}
type MemberRemover interface {
	MemberRemove(ctx context.Context, member string) error
}

type MemberLister interface {
	MemberList(ctx context.Context) ([]*etcdserverpb.Member, error)
}

type HealthyMemberLister interface {
	HealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error)
}

type UnhealthyMemberLister interface {
	UnhealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error)
}

type MemberStatusChecker interface {
	MemberStatus(ctx context.Context, member *etcdserverpb.Member) string
}
