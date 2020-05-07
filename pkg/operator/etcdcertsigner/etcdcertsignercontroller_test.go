package etcdcertsigner

import (
	"reflect"
	"sort"
	"testing"

	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

func TestPeerDNSSubjectAlternativeNames(t *testing.T) {
	tests := []struct {
		etcdMembers         []*etcdserverpb.Member
		etcdDiscoveryDomain string
		nodeInternalIPs     []string
		want                []string
	}{
		{
			// 4.3 upgrade
			[]*etcdserverpb.Member{
				{
					Name:       "etcd-member-0",
					PeerURLs:   []string{"https://etcd-0.clustername.com:2380"},
					ClientURLs: []string{"https://192.168.8.10:2379"},
				},
				{
					Name:       "etcd-member-1",
					PeerURLs:   []string{"https://etcd-1.clustername.com:2380"},
					ClientURLs: []string{"https://192.168.8.11:2379"},
				},
				{
					Name:       "etcd-member-2",
					PeerURLs:   []string{"https://etcd-0.clustername.com:2380"},
					ClientURLs: []string{""},
				},
			},
			"clustername.com",
			[]string{"172.168.16.4", "192.168.8.10"},
			[]string{"etcd-0.clustername.com"},
		},
		{
			// 4.3 upgrade in progress
			[]*etcdserverpb.Member{
				{
					Name:       "etcd-member-0",
					PeerURLs:   []string{"https://etcd-0.clustername.com:2380"},
					ClientURLs: []string{"https://192.168.8.12:2379"},
				},
				{
					Name:       "etcd-1",
					PeerURLs:   []string{"https://192.168.8.13:2380"},
					ClientURLs: []string{"https://192.168.8.13:2379"},
				},
				{
					Name:       "etcd-member-2",
					PeerURLs:   []string{"https://etcd-2.clustername.com:2380"},
					ClientURLs: []string{"https://192.168.8.13:2379"},
				},
			},
			"clustername.com",
			[]string{"192.168.8.12"},
			[]string{"etcd-0.clustername.com"},
		},
		{
			// 4.4+ install
			[]*etcdserverpb.Member{
				{
					Name:       "etcd-0",
					PeerURLs:   []string{"https://192.168.8.10:2380"},
					ClientURLs: []string{"https://192.168.8.10:2379"},
				},
				{
					Name:       "etcd-1",
					PeerURLs:   []string{"https://192.168.8.11:2380"},
					ClientURLs: []string{"https://192.168.8.11:2379"},
				},
				{
					Name:       "etcd-2",
					PeerURLs:   []string{"https://192.168.8.12:2380"},
					ClientURLs: []string{"https://192.168.8.12:2379"},
				},
			},
			"clustername.com",
			[]string{"192.168.8.12"},
			nil,
		},
		{
			// 4.3 upgrade ipv6
			[]*etcdserverpb.Member{
				{
					Name:       "etcd-0",
					PeerURLs:   []string{"https://[001:0db8:85a3:0000:0000:8a2e:0370:7333]:2380"},
					ClientURLs: []string{"https://[001:0db8:85a3:0000:0000:8a2e:0370:7333]:2379"},
				},
				{
					Name:       "etcd-1",
					PeerURLs:   []string{"https://[001:0db8:85a3:0000:0000:8a2e:0370:7334]:2380"},
					ClientURLs: []string{"https://[001:0db8:85a3:0000:0000:8a2e:0370:7334]:2379"},
				},
				{
					Name:       "etcd-member-2",
					PeerURLs:   []string{"https://etcd-2.clustername.com:2380"},
					ClientURLs: []string{"https://[001:0db8:85a3:0000:0000:8a2e:0370:7335]:2379"},
				},
			},
			"clustername.com",
			[]string{"001:0db8:85a3:0000:0000:8a2e:0370:7335"},
			[]string{"etcd-2.clustername.com"},
		},
	}
	for _, test := range tests {
		got, _ := getPeerDNSSubjectAlternativeNamesFromMemberList(test.etcdDiscoveryDomain, test.etcdMembers, test.nodeInternalIPs)
		sort.Strings(test.want)
		sort.Strings(got)
		if reflect.DeepEqual(test.want, got) != true {
			t.Errorf("createDNSSubjectAlternativeNamesFromMemberList() = %v, want %v", got, test.want)
		}
	}
}
