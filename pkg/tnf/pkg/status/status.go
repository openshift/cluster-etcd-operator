package status

import (
	"context"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
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
	kubeClient            kubernetes.Interface
	isDualReplicaTopology bool
	bootstrapCompleted    bool
	wasReady              bool
	mu                    sync.Mutex
}

func NewClusterStatus(ctx context.Context, kubeClient kubernetes.Interface, isDualReplicaTopology, bootstrapCompleted bool) ClusterStatus {
	return &clusterStatus{
		ctx:                   ctx,
		kubeClient:            kubeClient,
		isDualReplicaTopology: isDualReplicaTopology,
		bootstrapCompleted:    bootstrapCompleted,
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
			// if we were ready already, no need to check the job status again
			if cs.wasReady {
				newStatus = DualReplicaClusterStatusSetupReadyForEtcdRemoval
			} else {
				ready, err := jobs.IsTNFReadyForEtcdContainerRemoval(cs.ctx, cs.kubeClient)
				if err != nil {
					// it's expected to potentially run into errors during etcd handover
					// never return an older status than before
					klog.Errorf("Failed to check if TNF setup is ready for etcd container removal: %v", err)
				} else if ready {
					newStatus = DualReplicaClusterStatusSetupReadyForEtcdRemoval
					cs.wasReady = true
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
