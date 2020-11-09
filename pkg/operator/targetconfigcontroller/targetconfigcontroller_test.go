package targetconfigcontroller

import (
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
)

func Test_ensureRollbackCopier(t *testing.T) {
	// TODO
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
