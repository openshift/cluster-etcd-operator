package pacemaker

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
)

// =============================================================================
// Shared Test Fixtures
//
// Reusable test data constructors for healthcheck_test.go and statuscollector_test.go.
// XML test data files are in the testdata/ directory.
// =============================================================================

// =============================================================================
// Cluster Condition Fixtures
// =============================================================================

// createHealthyClusterConditions creates healthy cluster-level conditions
func createHealthyClusterConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               pacmkrv1.ClusterHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ClusterHealthyReasonHealthy,
			Message:            "Cluster is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ClusterInServiceReasonInService,
			Message:            "Cluster is in service",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ClusterNodeCountAsExpectedReasonAsExpected,
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
			Type:               pacmkrv1.ClusterHealthyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ClusterHealthyReasonUnhealthy,
			Message:            "Cluster has issues",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ClusterInServiceReasonInService,
			Message:            "Cluster is in service",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ClusterNodeCountAsExpectedReasonInsufficientNodes,
			Message:            "Expected 2 nodes, found 1",
			LastTransitionTime: now,
		},
	}
}

// createExcessiveNodesClusterConditions creates cluster conditions with excessive nodes
func createExcessiveNodesClusterConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               pacmkrv1.ClusterHealthyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ClusterHealthyReasonUnhealthy,
			Message:            "Cluster has issues",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ClusterInServiceReasonInService,
			Message:            "In service",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ClusterNodeCountAsExpectedReasonExcessiveNodes,
			Message:            "Expected 2 nodes, found 3",
			LastTransitionTime: now,
		},
	}
}

// createMaintenanceModeClusterConditions creates cluster conditions for maintenance mode
func createMaintenanceModeClusterConditions() []metav1.Condition {
	now := metav1.Now()
	return []metav1.Condition{
		{
			Type:               pacmkrv1.ClusterHealthyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ClusterHealthyReasonUnhealthy,
			Message:            "Cluster in maintenance",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterInServiceConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ClusterInServiceReasonInMaintenance,
			Message:            "Cluster is in maintenance mode",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ClusterNodeCountAsExpectedReasonAsExpected,
			Message:            "Expected nodes present",
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
			Type:               pacmkrv1.NodeHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeHealthyReasonHealthy,
			Message:            "Node is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeOnlineConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeOnlineReasonOnline,
			Message:            "Node is online",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeInServiceReasonInService,
			Message:            "Node is in service",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeActiveConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeActiveReasonActive,
			Message:            "Node is active",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeReadyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeReadyReasonReady,
			Message:            "Node is ready",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeCleanConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeCleanReasonClean,
			Message:            "Node is clean",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeMemberConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeMemberReasonMember,
			Message:            "Node is a member",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeFencingAvailableConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeFencingAvailableReasonAvailable,
			Message:            "At least one fencing agent is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.NodeFencingHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.NodeFencingHealthyReasonHealthy,
			Message:            "All fencing agents are healthy",
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
			Type:               pacmkrv1.ResourceHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceHealthyReasonHealthy,
			Message:            "Resource is healthy",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceInServiceReasonInService,
			Message:            "Resource is in service",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceManagedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceManagedReasonManaged,
			Message:            "Resource is managed",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceEnabledConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceEnabledReasonEnabled,
			Message:            "Resource is enabled",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceOperationalConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceOperationalReasonOperational,
			Message:            "Resource is operational",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceActiveConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceActiveReasonActive,
			Message:            "Resource is active",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceStartedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceStartedReasonStarted,
			Message:            "Resource is started",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceSchedulableConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceSchedulableReasonSchedulable,
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
			Type:               pacmkrv1.ResourceHealthyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ResourceHealthyReasonUnhealthy,
			Message:            "Resource is unhealthy",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceInServiceReasonInService,
			Message:            "Resource is in service",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceManagedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceManagedReasonManaged,
			Message:            "Resource is managed",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceEnabledConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceEnabledReasonEnabled,
			Message:            "Resource is enabled",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceOperationalConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ResourceOperationalReasonFailed,
			Message:            "Resource has failed",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceActiveConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ResourceActiveReasonInactive,
			Message:            "Resource is not active",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceStartedConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pacmkrv1.ResourceStartedReasonStopped,
			Message:            "Resource is stopped",
			LastTransitionTime: now,
		},
		{
			Type:               pacmkrv1.ResourceSchedulableConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             pacmkrv1.ResourceSchedulableReasonSchedulable,
			Message:            "Resource is schedulable",
			LastTransitionTime: now,
		},
	}
}

// =============================================================================
// Complete Node Status Fixtures
// =============================================================================

// createHealthyNodeStatus creates a healthy node status for testing
func createHealthyNodeStatus(name string, ipAddresses []string) pacmkrv1.PacemakerClusterNodeStatus {
	// Convert IP strings to PacemakerNodeAddress format
	addresses := make([]pacmkrv1.PacemakerNodeAddress, len(ipAddresses))
	for i, ip := range ipAddresses {
		addresses[i] = pacmkrv1.PacemakerNodeAddress{Type: pacmkrv1.PacemakerNodeInternalIP, Address: ip}
	}

	return pacmkrv1.PacemakerClusterNodeStatus{
		Conditions: createHealthyNodeConditions(),
		NodeName:   name,
		Addresses:  addresses,
		Resources: []pacmkrv1.PacemakerClusterResourceStatus{
			{
				Conditions: createHealthyResourceConditions(),
				Name:       pacmkrv1.PacemakerClusterResourceNameKubelet,
			},
			{
				Conditions: createHealthyResourceConditions(),
				Name:       pacmkrv1.PacemakerClusterResourceNameEtcd,
			},
		},
		FencingAgents: []pacmkrv1.PacemakerClusterFencingAgentStatus{
			{
				Conditions: createHealthyResourceConditions(),
				Name:       name + "_redfish",
				Method:     pacmkrv1.FencingMethodRedfish,
			},
		},
	}
}

// createUnhealthyNodeStatus creates an unhealthy node status with specified unhealthy resource
func createUnhealthyNodeStatus(name string, ipAddresses []string, unhealthyResourceName pacmkrv1.PacemakerClusterResourceName) pacmkrv1.PacemakerClusterNodeStatus {
	now := metav1.Now()

	// Convert IP strings to PacemakerNodeAddress format
	addresses := make([]pacmkrv1.PacemakerNodeAddress, len(ipAddresses))
	for i, ip := range ipAddresses {
		addresses[i] = pacmkrv1.PacemakerNodeAddress{Type: pacmkrv1.PacemakerNodeInternalIP, Address: ip}
	}

	// Create node with unhealthy condition
	nodeConditions := createHealthyNodeConditions()
	// Mark node as unhealthy
	for i := range nodeConditions {
		if nodeConditions[i].Type == pacmkrv1.NodeHealthyConditionType {
			nodeConditions[i].Status = metav1.ConditionFalse
			nodeConditions[i].Reason = pacmkrv1.NodeHealthyReasonUnhealthy
			nodeConditions[i].Message = "Node has unhealthy resources"
			nodeConditions[i].LastTransitionTime = now
		}
	}

	resources := []pacmkrv1.PacemakerClusterResourceStatus{
		{
			Conditions: createHealthyResourceConditions(),
			Name:       pacmkrv1.PacemakerClusterResourceNameKubelet,
		},
		{
			Conditions: createHealthyResourceConditions(),
			Name:       pacmkrv1.PacemakerClusterResourceNameEtcd,
		},
	}

	// Mark the specified resource as unhealthy
	for i := range resources {
		if resources[i].Name == unhealthyResourceName {
			resources[i].Conditions = createUnhealthyResourceConditions()
		}
	}

	// Create fencing agent - healthy by default
	fencingAgents := []pacmkrv1.PacemakerClusterFencingAgentStatus{
		{
			Conditions: createHealthyResourceConditions(),
			Name:       name + "_redfish",
			Method:     pacmkrv1.FencingMethodRedfish,
		},
	}

	return pacmkrv1.PacemakerClusterNodeStatus{
		Conditions:    nodeConditions,
		NodeName:      name,
		Addresses:     addresses,
		Resources:     resources,
		FencingAgents: fencingAgents,
	}
}

// =============================================================================
// Test Lookup Helpers
// =============================================================================

// findResourceInList finds a resource by name in a list of resources
func findResourceInList(resources []pacmkrv1.PacemakerClusterResourceStatus, name pacmkrv1.PacemakerClusterResourceName) *pacmkrv1.PacemakerClusterResourceStatus {
	for i := range resources {
		if resources[i].Name == name {
			return &resources[i]
		}
	}
	return nil
}
