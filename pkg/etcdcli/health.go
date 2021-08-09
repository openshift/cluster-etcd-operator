package etcdcli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
)

func init() {
	legacyregistry.RawMustRegister(raftTerms)
}

const raftTermsMetricName = "etcd_debugging_raft_terms_total"

var raftTerms = &raftTermsCollector{
	desc: prometheus.NewDesc(
		raftTermsMetricName,
		"Number of etcd raft terms as observed by each member.",
		[]string{"member"},
		prometheus.Labels{},
	),
	terms: map[string]uint64{},
	lock:  sync.RWMutex{},
}

type healthCheck struct {
	Member  *etcdserverpb.Member
	Healthy bool
	Took    string
	Error   error
}

type memberHealth []healthCheck

func getMemberHealth(etcdMembers []*etcdserverpb.Member) memberHealth {
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
			cli, err := getEtcdClientWithClientOpts([]string{member.ClientURLs[0]})
			if err != nil {
				hch <- healthCheck{Member: member, Healthy: false, Error: fmt.Errorf("create client failure: %w", err)}
				return
			}
			defer cli.Close()
			st := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			// linearized request to verify health of member
			resp, err := cli.Get(ctx, "health")
			cancel()
			hc := healthCheck{Member: member, Healthy: false, Took: time.Since(st).String()}
			if err == nil {
				if resp.Header != nil {
					raftTerms.Set(member.Name, resp.Header.RaftTerm)
				}
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

	// Purge any unknown members from the raft term metrics collector.
	for _, cachedMember := range raftTerms.List() {
		found := false
		for _, member := range etcdMembers {
			if member.Name == cachedMember {
				found = true
				break
			}
		}
		if !found {
			raftTerms.Forget(cachedMember)
		}
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

// IsQuorumFaultTolerant checks the current etcd cluster and returns true if the cluster can tolerate the
// loss of a single etcd member. Such loss is common during new static pod revision.
func IsQuorumFaultTolerant(memberHealth []healthCheck) bool {
	totalMembers := len(memberHealth)
	quorum := totalMembers/2 + 1
	healthyMembers := len(GetHealthyMemberNames(memberHealth))
	switch {
	case totalMembers-quorum < 1:
		klog.Errorf("etcd cluster has quorum of %d which is not fault tolerant: %+v", quorum, memberHealth)
		return false
	case healthyMembers-quorum < 1:
		klog.Errorf("etcd cluster has quorum of %d and %d healthy members which is not fault tolerant: %+v", quorum, healthyMembers, memberHealth)
		return false
	}
	return true
}

func IsClusterHealthy(memberHealth memberHealth) bool {
	unhealthyMembers := memberHealth.GetUnhealthyMembers()
	if len(unhealthyMembers) > 0 {
		return false
	}
	return true
}

// raftTermsCollector is a Prometheus collector to re-expose raft terms as a counter.
type raftTermsCollector struct {
	desc  *prometheus.Desc
	terms map[string]uint64
	lock  sync.RWMutex
}

func (c *raftTermsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *raftTermsCollector) Set(member string, value uint64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.terms[member] = value
}

func (c *raftTermsCollector) Forget(member string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.terms, member)
}

func (c *raftTermsCollector) List() []string {
	c.lock.RLock()
	defer c.lock.RUnlock()
	var members []string
	for member := range c.terms {
		members = append(members, member)
	}
	return members
}

func (c *raftTermsCollector) Collect(ch chan<- prometheus.Metric) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	for member, val := range c.terms {
		ch <- prometheus.MustNewConstMetric(
			c.desc,
			prometheus.CounterValue,
			float64(val),
			member,
		)
	}
}
