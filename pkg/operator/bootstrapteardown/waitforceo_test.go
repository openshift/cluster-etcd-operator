package bootstrapteardown

import (
	"context"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	v1 "github.com/openshift/api/operator/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func Test_wait(t *testing.T) {
	type args struct {
		etcd *operatorv1.Etcd
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "test unmanaged cluster",
			args: args{
				etcd: &v1.Etcd{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Spec: v1.EtcdSpec{
						StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
							OperatorSpec: v1.OperatorSpec{
								ManagementState: v1.Unmanaged,
							},
						},
					},
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name: "test managed cluster but degraded",
			args: args{
				etcd: &v1.Etcd{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Spec: v1.EtcdSpec{
						StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
							OperatorSpec: v1.OperatorSpec{
								ManagementState: v1.Managed,
							},
						},
					},
					Status: v1.EtcdStatus{
						StaticPodOperatorStatus: v1.StaticPodOperatorStatus{
							OperatorStatus: v1.OperatorStatus{
								Conditions: []v1.OperatorCondition{
									{
										Type:   v1.OperatorStatusTypeDegraded,
										Status: v1.ConditionTrue,
									},
								},
							},
						}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "test managed cluster but progressing",
			args: args{
				etcd: &v1.Etcd{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Spec: v1.EtcdSpec{
						StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
							OperatorSpec: v1.OperatorSpec{
								ManagementState: v1.Managed,
							},
						},
					},
					Status: v1.EtcdStatus{
						StaticPodOperatorStatus: v1.StaticPodOperatorStatus{
							OperatorStatus: v1.OperatorStatus{
								Conditions: []v1.OperatorCondition{
									{
										Type:   v1.OperatorStatusTypeProgressing,
										Status: v1.ConditionTrue,
									},
								},
							},
						}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "test managed cluster but unavailable",
			args: args{
				etcd: &v1.Etcd{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Spec: v1.EtcdSpec{
						StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
							OperatorSpec: v1.OperatorSpec{
								ManagementState: v1.Managed,
							},
						},
					},
					Status: v1.EtcdStatus{
						StaticPodOperatorStatus: v1.StaticPodOperatorStatus{
							OperatorStatus: v1.OperatorStatus{
								Conditions: []v1.OperatorCondition{
									{
										Type:   v1.OperatorStatusTypeAvailable,
										Status: v1.ConditionFalse,
									},
								},
							},
						}},
				},
			},
			want:    false,
			wantErr: false,
		},
		{
			name: "test managed cluster and done",
			args: args{
				etcd: &v1.Etcd{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Spec: v1.EtcdSpec{
						StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
							OperatorSpec: v1.OperatorSpec{
								ManagementState: v1.Managed,
							},
						},
					},
					Status: v1.EtcdStatus{
						StaticPodOperatorStatus: v1.StaticPodOperatorStatus{
							OperatorStatus: v1.OperatorStatus{
								Conditions: []v1.OperatorCondition{
									{
										Type:   v1.OperatorStatusTypeDegraded,
										Status: v1.ConditionFalse,
									},
									{
										Type:   v1.OperatorStatusTypeProgressing,
										Status: v1.ConditionFalse,
									},
									{
										Type:   v1.OperatorStatusTypeAvailable,
										Status: v1.ConditionTrue,
									},
								},
							},
						}},
				},
			},
			want:    true,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := done(tt.args.etcd)
			if (err != nil) != tt.wantErr {
				t.Errorf("done() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("done() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWaitForEtcdBootstrap(t *testing.T) {
	type args struct {
		ctx                context.Context
		operatorRestClient rest.Interface
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := waitForEtcdBootstrap(tt.args.ctx, tt.args.operatorRestClient); (err != nil) != tt.wantErr {
				t.Errorf("WaitForEtcdBootstrap() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
