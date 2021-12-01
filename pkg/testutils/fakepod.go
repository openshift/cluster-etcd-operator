package testutils

import (
	"errors"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

func FakePod(name string, configs ...func(node *corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operatorclient.TargetNamespace,
			UID:       uuid.NewUUID(),
		},
	}
	for _, config := range configs {
		config(pod)
	}
	return pod
}

func WithPodStatus(status corev1.PodPhase) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Status = corev1.PodStatus{
			Phase: status,
		}
	}
}

func WithPodLabels(labels map[string]string) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Labels = labels
	}
}

func WithCreationTimestamp(time metav1.Time) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.CreationTimestamp = time
	}
}

func WithScheduledNodeName(name string) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Spec.NodeName = name
	}
}

type FakePodLister struct {
	PodList []*corev1.Pod
}

func (f *FakePodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.PodList, nil
}

func (f *FakePodLister) Pods(namespace string) corev1listers.PodNamespaceLister {
	return &fakePodNamespacer{
		Pods: f.PodList,
	}
}

type fakePodNamespacer struct {
	Pods []*corev1.Pod
}

func (f *fakePodNamespacer) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.Pods, nil
}

func (f *fakePodNamespacer) Get(name string) (*corev1.Pod, error) {
	for _, pod := range f.Pods {
		if pod.Name == name {
			return pod, nil
		}
	}
	return nil, errors.New("NotFound")
}
