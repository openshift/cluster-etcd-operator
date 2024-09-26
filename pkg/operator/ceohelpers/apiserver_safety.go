package ceohelpers

import (
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

type ApiserverCheck struct {
	podLister       corev1listers.PodLister
	configMapLister corev1listers.ConfigMapLister
	namespaceLister corev1listers.NamespaceLister
	infraLister     configv1listers.InfrastructureLister
	operatorClient  v1helpers.StaticPodOperatorClient
	etcdClient      etcdcli.AllMemberLister
}

func (c *ApiserverCheck) IsSafeToUpdateRevision() (bool, error) {
	bootstrapComplete, err := IsBootstrapComplete(c.configMapLister, c.operatorClient, c.etcdClient)
	if err != nil {
		return false, err
	}

	// we don't care about this anymore when we're done bootstrapping
	if bootstrapComplete {
		return true, nil
	}

	label, _ := labels.Parse("app=openshift-kube-apiserver")
	pods, err := c.podLister.Pods("openshift-kube-apiserver").List(label)
	if err != nil {
		return false, err
	}

	if len(pods) >= 2 {
		return true, nil
	}

	return false, nil
}

func NewApiserverChecker(
	podLister corev1listers.PodLister,
	configMapLister corev1listers.ConfigMapLister,
	namespaceLister corev1listers.NamespaceLister,
	infraLister configv1listers.InfrastructureLister,
	operatorClient v1helpers.StaticPodOperatorClient,
	etcdClient etcdcli.AllMemberLister,
) *ApiserverCheck {
	c := &ApiserverCheck{
		podLister,
		configMapLister,
		namespaceLister,
		infraLister,
		operatorClient,
		etcdClient,
	}
	return c
}
