package ceohelpers

import (
	"context"

	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
)

type QuorumChecker interface {
	// IsSafeToUpdateRevision checks the current etcd cluster and returns true if the cluster can tolerate the
	// loss of a single etcd member. Such loss is common during new static pod revision.
	// Returns True when it is absolutely safe, false if not. Error otherwise, which always indicates it is unsafe.
	IsSafeToUpdateRevision(ctx context.Context) (bool, error)
}

// QuorumCheck is just a convenience struct around bootstrap.go
type QuorumCheck struct {
	configMapLister corev1listers.ConfigMapLister
	namespaceLister corev1listers.NamespaceLister
	infraLister     configv1listers.InfrastructureLister
	operatorClient  v1helpers.StaticPodOperatorClient
	etcdClient      etcdcli.EtcdClient
}

func (c *QuorumCheck) IsSafeToUpdateRevision(ctx context.Context) (bool, error) {
	err := CheckSafeToScaleCluster(ctx, c.configMapLister, c.operatorClient, c.namespaceLister, c.infraLister, c.etcdClient)
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
	etcdClient etcdcli.EtcdClient,
) QuorumChecker {
	c := &QuorumCheck{
		configMapLister,
		namespaceLister,
		infraLister,
		operatorClient,
		etcdClient,
	}
	return c
}
