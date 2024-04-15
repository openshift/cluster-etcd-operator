package tlshelpers

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// Below lister implementations are required to make the library-go certificate logic more robust.
// Especially in render, we were relying on a race condition between the informer cache and the client
// to create the bundles correctly. This lister always queries the API (or its mock) to directly return the
// configmap/secret at all times - no caching involved.
// Be mindful that this implementation only allows for one namespace, driven by the client passed to the struct.
// Calls to ConfigMaps/Secrets will thus panic, because we can not create a new client from within.

type ConfigMapClientLister struct {
	ConfigMapClient v1.ConfigMapInterface
	Namespace       string
}

func (c *ConfigMapClientLister) Get(name string) (get *corev1.ConfigMap, err error) {
	get, err = c.ConfigMapClient.Get(context.TODO(), name, metav1.GetOptions{})
	return
}

func (c *ConfigMapClientLister) List(selector labels.Selector) ([]*corev1.ConfigMap, error) {
	retLst, err := c.ConfigMapClient.List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	var cms []*corev1.ConfigMap
	for i := range retLst.Items {
		cms = append(cms, &retLst.Items[i])
	}

	return cms, nil
}

func (c *ConfigMapClientLister) ConfigMaps(ns string) corev1listers.ConfigMapNamespaceLister {
	if ns != c.Namespace {
		panic("unsupported operation, can't recreate the client here")
	}
	return c
}

type SecretClientLister struct {
	SecretClient v1.SecretInterface
	Namespace    string
}

func (c *SecretClientLister) Get(name string) (get *corev1.Secret, err error) {
	get, err = c.SecretClient.Get(context.TODO(), name, metav1.GetOptions{})
	return
}

func (c *SecretClientLister) List(selector labels.Selector) ([]*corev1.Secret, error) {
	retLst, err := c.SecretClient.List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	var cms []*corev1.Secret
	for i := range retLst.Items {
		cms = append(cms, &retLst.Items[i])
	}

	return cms, nil
}

func (c *SecretClientLister) Secrets(ns string) corev1listers.SecretNamespaceLister {
	if ns != c.Namespace {
		panic("unsupported operation, can't recreate the client here")
	}
	return c
}
