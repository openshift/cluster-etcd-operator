package targetconfigcontroller

import (
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/diff"
)

func Test_ensureRollbackCopier(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		config   *operatorv1.Etcd
		expected *corev1.Pod
	}{
		{
			name:     "copier enabled, preserve copier",
			pod:      newPod().WithContainers(rollbackCopierContainerName, "etcd").Build(),
			config:   newEtcdConfig().Build(),
			expected: newPod().WithContainers(rollbackCopierContainerName, "etcd").Build(),
		},
		{
			name:     "copier disabled, remove copier",
			pod:      newPod().WithContainers(rollbackCopierContainerName, "etcd").Build(),
			config:   newEtcdConfig().WithAnnotation(disableRollbackCopierAnnotation, "").Build(),
			expected: newPod().WithContainers("etcd").Build(),
		},
		{
			name:     "copier disabled, noop",
			pod:      newPod().WithContainers("etcd").Build(),
			config:   newEtcdConfig().WithAnnotation(disableRollbackCopierAnnotation, "").Build(),
			expected: newPod().WithContainers("etcd").Build(),
		},
		{
			name:     "copier enabled, noop",
			pod:      newPod().WithContainers("etcd").Build(),
			config:   newEtcdConfig().Build(),
			expected: newPod().WithContainers("etcd").Build(),
		},
	}

	for _, tc := range tests {
		actual := ensureRollbackCopier(tc.pod, tc.config)
		if !equality.Semantic.DeepEqual(actual, tc.expected) {
			t.Errorf(diff.ObjectDiff(tc.expected, actual))
		}
	}
}

type podBuilder struct {
	corev1.Pod
}

func newPod() *podBuilder { return &podBuilder{} }

func (b *podBuilder) WithContainers(names ...string) *podBuilder {
	for _, name := range names {
		b.Spec.Containers = append(b.Spec.Containers, corev1.Container{Name: name})
	}
	return b
}

func (b *podBuilder) Build() *corev1.Pod {
	return &b.Pod
}

type etcdConfigBuilder struct {
	operatorv1.Etcd
}

func newEtcdConfig() *etcdConfigBuilder { return &etcdConfigBuilder{} }

func (b *etcdConfigBuilder) WithAnnotation(k, v string) *etcdConfigBuilder {
	if b.Annotations == nil {
		b.Annotations = map[string]string{}
	}
	b.Annotations[k] = v
	return b
}

func (b *etcdConfigBuilder) Build() *operatorv1.Etcd {
	return &b.Etcd
}
