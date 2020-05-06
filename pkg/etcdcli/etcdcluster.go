package etcdcli

import (
	"context"
	"sync"

	"go.etcd.io/etcd/etcdserver/etcdserverpb"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog"
)

var _ EtcdCluster = &cluster{}

func NewEtcdCluster(client EtcdClient) *cluster {
	return &cluster{
		client:        client,
		members:       map[uint64]*member{},
		membersByName: map[string]*member{},
	}
}

type cluster struct {
	client EtcdClient

	l             sync.RWMutex
	members       map[uint64]*member
	membersByName map[string]*member
}

type member struct {
	*etcdserverpb.Member
	Status string
}

// TODO: refresh on an interval and/or in response to etcd watch events
func (c *cluster) Refresh() error {
	c.l.Lock()
	defer c.l.Unlock()

	// TODO: record an event, increment a prometheus counter

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := c.client.MemberList(ctx)
	if err != nil {
		return err
	}

	// TODO: parallelize, timeouts cause requests to stack up
	for i := range membersResp.Members {
		latest := membersResp.Members[i]

		var status string
		switch {
		case len(latest.ClientURLs) == 0:
			status = EtcdMemberStatusNotStarted
		case len(latest.ClientURLs) > 0:
			if _, err := c.client.Status(ctx, latest.ClientURLs[0]); err != nil {
				status = EtcdMemberStatusAvailable
			} else {
				status = EtcdMemberStatusUnhealthy
			}
		default:
			status = EtcdMemberStatusUnknown
		}

		// TODO: record and event and/or prometheus stat for latest status
		m := &member{Member: latest, Status: status}
		c.members[m.ID] = m
		if len(m.Name) > 0 {
			c.membersByName[m.Name] = m
		}
	}

	klog.V(0).Infof("refreshed etcd cluster cache")
	return nil
}

func (c *cluster) GetMember(name string) (*etcdserverpb.Member, error) {
	c.l.RLock()
	defer c.l.RUnlock()

	if m, found := c.membersByName[name]; found {
		return m.Member, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "etcd.operator.openshift.io", Resource: "etcdmembers"}, name)
}

func (c *cluster) MemberUpdatePeerURL(id uint64, peerURLs []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := c.client.MemberUpdate(ctx, id, peerURLs); err != nil {
		return err
	}
	return c.Refresh()
}

func (c *cluster) MemberAdd(peerURL string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := c.client.MemberAdd(ctx, []string{peerURL}); err != nil {
		return err
	}
	return c.Refresh()
}

func (c *cluster) MemberRemove(member string) error {
	m, err := c.GetMember(member)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		} else {
			return err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.client.MemberRemove(ctx, m.ID); err != nil {
		return err
	}
	return c.Refresh()
}

func (c *cluster) MemberList() ([]*etcdserverpb.Member, error) {
	c.l.RLock()
	defer c.l.RUnlock()

	var list []*etcdserverpb.Member
	for k := range c.members {
		list = append(list, c.members[k].Member)
	}
	return list, nil
}

func (c *cluster) MemberStatus(member *etcdserverpb.Member) string {
	c.l.RLock()
	defer c.l.RUnlock()

	m, found := c.members[member.ID]
	if !found {
		return EtcdMemberStatusUnknown
	}
	return m.Status
}

func (c *cluster) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	c.l.RLock()
	defer c.l.RUnlock()

	unhealthyMembers := []*etcdserverpb.Member{}
	for i := range c.members {
		member := c.members[i]
		if member.Status != EtcdMemberStatusAvailable {
			unhealthyMembers = append(unhealthyMembers, member.Member)
		}
	}

	return unhealthyMembers, nil
}
