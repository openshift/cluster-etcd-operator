package pacemaker

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// =============================================================================
// Condition Builder Types and Functions
// =============================================================================

// ConditionSpec defines the parameters for building a condition.
// Each spec maps a boolean state to appropriate reason/message pairs.
type ConditionSpec struct {
	Type        string
	TrueReason  string
	FalseReason string
	TrueMsg     string
	FalseMsg    string
}

// buildCondition creates a metav1.Condition based on whether the condition is true or false.
// This eliminates repetitive condition builder functions by parameterizing the differences.
func buildCondition(spec ConditionSpec, isTrue bool, now metav1.Time) metav1.Condition {
	if isTrue {
		return metav1.Condition{
			Type:               spec.Type,
			Status:             metav1.ConditionTrue,
			Reason:             spec.TrueReason,
			Message:            spec.TrueMsg,
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               spec.Type,
		Status:             metav1.ConditionFalse,
		Reason:             spec.FalseReason,
		Message:            spec.FalseMsg,
		LastTransitionTime: now,
	}
}

// FindCondition finds a condition by type from a list of conditions.
// Returns nil if the condition is not found.
func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// getConditionStatus gets the status of a condition by type from a list of conditions.
// Returns metav1.ConditionUnknown if the condition is not found.
func getConditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	if cond := FindCondition(conditions, conditionType); cond != nil {
		return cond.Status
	}
	return metav1.ConditionUnknown
}

// =============================================================================
// Node Condition Specs
// =============================================================================

var (
	nodeOnlineSpec = ConditionSpec{
		Type:        v1alpha1.NodeOnlineConditionType,
		TrueReason:  v1alpha1.NodeOnlineReasonOnline,
		FalseReason: v1alpha1.NodeOnlineReasonOffline,
		TrueMsg:     "Node is online",
		FalseMsg:    "Node is offline",
	}
	nodeInServiceSpec = ConditionSpec{
		Type:        v1alpha1.NodeInServiceConditionType,
		TrueReason:  v1alpha1.NodeInServiceReasonInService,
		FalseReason: v1alpha1.NodeInServiceReasonInMaintenance,
		TrueMsg:     "Node is in service",
		FalseMsg:    "Node is in maintenance mode",
	}
	nodeActiveSpec = ConditionSpec{
		Type:        v1alpha1.NodeActiveConditionType,
		TrueReason:  v1alpha1.NodeActiveReasonActive,
		FalseReason: v1alpha1.NodeActiveReasonStandby,
		TrueMsg:     "Node is active",
		FalseMsg:    "Node is in standby mode",
	}
	nodeReadySpec = ConditionSpec{
		Type:        v1alpha1.NodeReadyConditionType,
		TrueReason:  v1alpha1.NodeReadyReasonReady,
		FalseReason: v1alpha1.NodeReadyReasonPending,
		TrueMsg:     "Node is ready",
		FalseMsg:    "Node is pending",
	}
	nodeCleanSpec = ConditionSpec{
		Type:        v1alpha1.NodeCleanConditionType,
		TrueReason:  v1alpha1.NodeCleanReasonClean,
		FalseReason: v1alpha1.NodeCleanReasonUnclean,
		TrueMsg:     "Node is in a clean state",
		FalseMsg:    "Node is in an unclean state",
	}
	nodeMemberSpec = ConditionSpec{
		Type:        v1alpha1.NodeMemberConditionType,
		TrueReason:  v1alpha1.NodeMemberReasonMember,
		FalseReason: v1alpha1.NodeMemberReasonNotMember,
		TrueMsg:     "Node is a cluster member",
		FalseMsg:    "Node is not a cluster member",
	}
	nodeHealthySpec = ConditionSpec{
		Type:        v1alpha1.NodeHealthyConditionType,
		TrueReason:  v1alpha1.NodeHealthyReasonHealthy,
		FalseReason: v1alpha1.NodeHealthyReasonUnhealthy,
		TrueMsg:     "Node is healthy",
		FalseMsg:    "Node has issues that need investigation",
	}
	nodeFencingAvailableSpec = ConditionSpec{
		Type:        v1alpha1.NodeFencingAvailableConditionType,
		TrueReason:  v1alpha1.NodeFencingAvailableReasonAvailable,
		FalseReason: v1alpha1.NodeFencingAvailableReasonUnavailable,
		TrueMsg:     "At least one fencing agent is healthy",
		FalseMsg:    "No fencing agents are healthy",
	}
	nodeFencingHealthySpec = ConditionSpec{
		Type:        v1alpha1.NodeFencingHealthyConditionType,
		TrueReason:  v1alpha1.NodeFencingHealthyReasonHealthy,
		FalseReason: v1alpha1.NodeFencingHealthyReasonUnhealthy,
		TrueMsg:     "All fencing agents are healthy",
		FalseMsg:    "One or more fencing agents are unhealthy",
	}
)

// =============================================================================
// Resource Condition Specs (shared by resources and fencing agents)
// =============================================================================

var (
	resourceInServiceSpec = ConditionSpec{
		Type:        v1alpha1.ResourceInServiceConditionType,
		TrueReason:  v1alpha1.ResourceInServiceReasonInService,
		FalseReason: v1alpha1.ResourceInServiceReasonInMaintenance,
		TrueMsg:     "Resource is in service",
		FalseMsg:    "Resource is in maintenance mode",
	}
	resourceManagedSpec = ConditionSpec{
		Type:        v1alpha1.ResourceManagedConditionType,
		TrueReason:  v1alpha1.ResourceManagedReasonManaged,
		FalseReason: v1alpha1.ResourceManagedReasonUnmanaged,
		TrueMsg:     "Resource is managed by pacemaker",
		FalseMsg:    "Resource is not managed by pacemaker",
	}
	resourceEnabledSpec = ConditionSpec{
		Type:        v1alpha1.ResourceEnabledConditionType,
		TrueReason:  v1alpha1.ResourceEnabledReasonEnabled,
		FalseReason: v1alpha1.ResourceEnabledReasonDisabled,
		TrueMsg:     "Resource is enabled",
		FalseMsg:    "Resource is disabled",
	}
	resourceOperationalSpec = ConditionSpec{
		Type:        v1alpha1.ResourceOperationalConditionType,
		TrueReason:  v1alpha1.ResourceOperationalReasonOperational,
		FalseReason: v1alpha1.ResourceOperationalReasonFailed,
		TrueMsg:     "Resource is operational",
		FalseMsg:    "Resource has failed",
	}
	resourceActiveSpec = ConditionSpec{
		Type:        v1alpha1.ResourceActiveConditionType,
		TrueReason:  v1alpha1.ResourceActiveReasonActive,
		FalseReason: v1alpha1.ResourceActiveReasonInactive,
		TrueMsg:     "Resource is active",
		FalseMsg:    "Resource is not active",
	}
	resourceStartedSpec = ConditionSpec{
		Type:        v1alpha1.ResourceStartedConditionType,
		TrueReason:  v1alpha1.ResourceStartedReasonStarted,
		FalseReason: v1alpha1.ResourceStartedReasonStopped,
		TrueMsg:     "Resource is started",
		FalseMsg:    "Resource is stopped",
	}
	resourceSchedulableSpec = ConditionSpec{
		Type:        v1alpha1.ResourceSchedulableConditionType,
		TrueReason:  v1alpha1.ResourceSchedulableReasonSchedulable,
		FalseReason: v1alpha1.ResourceSchedulableReasonUnschedulable,
		TrueMsg:     "Resource is schedulable",
		FalseMsg:    "Resource is unschedulable (blocked)",
	}
	resourceHealthySpec = ConditionSpec{
		Type:        v1alpha1.ResourceHealthyConditionType,
		TrueReason:  v1alpha1.ResourceHealthyReasonHealthy,
		FalseReason: v1alpha1.ResourceHealthyReasonUnhealthy,
		TrueMsg:     "Resource is healthy",
		FalseMsg:    "Resource has issues that need investigation",
	}
)

// =============================================================================
// Cluster-level Condition Specs
// =============================================================================

var (
	clusterInServiceSpec = ConditionSpec{
		Type:        v1alpha1.ClusterInServiceConditionType,
		TrueReason:  v1alpha1.ClusterInServiceReasonInService,
		FalseReason: v1alpha1.ClusterInServiceReasonInMaintenance,
		TrueMsg:     "Cluster is in service (not in maintenance mode)",
		FalseMsg:    "Cluster is in maintenance mode",
	}
	clusterHealthySpec = ConditionSpec{
		Type:        v1alpha1.ClusterHealthyConditionType,
		TrueReason:  v1alpha1.ClusterHealthyReasonHealthy,
		FalseReason: v1alpha1.ClusterHealthyReasonUnhealthy,
		TrueMsg:     "Pacemaker cluster is healthy",
		FalseMsg:    "Pacemaker cluster has issues that need investigation",
	}
)
