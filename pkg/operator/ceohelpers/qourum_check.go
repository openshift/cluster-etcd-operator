package ceohelpers

import (
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
)

type QuorumChecker interface {
	// IsSafeToUpdateRevision checks the current etcd cluster and returns true if the cluster can tolerate the
	// loss of a single etcd member. Such loss is common during new static pod revision.
	// Returns True when it is absolutely safe, false if not. Error otherwise, which always indicates it is unsafe.
	IsSafeToUpdateRevision() (bool, error)
}

// AlwaysSafeQuorumChecker can be used for testing and always returns that it is safe to update a revision
type AlwaysSafeQuorumChecker struct {
}

// IsSafeToUpdateRevision always returns true, nil
func (c *AlwaysSafeQuorumChecker) IsSafeToUpdateRevision() (bool, error) {
	return true, nil
}

// QuorumCheck is just a convenience struct around bootstrap.go
type QuorumCheck struct {
	configMapLister   corev1listers.ConfigMapLister
	namespaceLister   corev1listers.NamespaceLister
	infraLister       configv1listers.InfrastructureLister
	operatorClient    v1helpers.StaticPodOperatorClient
	etcdClient        etcdcli.AllMemberLister
	machineAPIChecker MachineAPIChecker
	machineLister     machinelistersv1beta1.MachineLister
	machineSelector   labels.Selector
	masterNodeLister  corev1listers.NodeLister
	networkLister     configv1listers.NetworkLister
}

func (c *QuorumCheck) IsSafeToUpdateRevision() (bool, error) {
	err := CheckSafeToScaleCluster(c.configMapLister, c.operatorClient, c.namespaceLister, c.infraLister, c.etcdClient, c.machineAPIChecker, c.machineLister, c.machineSelector, c.masterNodeLister, c.networkLister)
	if err != nil {
		return false, err
	}

	return true, nil
}

func NewQuorumChecker(
	configMapLister corev1listers.ConfigMapLister,
	namespaceLister corev1listers.NamespaceLister,
	infraLister configv1listers.InfrastructureLister,
	operatorClient v1helpers.StaticPodOperatorClient,
	etcdClient etcdcli.AllMemberLister,
	machineAPIChecker MachineAPIChecker,
	machineLister machinelistersv1beta1.MachineLister,
	machineSelector labels.Selector,
	masterNodeLister corev1listers.NodeLister,
	networkInformer configv1informers.NetworkInformer,
) QuorumChecker {
	c := &QuorumCheck{
		configMapLister,
		namespaceLister,
		infraLister,
		operatorClient,
		etcdClient,
		machineAPIChecker,
		machineLister,
		machineSelector,
		masterNodeLister,
		networkInformer.Lister(),
	}
	return c
}
