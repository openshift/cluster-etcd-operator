package etcdcli

import (
	"context"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"sync"
	"time"
)

const refreshPeriod = time.Minute

type CachedMembers struct {
	client  EtcdClient
	lock    sync.RWMutex
	members memberHealth

	lastRefresh time.Time
}

func NewMemberCache(client EtcdClient) AllMemberLister {
	return &CachedMembers{
		client:      client,
		lock:        sync.RWMutex{},
		lastRefresh: time.UnixMilli(0),
	}
}

func (c *CachedMembers) refresh(ctx context.Context) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.lastRefresh.Add(refreshPeriod).Before(time.Now()) {
		health, err := c.client.MemberHealth(ctx)
		if err != nil {
			return err
		}

		c.members = health
		c.lastRefresh = time.Now()
	}

	return nil
}

func (c *CachedMembers) convertToProtoMemberList() []*etcdserverpb.Member {
	c.lock.RLock()
	defer c.lock.RUnlock()

	var members []*etcdserverpb.Member
	for _, member := range c.members {
		members = append(members, member.Member)
	}
	return members
}

func (c *CachedMembers) MemberHealth(ctx context.Context) (memberHealth, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.members, nil
}

func (c *CachedMembers) MemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	return c.convertToProtoMemberList(), nil
}

func (c *CachedMembers) VotingMemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	list := c.convertToProtoMemberList()
	return filterVotingMembers(list), nil
}

func (c *CachedMembers) HealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.members.GetHealthyMembers(), nil
}

func (c *CachedMembers) HealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	return filterVotingMembers(c.members.GetHealthyMembers()), nil
}

func (c *CachedMembers) UnhealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.members.GetUnhealthyMembers(), nil
}

func (c *CachedMembers) UnhealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	return filterVotingMembers(c.members.GetUnhealthyMembers()), nil
}
