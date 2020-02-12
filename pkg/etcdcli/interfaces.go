package etcdcli

import (
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

type EtcdClient interface {
	MemberAdder
	MemberLister
	MemberRemover
	UnhealthyMemberLister
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
