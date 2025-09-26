package ceohelpers

import (
	"context"

	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
)

type ExternalEtcdClusterStatus struct {
	IsExternalEtcdCluster    bool
	IsEtcdRunningInCluster   bool
	IsReadyForEtcdTransition bool
}

// IsExternalEtcdCluster determines if the cluster is configured for external etcd
// by checking if the control plane topology is set to DualReplicaTopologyMode.
// This indicates that the cluster is using Two Node Fencing (TNF) with external etcd.
func IsExternalEtcdCluster(ctx context.Context, infraLister configv1listers.InfrastructureLister) (bool, error) {
	dualReplicaEnabled, err := IsDualReplicaTopology(ctx, infraLister)
	if err != nil {
		klog.Errorf("failed to determine DualReplicaTopology: %v", err)
		return false, err
	}

	if dualReplicaEnabled {
		klog.V(4).Infof("detected DualReplica topology - external etcd cluster")
	}

	return dualReplicaEnabled, nil
}

// IsReadyForEtcdTransition checks if the cluster is ready for etcd transition
// by examining the operator status for the ExternalEtcdReadyForTransition condition.
// This condition is set when the TNF setup is ready to take over the etcd container.
func IsReadyForEtcdTransition(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) (bool, error) {
	_, opStatus, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil {
		klog.Errorf("failed to get static pod operator state: %v", err)
		return false, err
	}

	readyForEtcdTransition := v1helpers.IsOperatorConditionTrue(opStatus.Conditions, etcd.OperatorConditionExternalEtcdReadyForTransition)
	if readyForEtcdTransition {
		klog.V(4).Infof("ready for etcd transition")
	}

	return readyForEtcdTransition, nil
}

// IsEtcdRunningInCluster checks if the etcd bootstrap process is completed
// by examining the operator status for the EtcdRunningInCluster condition.
func IsEtcdRunningInCluster(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) (bool, error) {
	_, opStatus, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil {
		klog.Errorf("failed to get static pod operator state: %v", err)
		return false, err
	}

	etcdRunningInCluster := v1helpers.IsOperatorConditionTrue(opStatus.Conditions, etcd.OperatorConditionEtcdRunningInCluster)
	if etcdRunningInCluster {
		klog.V(4).Infof("bootstrap completed, etcd running in cluster")
	}

	return etcdRunningInCluster, nil
}

// GetExternalEtcdClusterStatus provides a comprehensive status check for external etcd clusters.
// It returns the external etcd status, bootstrap completion status, and readiness for transition.
func GetExternalEtcdClusterStatus(ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	infraLister configv1listers.InfrastructureLister) (externalEtcdStatus ExternalEtcdClusterStatus, err error) {

	externalEtcdStatus = ExternalEtcdClusterStatus{
		IsExternalEtcdCluster:    false,
		IsEtcdRunningInCluster:   false,
		IsReadyForEtcdTransition: false,
	}

	// Check if this is an external etcd cluster
	externalEtcdStatus.IsExternalEtcdCluster, err = IsExternalEtcdCluster(ctx, infraLister)
	if err != nil {
		return externalEtcdStatus, err
	}

	// If not external etcd, return early
	if !externalEtcdStatus.IsExternalEtcdCluster {
		return externalEtcdStatus, nil
	}

	// Get operator status once for both bootstrap and transition checks
	_, opStatus, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil {
		klog.Errorf("failed to get static pod operator state: %v", err)
		return externalEtcdStatus, err
	}

	// Check bootstrap completion
	externalEtcdStatus.IsEtcdRunningInCluster = v1helpers.IsOperatorConditionTrue(opStatus.Conditions, etcd.OperatorConditionEtcdRunningInCluster)

	// Check readiness for transition
	externalEtcdStatus.IsReadyForEtcdTransition = v1helpers.IsOperatorConditionTrue(opStatus.Conditions, etcd.OperatorConditionExternalEtcdReadyForTransition)

	if externalEtcdStatus.IsEtcdRunningInCluster {
		klog.V(4).Infof("bootstrap completed, etcd running in cluster")
	}
	if externalEtcdStatus.IsReadyForEtcdTransition {
		klog.V(4).Infof("ready for etcd transition")
	}

	return externalEtcdStatus, nil
}
