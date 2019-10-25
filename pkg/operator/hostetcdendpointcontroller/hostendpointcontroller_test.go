package hostetcdendpointcontroller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func Test_diff(t *testing.T) {
	type args struct {
		hostnames      []string
		healthyMembers []string
	}
	tests := []struct {
		name       string
		args       args
		wantAdd    []string
		wantRemove []string
	}{
		{
			name: "only etcd-bootstrap",
			args: args{
				hostnames:      []string{"etcd-bootstrap"},
				healthyMembers: []string{"etcd-bootstrap"},
			},
			wantAdd:    nil,
			wantRemove: nil,
		},
		{
			name: "scaling: add a member after etcd-bootstrap",
			args: args{
				hostnames:      []string{"etcd-bootstrap"},
				healthyMembers: []string{"etcd-bootstrap", "etcd-0"},
			},
			wantAdd:    []string{"etcd-0"},
			wantRemove: nil,
		},
		{
			name: "scaling: add second member after etcd-bootstrap and etcd-0",
			args: args{
				hostnames:      []string{"etcd-bootstrap", "etcd-0"},
				healthyMembers: []string{"etcd-bootstrap", "etcd-0", "etcd-1"},
			},
			wantAdd:    []string{"etcd-1"},
			wantRemove: nil,
		},
		{
			name: "scaling: ignore etcd-bootstrap member",
			args: args{
				hostnames:      []string{"etcd-bootstrap", "etcd-0", "etcd-1"},
				healthyMembers: []string{"etcd-0", "etcd-1"},
			},
			wantAdd:    nil,
			wantRemove: nil,
		},
		{
			name: "scaling: add etcd-2 at the same time",
			args: args{
				hostnames:      []string{"etcd-bootstrap", "etcd-0", "etcd-1"},
				healthyMembers: []string{"etcd-0", "etcd-1", "etcd-2"},
			},
			wantAdd:    []string{"etcd-2"},
			wantRemove: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAdd, gotRemove := diff(tt.args.hostnames, tt.args.healthyMembers)
			if !reflect.DeepEqual(gotAdd, tt.wantAdd) {
				t.Errorf("diff() gotAdd = %v, want %v", gotAdd, tt.wantAdd)
			}
			if !reflect.DeepEqual(gotRemove, tt.wantRemove) {
				t.Errorf("diff() gotRemove = %v, want %v", gotRemove, tt.wantRemove)
			}
		})
	}
}

func Test_pickIpAddress(t *testing.T) {
	type args struct {
		assignedIPAddresses []string
		newIPAddressNeeded  int
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "case scaling from etcd-bootstrap",
			args: args{
				assignedIPAddresses: []string{subnetPrefix + "1"},
				newIPAddressNeeded:  2,
			},
		},
		{
			name: "case scaline from 2 nodes",
			args: args{
				assignedIPAddresses: []string{subnetPrefix + "102", subnetPrefix + "114"},
				newIPAddressNeeded:  3,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickUniqueIPAddress(tt.args.assignedIPAddresses, tt.args.newIPAddressNeeded)
			if len(got) != tt.args.newIPAddressNeeded {
				t.Fatalf("got %d, needed %d", len(got), tt.args.newIPAddressNeeded)
			}
			for _, ip := range got {
				if ok := in(tt.args.assignedIPAddresses, ip); ok {
					t.Fatalf("ip %s in already asigned %#v", ip, tt.args.assignedIPAddresses)
				}
				tt.args.assignedIPAddresses = append(tt.args.assignedIPAddresses, ip)
			}
		})
	}
}

type fakeEtcdMemberGetter []string

func (f fakeEtcdMemberGetter) GetHealthyEtcdMembers() ([]string, error) {
	return f, nil
}

func TestHostEtcdEndpointController_getNewAddressSubset(t *testing.T) {
	type fields struct {
		healthyEtcdMemberGetter HealthyEtcdMembersGetter
	}
	type args struct {
		addresses []corev1.EndpointAddress
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    []corev1.EndpointAddress
		wantErr bool
	}{
		{
			name:   "scaling up from bootstrap",
			fields: fields{healthyEtcdMemberGetter: fakeEtcdMemberGetter{"etcd-bootstrap", "etcd-0", "etcd-1"}},
			args: args{addresses: []corev1.EndpointAddress{
				{
					Hostname: "etcd-boostrap",
					IP:       subnetPrefix + "1",
				},
			}},
			want: []corev1.EndpointAddress{
				{
					Hostname: "etcd-bootstrap",
					IP:       subnetPrefix + "1",
				},
				{
					Hostname: "etcd-0",
				},
				{
					Hostname: "etcd-1",
				},
			},
			wantErr: false,
		},
		{
			name:   "scaling down etcd-bootstrap",
			fields: fields{healthyEtcdMemberGetter: fakeEtcdMemberGetter{"etcd-0", "etcd-1"}},
			args: args{addresses: []corev1.EndpointAddress{
				{
					Hostname: "etcd-boostrap",
					IP:       subnetPrefix + "1",
				},
				{
					Hostname: "etcd-0",
					IP:       subnetPrefix + "2",
				},
				{
					Hostname: "etcd-1",
					IP:       subnetPrefix + "3",
				},
			}},
			want: []corev1.EndpointAddress{
				{
					Hostname: "etcd-0",
				},
				{
					Hostname: "etcd-1",
				},
			},
			wantErr: false,
		},
		{
			name:   "scaling down etcd-bootstrap and scale another member at the same time",
			fields: fields{healthyEtcdMemberGetter: fakeEtcdMemberGetter{"etcd-0", "etcd-1", "etcd-2"}},
			args: args{addresses: []corev1.EndpointAddress{
				{
					Hostname: "etcd-boostrap",
					IP:       subnetPrefix + "1",
				},
				{
					Hostname: "etcd-0",
					IP:       subnetPrefix + "2",
				},
				{
					Hostname: "etcd-1",
					IP:       subnetPrefix + "3",
				},
			}},
			want: []corev1.EndpointAddress{
				{
					Hostname: "etcd-0",
				},
				{
					Hostname: "etcd-1",
				},
				{
					Hostname: "etcd-2",
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &HostEtcdEndpointController{
				healthyEtcdMemberGetter: tt.fields.healthyEtcdMemberGetter,
			}
			got, err := h.getNewAddressSubset(tt.args.addresses)
			if (err != nil) != tt.wantErr {
				t.Errorf("getNewAddressSubset() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("getNewAddressSubset() got length = %v, want length %v", len(got), len(tt.want))
				return
			}
			for i, addr := range got {
				if addr.Hostname != tt.want[i].Hostname {
					t.Errorf("for index %d want hostname  %v, got  %v", i, addr.Hostname, tt.want[i].Hostname)
					return
				}
			}
		})
	}
}
