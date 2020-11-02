package etcdcli

import (
	"reflect"
	"testing"

	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

func TestMemberHealthStatus(t *testing.T) {
	tests := []struct {
		name         string
		memberHealth memberHealth
		want         string
	}{
		{
			"test all available members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			"3 members are available",
		},
		{
			"test an unhealthy member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: false,
				},
			},
			"2 of 3 members are available, etcd-3 is unhealthy",
		},
		{
			"test an unstarted member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.3:2380"}},
					Healthy: false,
				},
			},
			"2 of 3 members are available, NAME-PENDING-10.0.0.3 has not started",
		},
		{
			"test an unstarted member and an unhealthy member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.3:2380"}},
					Healthy: false,
				},
			},
			"1 of 3 members are available, etcd-2 is unhealthy, NAME-PENDING-10.0.0.3 has not started",
		},
		{
			"test two unhealthy members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: false,
				},
			},
			"1 of 3 members are available, etcd-2 is unhealthy, etcd-3 is unhealthy",
		},
		{
			"test two unstarted members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{

					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.3:2380"}},
					Healthy: false,
				},
			},
			"1 of 3 members are available, NAME-PENDING-10.0.0.2 has not started, NAME-PENDING-10.0.0.3 has not started",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.memberHealth.Status(); got != tt.want {
				t.Errorf("test %q = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestGetUnstartedMemberNames(t *testing.T) {
	tests := []struct {
		name         string
		memberHealth memberHealth
		want         []string
	}{
		{
			"test all available members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			[]string{},
		},
		{
			"test an unhealthy member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: false,
				},
			},
			[]string{},
		},
		{
			"test an unstarted member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			[]string{"NAME-PENDING-10.0.0.2"},
		},
		{
			"test an unstarted and an unhealthy member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			[]string{"NAME-PENDING-10.0.0.2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetUnstartedMemberNames(tt.memberHealth)
			if !reflect.DeepEqual(tt.want, got) {
				t.Errorf("test %q = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestGetUnhealthyMemberNames(t *testing.T) {
	tests := []struct {
		name         string
		memberHealth memberHealth
		want         []string
	}{
		{
			"test all available members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			[]string{},
		},
		{
			"test an unhealthy members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: false,
				},
			},
			[]string{"etcd-3"},
		},
		{
			"test an unstarted member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			[]string{"NAME-PENDING-10.0.0.2"},
		},
		{
			"test an unstarted and an unhealthy member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			[]string{"etcd-1", "NAME-PENDING-10.0.0.2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetUnhealthyMemberNames(tt.memberHealth)
			if !reflect.DeepEqual(tt.want, got) {
				t.Errorf("test %q = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsQuorumFaultTolerant(t *testing.T) {
	tests := []struct {
		name         string
		memberHealth memberHealth
		want         bool
	}{
		{
			"test all available members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			true,
		},
		{
			"test an unhealthy members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-2", PeerURLs: []string{"https://10.0.0.2:2380"}, ClientURLs: []string{"https://10.0.0.2:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: false,
				},
			},
			false,
		},
		{
			"test an unstarted member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			false,
		},
		{
			"test an unstarted and an unhealthy member",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: false,
				},
				{
					Member:  &etcdserverpb.Member{Name: "etcd-3", PeerURLs: []string{"https://10.0.0.3:2380"}, ClientURLs: []string{"https://10.0.0.3:2379"}},
					Healthy: true,
				},
			},
			false,
		},
		{
			"test etcd cluster with less than 3 members",
			[]healthCheck{
				{
					Member:  &etcdserverpb.Member{Name: "etcd-1", PeerURLs: []string{"https://10.0.0.1:2380"}, ClientURLs: []string{"https://10.0.0.1:2379"}},
					Healthy: true,
				},
				{
					Member:  &etcdserverpb.Member{PeerURLs: []string{"https://10.0.0.2:2380"}},
					Healthy: true,
				},
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsQuorumFaultTolerant(tt.memberHealth)
			if got != tt.want {
				t.Errorf("test %q = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
