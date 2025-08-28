package etcdcli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/component-base/metrics/legacyregistry"
	klog "k8s.io/klog/v2"
)

func init() {
	legacyregistry.RawMustRegister(raftTerms)
}

const raftTermsMetricName = "etcd_debugging_raft_terms_total"

// raftTermsCollector is thread-safe internally
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

func GetMemberHealth(ctx context.Context, clipool *EtcdClientPool, etcdMembers []*etcdserverpb.Member) memberHealth {
	// while we don't explicitly mention that the returned ordering has to be the same as etcdMembers,
	// we try to keep it that way for backward compatibility reasons
	memberHealth := make([]healthCheck, len(etcdMembers))
	wg := sync.WaitGroup{}
	for i, member := range etcdMembers {
		if !HasStarted(member) {
			memberHealth[i] = healthCheck{Member: member, Healthy: false}
			continue
		}

		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			cli, err := clipool.Get()
			if err != nil {
				memberHealth[i] = healthCheck{Member: member, Healthy: false, Error: err}
				return
			}
			defer clipool.Return(cli)

			// Create an independent timeout context for each member health check
			// This prevents one slow member from affecting other members' health checks
			memberCtx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
			defer cancel()

			memberHealth[i] = checkSingleMemberHealth(memberCtx, cli, member)
		}(i)
	}

	wg.Wait()

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
			// Forget is a map deletion underneath, which is idempotent and under a lock.
			raftTerms.Forget(cachedMember)
		}
	}

	return memberHealth
}

func checkSingleMemberHealth(ctx context.Context, cli *clientv3.Client, member *etcdserverpb.Member) healthCheck {
	// we only want to check that one member that was passed, we're restoring the original client endpoints at the end
	defer cli.SetEndpoints(cli.Endpoints()...)
	cli.SetEndpoints(member.ClientURLs...)

	st := time.Now()
	var err error
	var resp *clientv3.GetResponse
	if member.IsLearner {
		// Learner members only support serializable (without consensus) read requests
		resp, err = cli.Get(ctx, "health", clientv3.WithSerializable())
	} else {
		// Linearized request to verify health of a voting member
		resp, err = cli.Get(ctx, "health")
	}

	hc := healthCheck{Member: member, Healthy: false, Took: time.Since(st).String()}
	if err == nil {
		if resp.Header != nil {
			// TODO(thomas): this is a somewhat misplaced side-effect that is safe to call from multiple goroutines
			raftTerms.Set(member.Name, resp.Header.RaftTerm)
		}
		hc.Healthy = true
	} else {
		klog.Errorf("health check for member (%v) failed: err(%v)", member.Name, err)
		hc.Error = fmt.Errorf("health check failed: %w", err)
	}

	return hc
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
	var members []*etcdserverpb.Member
	for _, etcd := range h {
		if etcd.Healthy {
			members = append(members, etcd.Member)
		}
	}
	return members
}

// GetUnhealthy returns unhealthy members
func (h memberHealth) GetUnhealthyMembers() []*etcdserverpb.Member {
	var members []*etcdserverpb.Member
	for _, etcd := range h {
		if !etcd.Healthy {
			members = append(members, etcd.Member)
		}
	}
	return members
}

// GetUnstarted returns unstarted members
func (h memberHealth) GetUnstartedMembers() []*etcdserverpb.Member {
	var members []*etcdserverpb.Member
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
	quorum, err := MinimumTolerableQuorum(totalMembers)
	if err != nil {
		klog.Errorf("etcd cluster could not determine minimum quorum required. total number of members is %v. minimum quorum required is %v: %v", totalMembers, quorum, err)
		return false
	}
	healthyMembers := len(GetHealthyMemberNames(memberHealth))
	switch {
	// This case should never occur when this function is called by CheckSafeToScaleCluster
	// since this function is never called for the UnsafeScalingStrategy (which covers Single Node OpenShift)
	// and the TwoNodeScalingStrategy and DelayedTwoNodeScalingStrategy when the cluster has two etcd members
	// which is a special expection we make for Two Node OpenShift with Fencing (TNF).
	//
	// It is also never triggered by the HAScalingStrategy and DelayedHAScalingStrategy because having less
	// than 3 healthy nodes violates these scaling strategies, which is checked before this function is called.
	//
	// The reason this is here is to ensure protection against 1 and 2 node membership if ever this function
	// is called directly.
	case totalMembers-quorum < 1:
		klog.Errorf("etcd cluster has quorum of %d which is not fault tolerant: %+v", quorum, memberHealth)
		return false
	case healthyMembers-quorum < 1:
		klog.Errorf("etcd cluster has quorum of %d and %d healthy members which is not fault tolerant: %+v", quorum, healthyMembers, memberHealth)
		return false
	}
	return true
}

// IsQuorumFaultTolerantErr is the same as IsQuorumFaultTolerant but with an error return instead of the log
func IsQuorumFaultTolerantErr(memberHealth []healthCheck) error {
	totalMembers := len(memberHealth)
	quorum, err := MinimumTolerableQuorum(totalMembers)
	if err != nil {
		return fmt.Errorf("etcd cluster could not determine minimum quorum required. total number of members is %v. minimum quorum required is %v: %w", totalMembers, quorum, err)
	}
	healthyMembers := len(GetHealthyMemberNames(memberHealth))
	switch {
	case totalMembers-quorum < 1:
		return fmt.Errorf("etcd cluster has quorum of %d which is not fault tolerant: %+v", quorum, memberHealth)
	case healthyMembers-quorum < 1:
		return fmt.Errorf("etcd cluster has quorum of %d and %d healthy members which is not fault tolerant: %+v", quorum, healthyMembers, memberHealth)
	}
	return nil
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

func MinimumTolerableQuorum(members int) (int, error) {
	if members <= 0 {
		return 0, fmt.Errorf("invalid etcd member length: %v", members)
	}
	return (members / 2) + 1, nil
}
