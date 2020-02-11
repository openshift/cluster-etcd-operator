package bootstrapteardown

import (
	"testing"

	"github.com/openshift/library-go/pkg/operator/events"

	corev1 "k8s.io/api/core/v1"
)

func NewFakeController() *BootstrapTeardownController {
	r := events.NewInMemoryRecorder("Test_isEtcdAvailable")
	return &BootstrapTeardownController{
		eventRecorder: r,
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
