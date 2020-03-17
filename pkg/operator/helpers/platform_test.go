package helpers

import (
	"k8s.io/client-go/kubernetes/fake"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type fakeConfigMapLister struct {
	client    kubernetes.Interface
	namespace string
}

func (f *fakeConfigMapLister) Get(name string) (*corev1.ConfigMap, error) {
	return f.client.CoreV1().ConfigMaps("kube-system").Get(name, metav1.GetOptions{})
}

func (f *fakeConfigMapLister) List(selector labels.Selector) (ret []*corev1.ConfigMap, err error) {
	cms, err := f.client.CoreV1().ConfigMaps(f.namespace).List(metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	ret = []*corev1.ConfigMap{}
	for i := range cms.Items {
		ret = append(ret, &cms.Items[i])
	}
	return ret, nil

}

var _configMapData = ` 
apiVersion: v1
baseDomain: devcluster.openshift.com
controlPlane:
  architecture: amd64
  hyperthreading: Enabled
  name: master
  platform:
	aws:
	  rootVolume:
		iops: 0
		size: 120
		type: gp2
	  type: m4.xlarge
	  zones:
	  - us-west-1a
	  - us-west-1b
  replicas: 3
`

func (f *fakeConfigMapLister) ConfigMaps(namespace string) corev1listers.ConfigMapNamespaceLister {
	if namespace == "kube-system" {
		return f
	}
	return nil
}

func TestGetExpectedEtcdSize(t *testing.T) {
	type args struct {
		kubeSystemConfigmapLister corev1listers.ConfigMapLister
	}
	tests := []struct {
		name    string
		args    args
		want    int
		wantErr bool
	}{
		{
			name: "valid install-config",
			args: args{kubeSystemConfigmapLister: &fakeConfigMapLister{
				client: fake.NewSimpleClientset(
					&corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "install-config-v1",
							Namespace: "kube-system",
						},
						Data: map[string]string{
							"install-config": _configMapData,
						},
						BinaryData: nil,
					}),
				namespace: "kube-system"},
			},
			want: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetExpectedEtcdSize(tt.args.kubeSystemConfigmapLister)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetExpectedEtcdSize() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetExpectedEtcdSize() got = %v, want %v", got, tt.want)
			}
		})
	}
}
