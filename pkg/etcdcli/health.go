package etcdcli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.etcd.io/etcd/etcdserver/api/v3rpc/rpctypes"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

type healthCheck struct {
	Member  *etcdserverpb.Member
	Healthy bool
	Took    string
	Error   error
}

type memberHealth []healthCheck

func GetMemberHealth(etcdMembers []*etcdserverpb.Member) memberHealth {
	var wg sync.WaitGroup
	memberHealth := memberHealth{}
	hch := make(chan healthCheck, len(etcdMembers))
	for _, member := range etcdMembers {
		if !HasStarted(member) {
			memberHealth = append(memberHealth, healthCheck{Member: member, Healthy: false})
			continue
		}
		wg.Add(1)
		go func(member *etcdserverpb.Member) {
			defer wg.Done()
			// new client vs shared is used here to minimize disruption of cached client consumers.
			// performance analisis of CPU and RSS consumption showed net gain after refactor
			cli, err := getEtcdClient([]string{member.ClientURLs[0]})
			if err != nil {
				hch <- healthCheck{Member: member, Healthy: false, Error: fmt.Errorf("create client failure: %w", err)}
				return
			}
			defer cli.Close()
			st := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			// linearized request to verify health of member
			_, err = cli.Get(ctx, "health")
			cancel()
			hc := healthCheck{Member: member, Healthy: false, Took: time.Since(st).String()}
			if err == nil || err == rpctypes.ErrPermissionDenied {
				hc.Healthy = true
			} else {
				hc.Error = fmt.Errorf("health check failed: %w", err)
			}
			hch <- hc
		}(member)
	}

	wg.Wait()
	close(hch)

	for healthCheck := range hch {
		memberHealth = append(memberHealth, healthCheck)
	}

	return memberHealth
}

// Status returns a reporting of memberHealth status
func (h memberHealth) Status() string {
	healthyMembers := h.GetHealthyMembers()

	status := []string{}
	if len(h) == len(healthyMembers) {
		status = append(status, fmt.Sprintf("%d members are available", len(h)))
	} else {
		status = append(status, fmt.Sprintf("%d of %d members are available", len(healthyMembers), len(h)))
		for _, etcd := range h {
			switch {
			case !HasStarted(etcd.Member):
				status = append(status, fmt.Sprintf("%s has not started", GetMemberNameOrHost(etcd.Member)))
				break
			case !etcd.Healthy:
				status = append(status, fmt.Sprintf("%s is unhealthy", etcd.Member.Name))
				break
			}
		}
	}
	return strings.Join(status, ", ")
}

// GetHealthyMembers returns healthy members
func (h memberHealth) GetHealthyMembers() []*etcdserverpb.Member {
	members := []*etcdserverpb.Member{}
	for _, etcd := range h {
		if etcd.Healthy {
			members = append(members, etcd.Member)
		}
	}
	return members
}

// GetUnhealthy returns unhealthy members
func (h memberHealth) GetUnhealthyMembers() []*etcdserverpb.Member {
	members := []*etcdserverpb.Member{}
	for _, etcd := range h {
		if !etcd.Healthy {
			members = append(members, etcd.Member)
		}
	}
	return members
}

// GetUnstarted returns unstarted members
func (h memberHealth) GetUnstartedMembers() []*etcdserverpb.Member {
	members := []*etcdserverpb.Member{}
	for _, etcd := range h {
		if !HasStarted(etcd.Member) {
			members = append(members, etcd.Member)
		}
	}
	return members
}

// GetUnhealthyMemberNames returns a list of unhealthy member names
func GetUnhealthyMemberNames(memberHealth []healthCheck) []string {
	memberNames := []string{}
	for _, etcd := range memberHealth {
		if !etcd.Healthy {
			memberNames = append(memberNames, GetMemberNameOrHost(etcd.Member))
		}
	}
	return memberNames
}

// GetHealthyMemberNames returns a list of healthy member names
func GetHealthyMemberNames(memberHealth []healthCheck) []string {
	memberNames := []string{}
	for _, etcd := range memberHealth {
		if etcd.Healthy {
			memberNames = append(memberNames, etcd.Member.Name)
		}
	}
	return memberNames
}

// GetUnstartedMemberNames returns a list of unstarted member names
func GetUnstartedMemberNames(memberHealth []healthCheck) []string {
	memberNames := []string{}
	for _, etcd := range memberHealth {
		if !HasStarted(etcd.Member) {
			memberNames = append(memberNames, GetMemberNameOrHost(etcd.Member))
		}
	}
	return memberNames
}

// HasStarted return true if etcd member has started.
func HasStarted(member *etcdserverpb.Member) bool {
	if len(member.ClientURLs) == 0 {
		return false
	}
	return true
}
