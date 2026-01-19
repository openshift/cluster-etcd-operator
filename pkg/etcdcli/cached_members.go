package etcdcli

import (
	"context"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"sync"
	"time"
)

const refreshPeriod = time.Minute

type CachedMembers struct {
	client          EtcdClient
	lock            sync.Mutex
	members         memberHealth
	refreshInterval time.Duration
	lastRefresh     time.Time
}

func NewMemberCache(client EtcdClient) AllMemberLister {
	return newMemberCache(client, refreshPeriod)
}

func newMemberCache(client EtcdClient, refreshInterval time.Duration) AllMemberLister {
	return &CachedMembers{
		client:          client,
		lock:            sync.Mutex{},
		refreshInterval: refreshInterval,
		lastRefresh:     time.UnixMilli(0),
	}
}

func (c *CachedMembers) shouldRefresh() bool {
	return c.lastRefresh.Add(c.refreshInterval).Before(time.Now())
}

func (c *CachedMembers) refresh(ctx context.Context) error {
	if c.shouldRefresh() {
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
	var members []*etcdserverpb.Member
	for _, member := range c.members {
		members = append(members, member.Member)
	}
	return members
}

func (c *CachedMembers) MemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return c.convertToProtoMemberList(), nil
}

func (c *CachedMembers) VotingMemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	list := c.convertToProtoMemberList()
	return filterVotingMembers(list), nil
}

func (c *CachedMembers) HealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return c.members.GetHealthyMembers(), nil
}

func (c *CachedMembers) HealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return filterVotingMembers(c.members.GetHealthyMembers()), nil
}

func (c *CachedMembers) UnhealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return c.members.GetUnhealthyMembers(), nil
}

func (c *CachedMembers) UnhealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	return filterVotingMembers(c.members.GetUnhealthyMembers()), nil
}

func (c *CachedMembers) MemberHealth(ctx context.Context) (memberHealth, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	var health memberHealth
	for _, m := range c.members {
		health = append(health, m)
	}
	return health, nil
}
