package pacemaker

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// =============================================================================
// Shared Test Fixtures
//
// This file contains reusable test fixture builders for the pacemaker package.
// These fixtures help create consistent test data across multiple test files.
// =============================================================================

// =============================================================================
// Cluster Condition Fixtures
// =============================================================================

// createHealthyClusterConditions creates healthy cluster-level conditions
func createHealthyClusterConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               v1alpha1.ClusterHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterHealthyReasonHealthy,
			Message:            "Cluster is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ClusterInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterInServiceReasonInService,
			Message:            "Cluster is in service",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected,
			Message:            "Expected 2 nodes, found 2",
			LastTransitionTime: now,
		},
	}
}

// createInsufficientNodesClusterConditions creates cluster conditions with insufficient nodes
func createInsufficientNodesClusterConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               v1alpha1.ClusterHealthyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ClusterHealthyReasonUnhealthy,
			Message:            "Cluster has issues",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ClusterInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterInServiceReasonInService,
			Message:            "Cluster is in service",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes,
			Message:            "Expected 2 nodes, found 1",
			LastTransitionTime: now,
		},
	}
}

// =============================================================================
// Node Condition Fixtures
// =============================================================================

// createHealthyNodeConditions creates healthy node-level conditions
func createHealthyNodeConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               v1alpha1.NodeHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeHealthyReasonHealthy,
			Message:            "Node is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.NodeOnlineConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeOnlineReasonOnline,
			Message:            "Node is online",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.NodeInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeInServiceReasonInService,
			Message:            "Node is in service",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.NodeActiveConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeActiveReasonActive,
			Message:            "Node is active",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.NodeReadyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeReadyReasonReady,
			Message:            "Node is ready",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.NodeCleanConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeCleanReasonClean,
			Message:            "Node is clean",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.NodeMemberConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeMemberReasonMember,
			Message:            "Node is a member",
			LastTransitionTime: now,
		},
	}
}

// =============================================================================
// Resource Condition Fixtures
// =============================================================================

// createHealthyResourceConditions creates healthy resource-level conditions
func createHealthyResourceConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               v1alpha1.ResourceHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceHealthyReasonHealthy,
			Message:            "Resource is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceInServiceReasonInService,
			Message:            "Resource is in service",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceManagedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceManagedReasonManaged,
			Message:            "Resource is managed",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceEnabledConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceEnabledReasonEnabled,
			Message:            "Resource is enabled",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceOperationalConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceOperationalReasonOperational,
			Message:            "Resource is operational",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceActiveConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceActiveReasonActive,
			Message:            "Resource is active",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceStartedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceStartedReasonStarted,
			Message:            "Resource is started",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceSchedulableConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceSchedulableReasonSchedulable,
			Message:            "Resource is schedulable",
			LastTransitionTime: now,
		},
	}
}

// createUnhealthyResourceConditions creates unhealthy resource-level conditions
func createUnhealthyResourceConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               v1alpha1.ResourceHealthyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ResourceHealthyReasonUnhealthy,
			Message:            "Resource is unhealthy",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceInServiceReasonInService,
			Message:            "Resource is in service",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceManagedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceManagedReasonManaged,
			Message:            "Resource is managed",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceEnabledConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceEnabledReasonEnabled,
			Message:            "Resource is enabled",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceOperationalConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ResourceOperationalReasonFailed,
			Message:            "Resource has failed",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceActiveConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ResourceActiveReasonInactive,
			Message:            "Resource is not active",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceStartedConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ResourceStartedReasonStopped,
			Message:            "Resource is stopped",
			LastTransitionTime: now,
		},
		{
			Type:               v1alpha1.ResourceSchedulableConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceSchedulableReasonSchedulable,
			Message:            "Resource is schedulable",
			LastTransitionTime: now,
		},
	}
}

// =============================================================================
// Complete Node Status Fixtures
// =============================================================================

// createHealthyNodeStatus creates a healthy node status for testing
func createHealthyNodeStatus(name string, ipAddresses []string) v1alpha1.PacemakerClusterNodeStatus {
	return v1alpha1.PacemakerClusterNodeStatus{
		Conditions:  createHealthyNodeConditions(),
		Name:        name,
		IPAddresses: ipAddresses,
		Resources: []v1alpha1.PacemakerClusterResourceStatus{
			{
				Conditions: createHealthyResourceConditions(),
				Name:       v1alpha1.PacemakerClusterResourceNameKubelet,
			},
			{
				Conditions: createHealthyResourceConditions(),
				Name:       v1alpha1.PacemakerClusterResourceNameEtcd,
			},
			{
				Conditions: createHealthyResourceConditions(),
				Name:       v1alpha1.PacemakerClusterResourceNameFencingAgent,
			},
		},
	}
}

// createUnhealthyNodeStatus creates an unhealthy node status with specified unhealthy resource
func createUnhealthyNodeStatus(name string, ipAddresses []string, unhealthyResourceName v1alpha1.PacemakerClusterResourceName) v1alpha1.PacemakerClusterNodeStatus {
	now := metav1.Now()
	// Create node with unhealthy condition
	nodeConditions := createHealthyNodeConditions()
	// Mark node as unhealthy
	for i := range nodeConditions {
		if nodeConditions[i].Type == v1alpha1.NodeHealthyConditionType {
			nodeConditions[i].Status = metav1.ConditionFalse
			nodeConditions[i].Reason = v1alpha1.NodeHealthyReasonUnhealthy
			nodeConditions[i].Message = "Node has unhealthy resources"
			nodeConditions[i].LastTransitionTime = now
		}
	}

	resources := []v1alpha1.PacemakerClusterResourceStatus{
		{
			Conditions: createHealthyResourceConditions(),
			Name:       v1alpha1.PacemakerClusterResourceNameKubelet,
		},
		{
			Conditions: createHealthyResourceConditions(),
			Name:       v1alpha1.PacemakerClusterResourceNameEtcd,
		},
		{
			Conditions: createHealthyResourceConditions(),
			Name:       v1alpha1.PacemakerClusterResourceNameFencingAgent,
		},
	}

	// Mark the specified resource as unhealthy
	for i := range resources {
		if resources[i].Name == unhealthyResourceName {
			resources[i].Conditions = createUnhealthyResourceConditions()
		}
	}

	return v1alpha1.PacemakerClusterNodeStatus{
		Conditions:  nodeConditions,
		Name:        name,
		IPAddresses: ipAddresses,
		Resources:   resources,
	}
}

// =============================================================================
// Condition Lookup Helpers
// =============================================================================

// findConditionInList finds a condition by type in a list of conditions
func findConditionInList(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// findResourceInList finds a resource by name in a list of resources
func findResourceInList(resources []v1alpha1.PacemakerClusterResourceStatus, name v1alpha1.PacemakerClusterResourceName) *v1alpha1.PacemakerClusterResourceStatus {
	for i := range resources {
		if resources[i].Name == name {
			return &resources[i]
		}
	}
	return nil
}


