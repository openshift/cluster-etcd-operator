package pacemaker

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
)

// HealthStatusValue represents the overall health status of the pacemaker cluster.
type HealthStatusValue string

// Health status constants
const (
	// Status values for health assessment
	StatusHealthy HealthStatusValue = "Healthy"
	StatusWarning HealthStatusValue = "Warning"
	StatusError   HealthStatusValue = "Error"
	StatusUnknown HealthStatusValue = "Unknown"

	// Warning message prefixes for categorizing events.
	warningPrefixFencingEvent = "Recent fencing event:"

	// Error messages
	msgNoNodesFound          = "No nodes found in cluster"
	msgNodeUnhealthy         = "%s node is unhealthy: %s"
	msgNodeOffline           = "Node %s is offline"
	msgInsufficientNodes     = "Insufficient nodes in cluster (expected %d, found %d)"
	msgExcessiveNodes        = "Excessive nodes in cluster (expected %d, found %d)"
	msgClusterInMaintenance  = "Cluster is in maintenance mode"
	msgClusterUnhealthy      = "Cluster is unhealthy: %s"
	msgFencingRedundancyLost = "fencing at risk (agent running but not managed for recovery)"
)

// errorPatternsByPriority maps error message substrings to event reasons.
// Order matters: more specific patterns should appear before generic ones.
var errorPatternsByPriority = []struct {
	pattern string
	reason  string
}{
	// CR-related errors (check before more generic patterns)
	{"Failed to get PacemakerCluster CR", EventReasonCRNotFound},
	{"CR has no status", EventReasonCRNotFound},
	// Status staleness
	{"status is stale", EventReasonStatusStale},
	// Cluster maintenance mode
	{"maintenance mode", EventReasonClusterInMaintenance},
	// Node offline
	{"is offline", EventReasonNodeOffline},
	// Resource health errors (most specific health check)
	{"resource is unhealthy", EventReasonResourceUnhealthy},
	// Node unhealthy
	{"node is unhealthy", EventReasonNodeUnhealthy},
	// Cluster unhealthy (least specific among health errors)
	{"Cluster is unhealthy", EventReasonClusterUnhealthy},
	// Node count errors (both map to same reason for filtering)
	{"Insufficient nodes", EventReasonInsufficientNodes},
	{"Excessive nodes", EventReasonInsufficientNodes},
}

// HealthStatus represents the overall status of pacemaker in the ExternalEtcd cluster.
//
// Warning vs Error Philosophy:
//   - Errors: Conditions requiring immediate action or causing downstream degradation.
//     Examples: node offline, fencing unavailable, etcd/kubelet unhealthy, maintenance mode.
//   - Warnings: Degraded redundancy where the cluster remains functional but should be investigated.
//     Examples: FencingHealthy=False with FencingAvailable=True (can still fence, but redundancy lost).
//
// The operator degrades on Errors but not Warnings. Warnings generate events for observability.
type HealthStatus struct {
	OverallStatus HealthStatusValue
	Warnings      []string
	Errors        []string
	// CRLastUpdated is the timestamp from the PacemakerCluster CR's status.lastUpdated field.
	// Used for change detection (skip if unchanged) and grace period calculation (time since last valid).
	CRLastUpdated time.Time
}

// BuildHealthStatusFromCR builds a HealthStatus from the PacemakerCluster CR status fields.
// Returns nil if the CR has no status populated (indicates CR was never updated by status collector).
func BuildHealthStatusFromCR(pacemakerStatus *pacmkrv1.PacemakerCluster) *HealthStatus {
	status := &HealthStatus{
		OverallStatus: StatusUnknown,
		Warnings:      []string{},
		Errors:        []string{},
	}

	// Defensive check: this should not happen as caller should check for unpopulated Status
	if pacemakerStatus == nil || pacemakerStatus.Status.LastUpdated.IsZero() {
		status.Errors = append(status.Errors, "Internal error: unpopulated Status in BuildHealthStatusFromCR")
		return status
	}

	status.CRLastUpdated = pacemakerStatus.Status.LastUpdated.Time

	// Check cluster-level configuration issues FIRST (node count, maintenance mode)
	// These are often root causes (e.g., "excessive nodes" causes resource failures on extra node)
	checkClusterConditions(pacemakerStatus, status)

	// Check node statuses (node-level conditions and resource health)
	checkNodeStatuses(pacemakerStatus, status)

	// Determine overall status: errors > warnings > healthy
	if len(status.Errors) > 0 {
		status.OverallStatus = StatusError
	} else if len(status.Warnings) > 0 {
		status.OverallStatus = StatusWarning
	} else {
		status.OverallStatus = StatusHealthy
	}

	return status
}

// checkClusterConditions checks cluster-level configuration issues (node count, maintenance mode).
// These are ALWAYS reported regardless of node-level errors, as they often represent root causes.
// For example, "excessive nodes" causes resource failures on the extra node.
func checkClusterConditions(pacemakerStatus *pacmkrv1.PacemakerCluster, status *HealthStatus) {
	conditions := pacemakerStatus.Status.Conditions
	if len(conditions) == 0 {
		// Missing cluster conditions means we can't verify cluster health configuration
		status.Errors = append(status.Errors, "No cluster conditions available")
		return
	}

	// Always check cluster-level configuration issues - these are root causes
	clusterIssues := getClusterConditionIssues(conditions, pacemakerStatus)

	// Add cluster-level error if there are configuration issues
	if len(clusterIssues) > 0 {
		status.Errors = append(status.Errors, fmt.Sprintf(msgClusterUnhealthy, strings.Join(clusterIssues, ", ")))
	}
}

// getClusterConditionIssues returns specific issues from cluster-level conditions (non-summary conditions)
func getClusterConditionIssues(conditions []metav1.Condition, pacemakerStatus *pacmkrv1.PacemakerCluster) []string {
	var issues []string

	// Check NodeCountAsExpected condition
	nodeCountCondition := FindCondition(conditions, pacmkrv1.ClusterNodeCountAsExpectedConditionType)
	if nodeCountCondition != nil && nodeCountCondition.Status != metav1.ConditionTrue {
		nodeCount := 0
		if pacemakerStatus.Status.Nodes != nil {
			nodeCount = len(*pacemakerStatus.Status.Nodes)
		}
		switch nodeCountCondition.Reason {
		case pacmkrv1.ClusterNodeCountAsExpectedReasonInsufficientNodes:
			issues = append(issues, fmt.Sprintf(msgInsufficientNodes, ExpectedNodeCount, nodeCount))
		case pacmkrv1.ClusterNodeCountAsExpectedReasonExcessiveNodes:
			issues = append(issues, fmt.Sprintf(msgExcessiveNodes, ExpectedNodeCount, nodeCount))
		}
	}

	// Check InService condition (cluster not in maintenance mode)
	inServiceCondition := FindCondition(conditions, pacmkrv1.ClusterInServiceConditionType)
	if inServiceCondition != nil && inServiceCondition.Status != metav1.ConditionTrue {
		issues = append(issues, msgClusterInMaintenance)
	}

	return issues
}

// checkNodeStatuses checks if all nodes have healthy conditions and resources
func checkNodeStatuses(pacemakerStatus *pacmkrv1.PacemakerCluster, status *HealthStatus) {
	// Nil-guard for Nodes field - missing node data is an error (cannot verify cluster health)
	if pacemakerStatus.Status.Nodes == nil {
		status.Errors = append(status.Errors, msgNoNodesFound)
		return
	}

	nodes := *pacemakerStatus.Status.Nodes

	// Empty nodes list is also an error - cannot verify cluster health without node data
	if len(nodes) == 0 {
		status.Errors = append(status.Errors, msgNoNodesFound)
		return
	}

	// Check each node's conditions and resource health (consolidated into single error per node)
	for _, node := range nodes {
		checkNodeConditions(node, status)
	}
}

// checkNodeConditions checks the conditions of a single node and its resources,
// routing issues to errors or warnings based on severity.
// Errors degrade the operator; warnings are informational (e.g., fencing redundancy lost).
func checkNodeConditions(node pacmkrv1.PacemakerClusterNodeStatus, status *HealthStatus) {
	conditions := node.Conditions
	if len(conditions) == 0 {
		status.Warnings = append(status.Warnings, fmt.Sprintf("Node %s: missing condition metadata", node.NodeName))
		return
	}

	// Check Online condition - this is critical for degraded status
	onlineCondition := FindCondition(conditions, pacmkrv1.NodeOnlineConditionType)
	if onlineCondition != nil && onlineCondition.Status != metav1.ConditionTrue {
		status.Errors = append(status.Errors, fmt.Sprintf(msgNodeOffline, node.NodeName))
		return // If node is offline, other conditions don't matter
	}

	// Always check for fencing warnings - these are independent of overall node health.
	// Fencing redundancy degraded (FencingHealthy=False but FencingAvailable=True) is a warning
	// that should be reported even when the node is otherwise healthy.
	fencingWarnings := getFencingWarnings(conditions)
	for _, warning := range fencingWarnings {
		status.Warnings = append(status.Warnings, fmt.Sprintf("%s: %s", node.NodeName, warning))
	}

	// Check overall node Healthy condition
	healthyCondition := FindCondition(conditions, pacmkrv1.NodeHealthyConditionType)
	if healthyCondition == nil {
		status.Warnings = append(status.Warnings, fmt.Sprintf("Node %s: missing Healthy condition", node.NodeName))
		return
	}
	if healthyCondition.Status == metav1.ConditionTrue {
		return // Node is healthy (except for warnings already captured above)
	}

	// Node is unhealthy - collect errors (warnings already captured above)
	nodeErrors := getNodeConditionErrors(conditions)

	// Collect resource errors (all resource issues are currently errors)
	resourceErrors := getNodeResourceSummaries(node)

	// Combine all errors
	allErrors := append(nodeErrors, resourceErrors...)

	// If there are errors, build consolidated error message
	if len(allErrors) > 0 {
		status.Errors = append(status.Errors, fmt.Sprintf(msgNodeUnhealthy, node.NodeName, strings.Join(allErrors, ", ")))
	}

	// If no specific issues found but node is unhealthy, use generic message.
	// Skip if we already captured fencing warnings - those explain the unhealthy state.
	if len(allErrors) == 0 && len(fencingWarnings) == 0 {
		status.Errors = append(status.Errors, fmt.Sprintf(msgNodeUnhealthy, node.NodeName, healthyCondition.Message))
	}
}

// getFencingWarnings returns warnings about degraded fencing redundancy.
// This is checked independently of overall node health because fencing redundancy
// degradation should be reported even when the node is otherwise healthy.
func getFencingWarnings(conditions []metav1.Condition) []string {
	var warnings []string

	// Check FencingHealthy - if false but FencingAvailable is true, fencing redundancy is degraded (warning)
	// This is a warning because the node CAN still be fenced, just with reduced redundancy.
	fencingAvailableCondition := FindCondition(conditions, pacmkrv1.NodeFencingAvailableConditionType)
	fencingHealthyCondition := FindCondition(conditions, pacmkrv1.NodeFencingHealthyConditionType)

	if fencingHealthyCondition != nil && fencingHealthyCondition.Status != metav1.ConditionTrue {
		if fencingAvailableCondition != nil && fencingAvailableCondition.Status == metav1.ConditionTrue {
			warnings = append(warnings, msgFencingRedundancyLost)
		}
	}

	return warnings
}

// nodeConditionCheck defines a condition to check and the error message when false.
type nodeConditionCheck struct {
	conditionType string
	errorMessage  string
}

// nodeConditionChecks maps node conditions to error messages.
// Each condition that is not True generates an error.
//
// Note on FencingAvailable: Pacemaker tracks fail-count for resources. When fail-count exceeds
// migration-threshold, pacemaker marks the resource as "blocked" and stops attempting operations.
// This implementation relies on pacemaker's Operational/Schedulable conditions to detect this state.
// If delays in failure detection are observed, the fail-count thresholds for fencing agents may need
// review to ensure failures are reported within a reasonable time window.
var nodeConditionChecks = []nodeConditionCheck{
	{pacmkrv1.NodeInServiceConditionType, "in maintenance mode"},
	{pacmkrv1.NodeActiveConditionType, "in standby mode"},
	{pacmkrv1.NodeCleanConditionType, "unclean state (fencing/communication issue)"},
	{pacmkrv1.NodeMemberConditionType, "not a cluster member"},
	{pacmkrv1.NodeFencingAvailableConditionType, "fencing unavailable (no agents running)"},
}

// getNodeConditionErrors returns errors from node-level conditions.
// Errors require immediate action and cause the operator to degrade.
func getNodeConditionErrors(conditions []metav1.Condition) []string {
	var errors []string

	for _, check := range nodeConditionChecks {
		if cond := FindCondition(conditions, check.conditionType); cond != nil && cond.Status != metav1.ConditionTrue {
			errors = append(errors, check.errorMessage)
		}
	}

	// Ready/Pending is logged but not reported as an error - it's a temporary transitional state
	if cond := FindCondition(conditions, pacmkrv1.NodeReadyConditionType); cond != nil && cond.Status != metav1.ConditionTrue {
		klog.V(2).Infof("Node is pending (temporary state)")
	}

	return errors
}

// getNodeResourceSummaries returns summaries of unhealthy resources on a node.
// Each summary includes the resource name and its specific issue.
func getNodeResourceSummaries(node pacmkrv1.PacemakerClusterNodeStatus) []string {
	var summaries []string

	for _, resource := range node.Resources {
		healthyCondition := FindCondition(resource.Conditions, pacmkrv1.ResourceHealthyConditionType)
		if healthyCondition != nil && healthyCondition.Status != metav1.ConditionTrue {
			// Get specific reason for this resource
			reason := getResourceIssue(resource.Conditions)
			summaries = append(summaries, fmt.Sprintf("%s %s", resource.Name, reason))
		}
	}

	return summaries
}

// getResourceIssue returns a specific issue description for an unhealthy resource.
// All resource-level issues are treated as errors. The Active=True with Started=False state
// (anomalous transitional state) could theoretically be a warning since it might self-resolve,
// but routing resource issues to warnings vs errors would require significant refactoring.
// Additionally: (1) this state is rare and brief and (2) etcd not running causes API server
// degradation anyway, so treating it as an error is appropriate.
func getResourceIssue(conditions []metav1.Condition) string {
	// Check for specific failure conditions (prioritized by severity)
	operationalCondition := FindCondition(conditions, pacmkrv1.ResourceOperationalConditionType)
	if operationalCondition != nil && operationalCondition.Status != metav1.ConditionTrue {
		return "has failed"
	}

	startedCondition := FindCondition(conditions, pacmkrv1.ResourceStartedConditionType)
	if startedCondition != nil && startedCondition.Status != metav1.ConditionTrue {
		return "is stopped"
	}

	activeCondition := FindCondition(conditions, pacmkrv1.ResourceActiveConditionType)
	if activeCondition != nil && activeCondition.Status != metav1.ConditionTrue {
		return "is not active"
	}

	managedCondition := FindCondition(conditions, pacmkrv1.ResourceManagedConditionType)
	if managedCondition != nil && managedCondition.Status != metav1.ConditionTrue {
		return "is unmanaged"
	}

	return "is unhealthy"
}

// GetEventReasonForError determines the appropriate event reason based on error content.
// Used for both deduplication (key) and event recording.
func GetEventReasonForError(errorMsg string) string {
	for _, entry := range errorPatternsByPriority {
		if strings.Contains(errorMsg, entry.pattern) {
			return entry.reason
		}
	}
	return EventReasonGenericError
}
