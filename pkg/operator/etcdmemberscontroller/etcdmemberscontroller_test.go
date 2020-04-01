package etcdmemberscontroller

import (
	"fmt"
	"testing"

	"go.etcd.io/etcd/etcdserver/etcdserverpb"
)

func Test_getMemberMessage(t *testing.T) {
	type args struct {
		availableMembers []string
		unhealthyMembers []string
		unstartedMembers []string
		allMembers       []*etcdserverpb.Member
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "test all available members",
			args: args{
				availableMembers: []string{"etcd-1", "etcd-2", "etcd-3"},
				unhealthyMembers: []string{},
				unstartedMembers: []string{},
				allMembers: []*etcdserverpb.Member{
					{Name: "etcd-1"},
					{Name: "etcd-2"},
					{Name: "etcd-3"},
				},
			},
			want: fmt.Sprintf("3 members are available"),
		},
		{
			name: "test an unhealthy members",
			args: args{
				availableMembers: []string{"etcd-1", "etcd-2"},
				unhealthyMembers: []string{"etcd-3"},
				unstartedMembers: []string{},
				allMembers: []*etcdserverpb.Member{
					{Name: "etcd-1"},
					{Name: "etcd-2"},
					{Name: "etcd-3"},
				},
			},
			want: fmt.Sprintf("2 of 3 members are available, etcd-3 is unhealthy"),
		},
		{
			name: "test an unstarted members",
			args: args{
				availableMembers: []string{"etcd-1", "etcd-2"},
				unhealthyMembers: []string{},
				unstartedMembers: []string{"etcd-3"},
				allMembers: []*etcdserverpb.Member{
					{Name: "etcd-1"},
					{Name: "etcd-2"},
					{Name: "etcd-3"},
				},
			},
			want: fmt.Sprintf("2 of 3 members are available, etcd-3 has not started"),
		},
		{
			name: "test an unstarted member and an unhealthy member",
			args: args{
				availableMembers: []string{"etcd-1"},
				unhealthyMembers: []string{"etcd-2"},
				unstartedMembers: []string{"etcd-3"},
				allMembers: []*etcdserverpb.Member{
					{Name: "etcd-1"},
					{Name: "etcd-2"},
					{Name: "etcd-3"},
				},
			},
			want: fmt.Sprintf("1 of 3 members are available, etcd-2 is unhealthy, etcd-3 has not started"),
		},
		{
			name: "test two unhealthy members",
			args: args{
				availableMembers: []string{"etcd-1"},
				unhealthyMembers: []string{"etcd-2", "etcd-3"},
				unstartedMembers: []string{},
				allMembers: []*etcdserverpb.Member{
					{Name: "etcd-1"},
					{Name: "etcd-2"},
					{Name: "etcd-3"},
				},
			},
			want: fmt.Sprintf("1 of 3 members are available, etcd-2 is unhealthy, etcd-3 is unhealthy"),
		},
		{
			name: "test two unstarted members",
			args: args{
				availableMembers: []string{"etcd-1"},
				unhealthyMembers: []string{},
				unstartedMembers: []string{"etcd-2", "etcd-3"},
				allMembers: []*etcdserverpb.Member{
					{Name: "etcd-1"},
					{Name: "etcd-2"},
					{Name: "etcd-3"},
				},
			},
			want: fmt.Sprintf("1 of 3 members are available, etcd-2 has not started, etcd-3 has not started"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getMemberMessage(tt.args.availableMembers, tt.args.unhealthyMembers, tt.args.unstartedMembers, tt.args.allMembers); got != tt.want {
				t.Errorf("getMemberMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}
