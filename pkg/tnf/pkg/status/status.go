package status

import (
	"context"
	"sync"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
)

type DualReplicaClusterStatus int

const (
	DualReplicaClusterStatusDisabled = iota
	DualReplicaClusterStatusEnabled
	DualReplicaClusterStatusBootstrapCompleted
	DualReplicaClusterStatusSetupReadyForEtcdRemoval
)

type ClusterStatus interface {
	IsDualReplicaTopology() bool
	IsBootstrapCompleted() bool
	IsReadyForEtcdRemoval() bool
	SetBootstrapCompleted()
}

type clusterStatus struct {
	ctx                   context.Context
	operatorClient        v1helpers.StaticPodOperatorClient
	isDualReplicaTopology bool
	bootstrapCompleted    bool
	readyForEtcdRemoval   bool
	mu                    sync.Mutex
}

func NewClusterStatus(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient, isDualReplicaTopology, bootstrapCompleted, readyForEtcdRemoval bool) ClusterStatus {
	return &clusterStatus{
		ctx:                   ctx,
		operatorClient:        operatorClient,
		isDualReplicaTopology: isDualReplicaTopology,
		bootstrapCompleted:    bootstrapCompleted,
		readyForEtcdRemoval:   readyForEtcdRemoval,
	}
}

func (cs *clusterStatus) GetStatus() DualReplicaClusterStatus {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var newStatus DualReplicaClusterStatus
	newStatus = DualReplicaClusterStatusDisabled
	if cs.isDualReplicaTopology {
		newStatus = DualReplicaClusterStatusEnabled
		if cs.bootstrapCompleted {
			newStatus = DualReplicaClusterStatusBootstrapCompleted
			// if we were ready already, no need to check the operator status again
			if cs.readyForEtcdRemoval {
				newStatus = DualReplicaClusterStatusSetupReadyForEtcdRemoval
			} else {
				_, status, _, err := cs.operatorClient.GetStaticPodOperatorState()
				if err != nil {
					// it's expected to potentially run into errors during etcd handover
					klog.Errorf("Failed to check if TNF setup is ready for etcd container removal: %v", err)
				} else if v1helpers.IsOperatorConditionTrue(status.Conditions, etcd.OperatorConditionExternalEtcdReady) {
					newStatus = DualReplicaClusterStatusSetupReadyForEtcdRemoval
					cs.readyForEtcdRemoval = true
				}
			}
		}
	}
	return newStatus
}

func (cs *clusterStatus) IsDualReplicaTopology() bool {
	return cs.GetStatus() >= DualReplicaClusterStatusEnabled
}

func (cs *clusterStatus) IsBootstrapCompleted() bool {
	return cs.GetStatus() >= DualReplicaClusterStatusBootstrapCompleted
}

func (cs *clusterStatus) IsReadyForEtcdRemoval() bool {
	return cs.GetStatus() >= DualReplicaClusterStatusSetupReadyForEtcdRemoval
}

func (cs *clusterStatus) SetBootstrapCompleted() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.bootstrapCompleted = true
}
