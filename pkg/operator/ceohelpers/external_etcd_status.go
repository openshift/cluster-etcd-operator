package ceohelpers

import (
	"context"

	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"
)

const (
	OperatorConditionEtcdRunningInCluster               = "EtcdRunningInCluster"
	OperatorConditionExternalEtcdReadyForTransition     = "ExternalEtcdReadyForTransition"
	OperatorConditionExternalEtcdHasCompletedTransition = "ExternalEtcdHasCompletedTransition"
)

type ExternalEtcdClusterStatus struct {
	IsExternalEtcdCluster              bool
	IsEtcdRunningInCluster             bool
	IsReadyForEtcdTransition           bool
	HasExternalEtcdCompletedTransition bool
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
func IsReadyForEtcdTransition(operatorClient v1helpers.StaticPodOperatorClient) (bool, error) {
	_, opStatus, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil {
		klog.Errorf("failed to get static pod operator state: %v", err)
		return false, err
	}

	if opStatus == nil {
		klog.V(2).Info("static pod operator status not yet populated; ready for etcd transition unknown")
		return false, nil
	}

	readyForEtcdTransition := v1helpers.IsOperatorConditionTrue(opStatus.Conditions, OperatorConditionExternalEtcdReadyForTransition)
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

	if opStatus == nil {
		klog.V(2).Info("static pod operator status not yet populated; bootstrap completion unknown")
		return false, nil
	}

	etcdRunningInCluster := v1helpers.IsOperatorConditionTrue(opStatus.Conditions, OperatorConditionEtcdRunningInCluster)
	if etcdRunningInCluster {
		klog.V(4).Infof("bootstrap completed, etcd running in cluster")
	}

	return etcdRunningInCluster, nil
}

// HasExternalEtcdCompletedTransition checks if the transition to external etcd process is completed
// by examining the operator status for the HasExternalEtcdCompletedTransition condition.
func HasExternalEtcdCompletedTransition(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) (bool, error) {
	_, opStatus, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil {
		klog.Errorf("failed to get static pod operator state: %v", err)
		return false, err
	}

	if opStatus == nil {
		klog.V(2).Info("static pod operator status not yet populated; transition completion unknown")
		return false, nil
	}

	hasExternalEtcdCompletedTransition := v1helpers.IsOperatorConditionTrue(opStatus.Conditions, OperatorConditionExternalEtcdHasCompletedTransition)
	if hasExternalEtcdCompletedTransition {
		klog.V(4).Infof("etcd has transitioned to running externally")
	}

	return hasExternalEtcdCompletedTransition, nil
}

// GetExternalEtcdClusterStatus provides a comprehensive status check for external etcd clusters.
// It returns the external etcd status, bootstrap completion status, and readiness for transition.
func GetExternalEtcdClusterStatus(ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	infraLister configv1listers.InfrastructureLister) (externalEtcdStatus ExternalEtcdClusterStatus, err error) {

	externalEtcdStatus = ExternalEtcdClusterStatus{
		IsExternalEtcdCluster:              false,
		IsEtcdRunningInCluster:             false,
		IsReadyForEtcdTransition:           false,
		HasExternalEtcdCompletedTransition: false,
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

	if opStatus == nil {
		klog.V(2).Info("static pod operator status not yet populated; external etcd cluster status unknown")
		return externalEtcdStatus, nil
	}

	// Check bootstrap completion
	externalEtcdStatus.IsEtcdRunningInCluster = v1helpers.IsOperatorConditionTrue(opStatus.Conditions, OperatorConditionEtcdRunningInCluster)

	// Check readiness for transition
	externalEtcdStatus.IsReadyForEtcdTransition = v1helpers.IsOperatorConditionTrue(opStatus.Conditions, OperatorConditionExternalEtcdReadyForTransition)

	// Check if etcd has completed transition to running externally
	externalEtcdStatus.HasExternalEtcdCompletedTransition = v1helpers.IsOperatorConditionTrue(opStatus.Conditions, OperatorConditionExternalEtcdHasCompletedTransition)

	if externalEtcdStatus.IsEtcdRunningInCluster {
		klog.V(4).Infof("bootstrap completed, etcd running in cluster")
	}
	if externalEtcdStatus.IsReadyForEtcdTransition {
		klog.V(4).Infof("ready for etcd transition")
	}
	if externalEtcdStatus.HasExternalEtcdCompletedTransition {
		klog.V(4).Infof("etcd has transitioned to running externally")
	}

	return externalEtcdStatus, nil
}
