package pacemaker

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
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
		Type:        pacmkrv1.NodeOnlineConditionType,
		TrueReason:  pacmkrv1.NodeOnlineReasonOnline,
		FalseReason: pacmkrv1.NodeOnlineReasonOffline,
		TrueMsg:     "Node is online",
		FalseMsg:    "Node is offline",
	}
	nodeInServiceSpec = ConditionSpec{
		Type:        pacmkrv1.NodeInServiceConditionType,
		TrueReason:  pacmkrv1.NodeInServiceReasonInService,
		FalseReason: pacmkrv1.NodeInServiceReasonInMaintenance,
		TrueMsg:     "Node is in service",
		FalseMsg:    "Node is in maintenance mode",
	}
	nodeActiveSpec = ConditionSpec{
		Type:        pacmkrv1.NodeActiveConditionType,
		TrueReason:  pacmkrv1.NodeActiveReasonActive,
		FalseReason: pacmkrv1.NodeActiveReasonStandby,
		TrueMsg:     "Node is active",
		FalseMsg:    "Node is in standby mode",
	}
	nodeReadySpec = ConditionSpec{
		Type:        pacmkrv1.NodeReadyConditionType,
		TrueReason:  pacmkrv1.NodeReadyReasonReady,
		FalseReason: pacmkrv1.NodeReadyReasonPending,
		TrueMsg:     "Node is ready",
		FalseMsg:    "Node is pending",
	}
	nodeCleanSpec = ConditionSpec{
		Type:        pacmkrv1.NodeCleanConditionType,
		TrueReason:  pacmkrv1.NodeCleanReasonClean,
		FalseReason: pacmkrv1.NodeCleanReasonUnclean,
		TrueMsg:     "Node is in a clean state",
		FalseMsg:    "Node is in an unclean state",
	}
	nodeMemberSpec = ConditionSpec{
		Type:        pacmkrv1.NodeMemberConditionType,
		TrueReason:  pacmkrv1.NodeMemberReasonMember,
		FalseReason: pacmkrv1.NodeMemberReasonNotMember,
		TrueMsg:     "Node is a cluster member",
		FalseMsg:    "Node is not a cluster member",
	}
	nodeHealthySpec = ConditionSpec{
		Type:        pacmkrv1.NodeHealthyConditionType,
		TrueReason:  pacmkrv1.NodeHealthyReasonHealthy,
		FalseReason: pacmkrv1.NodeHealthyReasonUnhealthy,
		TrueMsg:     "Node is healthy",
		FalseMsg:    "Node has issues that need investigation",
	}
	nodeFencingAvailableSpec = ConditionSpec{
		Type:        pacmkrv1.NodeFencingAvailableConditionType,
		TrueReason:  pacmkrv1.NodeFencingAvailableReasonAvailable,
		FalseReason: pacmkrv1.NodeFencingAvailableReasonUnavailable,
		TrueMsg:     "At least one fencing agent is healthy",
		FalseMsg:    "No fencing agents are healthy",
	}
	nodeFencingHealthySpec = ConditionSpec{
		Type:        pacmkrv1.NodeFencingHealthyConditionType,
		TrueReason:  pacmkrv1.NodeFencingHealthyReasonHealthy,
		FalseReason: pacmkrv1.NodeFencingHealthyReasonUnhealthy,
		TrueMsg:     "All fencing agents are healthy",
		FalseMsg:    "One or more fencing agents are unhealthy",
	}
)

// =============================================================================
// Resource Condition Specs (shared by resources and fencing agents)
// =============================================================================

var (
	resourceInServiceSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceInServiceConditionType,
		TrueReason:  pacmkrv1.ResourceInServiceReasonInService,
		FalseReason: pacmkrv1.ResourceInServiceReasonInMaintenance,
		TrueMsg:     "Resource is in service",
		FalseMsg:    "Resource is in maintenance mode",
	}
	resourceManagedSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceManagedConditionType,
		TrueReason:  pacmkrv1.ResourceManagedReasonManaged,
		FalseReason: pacmkrv1.ResourceManagedReasonUnmanaged,
		TrueMsg:     "Resource is managed by pacemaker",
		FalseMsg:    "Resource is not managed by pacemaker",
	}
	resourceEnabledSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceEnabledConditionType,
		TrueReason:  pacmkrv1.ResourceEnabledReasonEnabled,
		FalseReason: pacmkrv1.ResourceEnabledReasonDisabled,
		TrueMsg:     "Resource is enabled",
		FalseMsg:    "Resource is disabled",
	}
	resourceOperationalSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceOperationalConditionType,
		TrueReason:  pacmkrv1.ResourceOperationalReasonOperational,
		FalseReason: pacmkrv1.ResourceOperationalReasonFailed,
		TrueMsg:     "Resource is operational",
		FalseMsg:    "Resource has failed",
	}
	resourceActiveSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceActiveConditionType,
		TrueReason:  pacmkrv1.ResourceActiveReasonActive,
		FalseReason: pacmkrv1.ResourceActiveReasonInactive,
		TrueMsg:     "Resource is active",
		FalseMsg:    "Resource is not active",
	}
	resourceStartedSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceStartedConditionType,
		TrueReason:  pacmkrv1.ResourceStartedReasonStarted,
		FalseReason: pacmkrv1.ResourceStartedReasonStopped,
		TrueMsg:     "Resource is started",
		FalseMsg:    "Resource is stopped",
	}
	resourceSchedulableSpec = ConditionSpec{
		Type:        pacmkrv1.ResourceSchedulableConditionType,
		TrueReason:  pacmkrv1.ResourceSchedulableReasonSchedulable,
		FalseReason: pacmkrv1.ResourceSchedulableReasonUnschedulable,
		TrueMsg:     "Resource is schedulable",
		FalseMsg:    "Resource is unschedulable (blocked)",
	}
	resourceHealthySpec = ConditionSpec{
		Type:        pacmkrv1.ResourceHealthyConditionType,
		TrueReason:  pacmkrv1.ResourceHealthyReasonHealthy,
		FalseReason: pacmkrv1.ResourceHealthyReasonUnhealthy,
		TrueMsg:     "Resource is healthy",
		FalseMsg:    "Resource has issues that need investigation",
	}
)

// =============================================================================
// Cluster-level Condition Specs
// =============================================================================

var (
	clusterInServiceSpec = ConditionSpec{
		Type:        pacmkrv1.ClusterInServiceConditionType,
		TrueReason:  pacmkrv1.ClusterInServiceReasonInService,
		FalseReason: pacmkrv1.ClusterInServiceReasonInMaintenance,
		TrueMsg:     "Cluster is in service (not in maintenance mode)",
		FalseMsg:    "Cluster is in maintenance mode",
	}
	clusterHealthySpec = ConditionSpec{
		Type:        pacmkrv1.ClusterHealthyConditionType,
		TrueReason:  pacmkrv1.ClusterHealthyReasonHealthy,
		FalseReason: pacmkrv1.ClusterHealthyReasonUnhealthy,
		TrueMsg:     "Pacemaker cluster is healthy",
		FalseMsg:    "Pacemaker cluster has issues that need investigation",
	}
)
