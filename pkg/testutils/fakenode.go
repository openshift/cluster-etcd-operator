package testutils

import (
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
)

func FakeNode(name string, configs ...func(node *corev1.Node)) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  uuid.NewUUID(),
		},
	}
	for _, config := range configs {
		config(node)
	}
	return node
}

func WithMasterLabel() func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels["node-role.kubernetes.io/master"] = ""
	}
}

func WithCreatedTimeStamp(ts metav1.Time) func(*corev1.Node) {
	return func(node *corev1.Node) {
		node.CreationTimestamp = ts
	}
}

func WithAllocatableStorage(allocatable int64) func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Status.Allocatable == nil {
			node.Status.Allocatable = corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(allocatable, resource.BinarySI),
			}
		}
	}
}

func WithNodeInternalIP(ip string) func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Status.Addresses == nil {
			node.Status.Addresses = []corev1.NodeAddress{}
		}
		node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{
			Type:    corev1.NodeInternalIP,
			Address: ip,
		})
	}
}

type FakeNodeLister struct {
	Nodes []*corev1.Node
}

func (f *FakeNodeLister) List(selector labels.Selector) ([]*corev1.Node, error) {
	return f.Nodes, nil
}
func (f *FakeNodeLister) Get(name string) (*corev1.Node, error) {
	for _, node := range f.Nodes {
		if node.Name == name {
			return node, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "v1.core.kubernetes.io", Resource: "nodes"}, name)
}

type fakeNodeNamespacer struct {
	Nodes []*corev1.Node
}

func (f *fakeNodeNamespacer) List(selector labels.Selector) ([]*corev1.Node, error) {
	return f.Nodes, nil
}

func (f *fakeNodeNamespacer) Get(name string) (*corev1.Node, error) {
	panic("implement me")
}
