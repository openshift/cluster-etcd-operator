package status

import (
	"context"
	"sync"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
)

type status int

const (
	ExternalEtcdClusterStatusDisabled = iota
	ExternalEtcdClusterStatusEnabled
	ExternalEtcdClusterStatusBootstrapCompleted
	ExternalEtcdClusterStatusReadyForEtcdRemoval
)

type ExternalEtcdClusterStatus interface {
	IsExternalEtcdCluster() bool
	IsBootstrapCompleted() bool
	IsReadyForEtcdRemoval() bool
	SetBootstrapCompleted()
}

type externalEtcdClusterStatus struct {
	ctx                   context.Context
	operatorClient        v1helpers.StaticPodOperatorClient
	isExternalEtcdCluster bool
	bootstrapCompleted    bool
	readyForEtcdRemoval   bool
	mu                    sync.Mutex
}

func NewClusterStatus(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient, isExternalEtcdCluster, bootstrapCompleted, readyForEtcdRemoval bool) ExternalEtcdClusterStatus {
	return &externalEtcdClusterStatus{
		ctx:                   ctx,
		operatorClient:        operatorClient,
		isExternalEtcdCluster: isExternalEtcdCluster,
		bootstrapCompleted:    bootstrapCompleted,
		readyForEtcdRemoval:   readyForEtcdRemoval,
	}
}

func (cs *externalEtcdClusterStatus) getStatus() status {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var newStatus status
	newStatus = ExternalEtcdClusterStatusDisabled
	if cs.isExternalEtcdCluster {
		newStatus = ExternalEtcdClusterStatusEnabled
		if cs.bootstrapCompleted {
			newStatus = ExternalEtcdClusterStatusBootstrapCompleted
			// if we were ready already, no need to check the operator status again
			if cs.readyForEtcdRemoval {
				newStatus = ExternalEtcdClusterStatusReadyForEtcdRemoval
			} else {
				_, status, _, err := cs.operatorClient.GetStaticPodOperatorState()
				if err != nil {
					// it's expected to potentially run into errors during etcd handover
					klog.Errorf("Failed to check if TNF setup is ready for etcd container removal: %v", err)
				} else if v1helpers.IsOperatorConditionTrue(status.Conditions, etcd.OperatorConditionExternalEtcdReady) {
					newStatus = ExternalEtcdClusterStatusReadyForEtcdRemoval
					cs.readyForEtcdRemoval = true
				}
			}
		}
	}
	return newStatus
}

func (cs *externalEtcdClusterStatus) IsExternalEtcdCluster() bool {
	return cs.getStatus() >= ExternalEtcdClusterStatusEnabled
}

func (cs *externalEtcdClusterStatus) IsBootstrapCompleted() bool {
	return cs.getStatus() >= ExternalEtcdClusterStatusBootstrapCompleted
}

func (cs *externalEtcdClusterStatus) IsReadyForEtcdRemoval() bool {
	return cs.getStatus() >= ExternalEtcdClusterStatusReadyForEtcdRemoval
}

func (cs *externalEtcdClusterStatus) SetBootstrapCompleted() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.bootstrapCompleted = true
}
