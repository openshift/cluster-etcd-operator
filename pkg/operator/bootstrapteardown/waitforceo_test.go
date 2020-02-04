package bootstrapteardown

import (
	"testing"

	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"

	operatorv1 "github.com/openshift/api/operator/v1"
	v1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func NewFakeController() *BootstrapTeardownController {
	r := events.NewInMemoryRecorder("Test_isEtcdAvailable")
	return &BootstrapTeardownController{
		eventRecorder: r,
	}
}

func Test_isEtcdAvailable(t *testing.T) {
	c := NewFakeController()
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
			name: "test managed cluster and safe to remove",
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
										Type:   clustermembercontroller.ConditionBootstrapSafeToRemove,
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
		{
			name: "test managed cluster and unsafe to remove",
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
										Type:   clustermembercontroller.ConditionBootstrapSafeToRemove,
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.isEtcdAvailable(tt.args.etcd)
			if (err != nil) != tt.wantErr {
				t.Errorf("doneEtcd() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("doneEtcd() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_doneApiServer(t *testing.T) {
	// TODO: implement me
}

func Test_configMapHasRequiredValues(t *testing.T) {
	c := NewFakeController()
	type args struct {
		configMap *corev1.ConfigMap
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "three urls",
			args: args{
				configMap: &corev1.ConfigMap{Data: map[string]string{
					configMapKey: `{
							"storageConfig": {
								"urls":[
									"https://etcd-1.foo.bar:2379",
									"https://etcd-0.foo.bar:2379",
									"https://etcd-2.foo.bar:2379"
								]
							}
						}`,
				},
				},
			},
			want: true,
		},
		{
			name: "2 urls but not bootstrap",
			args: args{
				configMap: &corev1.ConfigMap{Data: map[string]string{
					configMapKey: `{
							"storageConfig": {
								"urls":[
									"https://etcd-1.foo.bar:2379",
									"https://etcd-2.foo.bar:2379"
								]
							}
						}`,
				},
				},
			},
			want: true,
		},
		{
			name: "single urls but not bootstrap",
			args: args{
				configMap: &corev1.ConfigMap{Data: map[string]string{
					configMapKey: `{
							"storageConfig": {
								"urls":[
									"https://etcd-1.foo.bar:2379"
								]
							}
						}`,
				},
				},
			},
			want: true,
		},
		{
			name: "just the bootstrap url",
			args: args{
				configMap: &corev1.ConfigMap{Data: map[string]string{
					configMapKey: `{
							"storageConfig": {
								"urls": [
									"https://10.13.14.15:2379"
								]
							}
						}`,
				},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.configMapHasRequiredValues(tt.args.configMap); got != tt.want {
				t.Errorf("configMapHasRequiredValues() = %v, want %v", got, tt.want)
			}
		})
	}
}
