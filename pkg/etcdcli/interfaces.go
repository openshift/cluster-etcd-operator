package etcdcli

import (
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

const (
	EtcdMemberStatusAvailable  = "EtcdMemberAvailable"
	EtcdMemberStatusNotStarted = "EtcdMemberNotStarted"
	EtcdMemberStatusUnhealthy  = "EtcdMemberUnhealthy"
	EtcdMemberStatusUnknown    = "EtcdMemberUnknown"
)

type EtcdClient interface {
	clientv3.Cluster
	clientv3.Maintenance
}

type EtcdCluster interface {
	MemberAdder
	MemberLister
	MemberRemover
	UnhealthyMemberLister
	MemberStatusChecker

	GetMember(name string) (*etcdserverpb.Member, error)
	MemberUpdatePeerURL(id uint64, peerURL []string) error

	Refresh() error
}

type MemberAdder interface {
	MemberAdd(peerURL string) error
}

type MemberRemover interface {
	MemberRemove(member string) error
}

type MemberLister interface {
	MemberList() ([]*etcdserverpb.Member, error)
}

type UnhealthyMemberLister interface {
	UnhealthyMembers() ([]*etcdserverpb.Member, error)
}

type MemberStatusChecker interface {
	MemberStatus(member *etcdserverpb.Member) string
}
