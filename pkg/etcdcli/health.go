package etcdcli

import (
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"k8s.io/klog"
)

type healthCheck struct {
	Member  *etcdserverpb.Member
	Health  bool
	Started bool
	Took    string
	Error   string
}

type memberHealth struct {
	Check []healthCheck
}

// Status returns a reporting of memberHealth results by name in three buckets healthy, unhealthy and unstarted.
func (h *memberHealth) Status() ([]string, []string, []string) {
	healthy := []string{}
	unhealthy := []string{}
	unstarted := []string{}
	for _, etcd := range h.Check {
		switch {
		case len(etcd.Member.ClientURLs) == 0:
			unstarted = append(unstarted, GetMemberNameOrHost(etcd.Member))
		case etcd.Health:
			healthy = append(healthy, etcd.Member.Name)
		default:
			unhealthy = append(unhealthy, etcd.Member.Name)
		}
	}
	klog.Infof("Status() healthy %v unhealthy %v unstarted %v", healthy, unhealthy, unstarted)
	return healthy, unhealthy, unstarted
}

// MemberStatus returns healthy and unhealthy
func (h *memberHealth) MemberStatus() ([]*etcdserverpb.Member, []*etcdserverpb.Member) {
	healthy := []*etcdserverpb.Member{}
	unhealthy := []*etcdserverpb.Member{}
	for _, etcd := range h.Check {
		switch {
		case etcd.Health:
			healthy = append(healthy, etcd.Member)
		default:
			unhealthy = append(unhealthy, etcd.Member)
		}
	}
	klog.Infof("MemberStatus() healthy %+v unhealthy %+v", healthy, unhealthy)
	return healthy, unhealthy
}
