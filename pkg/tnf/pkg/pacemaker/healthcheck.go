package pacemaker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// HealthStatusValue represents the overall health status of the pacemaker cluster.
type HealthStatusValue string

// Local constants for healthcheck controller
const (
	// Status values for health assessment
	statusHealthy HealthStatusValue = "Healthy"
	statusWarning HealthStatusValue = "Warning"
	statusError   HealthStatusValue = "Error"
	statusUnknown HealthStatusValue = "Unknown"

	// Degraded condition reason
	reasonPacemakerUnhealthy = "PacemakerUnhealthy"

	// Warning message prefixes for categorizing events.
	// Note: warningPrefixFailedAction was removed - failed action events are recorded
	// by the status collector directly from pacemaker XML, not the healthcheck controller.
	warningPrefixFencingEvent = "Recent fencing event:"

	// Operator condition types
	conditionTypePacemakerDegraded = "PacemakerHealthCheckDegraded"

	// Event key prefixes
	eventKeyPrefixWarning = "warning:"
	eventKeyPrefixError   = "error:"

	// Error messages
	msgNoNodesFound             = "No nodes found in cluster"
	msgNodeUnhealthy            = "%s node is unhealthy: %s"
	msgNodeOffline              = "Node %s is offline"
	msgInsufficientNodes        = "Insufficient nodes in cluster (expected %d, found %d)"
	msgExcessiveNodes           = "Excessive nodes in cluster (expected %d, found %d)"
	msgClusterInMaintenance     = "Cluster is in maintenance mode"
	msgClusterUnhealthy         = "Cluster is unhealthy: %s"
	msgPacemakerDegraded        = "Pacemaker cluster is in degraded state"
	msgPacemakerHealthy         = "Pacemaker cluster is healthy - all nodes have healthy critical resources"
	msgPacemakerWarningsCleared = "Pacemaker cluster warnings cleared"
	msgFencingRedundancyLost    = "fencing at risk (agent running but not managed for recovery)"

	// Event message templates
	msgDetectedFencing  = "Pacemaker detected a recent fencing event: %s"
	msgPacemakerWarning = "Pacemaker warning: %s"
	msgPacemakerError   = "Pacemaker error: %s"
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

// HealthCheck monitors pacemaker status in ExternalEtcd topology clusters
type HealthCheck struct {
	operatorClient    v1helpers.StaticPodOperatorClient
	kubeClient        kubernetes.Interface
	eventRecorder     events.Recorder
	pacemakerInformer cache.SharedIndexInformer

	// Event deduplication: tracks recently recorded events to avoid duplicates
	recordedEventsMu sync.Mutex
	recordedEvents   map[string]time.Time

	// Previous health status for transition detection and grace period calculation.
	// Only updated when status is non-Unknown, so CRLastUpdated reflects the last valid CR timestamp.
	previousMu sync.Mutex
	previous   *HealthStatus
}

// NewHealthCheck creates a new HealthCheck for monitoring pacemaker status
// in clusters that use ExternalEtcd clusters.
// Returns the controller and the PacemakerCluster informer (which must be started separately).
func NewHealthCheck(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	restConfig *rest.Config,
) (factory.Controller, cache.SharedIndexInformer, error) {
	// Create REST client for PacemakerStatus CRs
	restClient, err := createPacemakerRESTClient(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create REST client: %w", err)
	}

	// Create scheme for the parameter codec
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to add scheme for informer: %w", err)
	}

	// Create informer for PacemakerCluster
	klog.Infof("Creating PacemakerCluster informer for group %s, resource %s", v1alpha1.SchemeGroupVersion.String(), PacemakerResourceName)
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				klog.V(4).Infof("PacemakerCluster informer ListFunc called for resource %s", PacemakerResourceName)
				result := &v1alpha1.PacemakerClusterList{}
				err := restClient.Get().
					Resource(PacemakerResourceName).
					VersionedParams(&options, runtime.NewParameterCodec(scheme)).
					Do(context.Background()).
					Into(result)
				if err != nil {
					klog.Errorf("Failed to list PacemakerCluster resources (%s): %v", PacemakerResourceName, err)
				} else {
					klog.V(4).Infof("Successfully listed PacemakerCluster resources, found %d items", len(result.Items))
				}
				return result, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				klog.V(4).Infof("PacemakerCluster informer WatchFunc called for resource %s", PacemakerResourceName)
				watcher, err := restClient.Get().
					Resource(PacemakerResourceName).
					VersionedParams(&options, runtime.NewParameterCodec(scheme)).
					Watch(context.Background())
				if err != nil {
					klog.Errorf("Failed to watch PacemakerCluster resources (%s): %v", PacemakerResourceName, err)
				}
				return watcher, err
			},
		},
		&v1alpha1.PacemakerCluster{},
		HealthCheckResyncInterval,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	c := &HealthCheck{
		operatorClient:    operatorClient,
		kubeClient:        kubeClient,
		eventRecorder:     eventRecorder,
		pacemakerInformer: informer,
		recordedEvents:    make(map[string]time.Time),
		// previous starts as nil - first sync will be treated as "from Unknown"
	}

	syncCtx := factory.NewSyncContext("PacemakerHealthCheck", eventRecorder.WithComponentSuffix("pacemaker-health-check"))

	klog.Infof("PacemakerHealthCheck controller created, waiting for informers to sync before starting")
	klog.Infof("PacemakerHealthCheck will watch: operatorClient and %s/%s resource", v1alpha1.SchemeGroupVersion.String(), PacemakerResourceName)

	// ResyncEvery ensures the sync function is called at regular intervals (30s)
	// even if no informer events are detected.
	controller := factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(HealthCheckResyncInterval).
		WithSync(c.sync).
		WithInformers(
			operatorClient.Informer(),
			informer,
		).ToController("PacemakerHealthCheck", syncCtx.Recorder())

	klog.Infof("PacemakerHealthCheck controller successfully created and ready to start")
	klog.Infof("PacemakerHealthCheck informers to sync: 1) operatorClient.Informer(), 2) PacemakerCluster informer")
	klog.Infof("PacemakerCluster informer must be started separately before controller.Run() is called")
	klog.Infof("factory.Controller will wait up to 10 minutes for informers to sync, then exit if they fail to sync")
	return controller, informer, nil
}

// sync is the main sync function that gets called periodically to check pacemaker status
func (c *HealthCheck) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("PacemakerHealthCheck sync started")
	defer klog.V(4).Infof("PacemakerHealthCheck sync completed")

	// Get current and previous health status for transition detection
	currentStatus, previousStatus, err := c.getPacemakerStatus(ctx)
	if err != nil {
		klog.Errorf("Failed to get pacemaker status: %v", err)
		return err
	}

	// nil status means the CR hasn't changed since last sync (same lastUpdated).
	// Skip processing to avoid redundant operator status updates and event recording.
	if currentStatus == nil {
		return nil
	}

	// Log the determined status for visibility
	klog.V(2).Infof("Pacemaker health status: %s (errors: %d, warnings: %d)",
		currentStatus.OverallStatus, len(currentStatus.Errors), len(currentStatus.Warnings))

	if err := c.updateOperatorStatus(ctx, currentStatus, previousStatus); err != nil {
		return err
	}

	c.recordHealthCheckEvents(currentStatus, previousStatus)

	return nil
}

// getPacemakerStatus retrieves pacemaker status from the PacemakerCluster CR.
// Returns (currentStatus, previousStatus, nil) on success.
// Returns (nil, nil, nil) if the CR hasn't changed since last sync (same lastUpdated timestamp).
// For Unknown status, previous is not updated (preserves last valid status for grace period).
func (c *HealthCheck) getPacemakerStatus(ctx context.Context) (*HealthStatus, *HealthStatus, error) {
	klog.V(4).Infof("Retrieving pacemaker status from CR...")

	// Read previous status
	c.previousMu.Lock()
	previous := c.previous
	c.previousMu.Unlock()

	// Get the PacemakerCluster CR from the informer cache
	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(PacemakerClusterResourceName)
	if err != nil {
		// Unknown status - don't update previous (preserves last valid for grace period)
		return &HealthStatus{
			OverallStatus: statusUnknown,
			Warnings:      []string{},
			Errors:        []string{fmt.Sprintf("Failed to get PacemakerCluster CR from cache: %v", err)},
		}, previous, nil
	}
	if !exists {
		// Unknown status - CR not found in cache
		return &HealthStatus{
			OverallStatus: statusUnknown,
			Warnings:      []string{},
			Errors:        []string{"PacemakerCluster CR not found in cache"},
		}, previous, nil
	}

	pacemakerCR, ok := item.(*v1alpha1.PacemakerCluster)
	if !ok {
		return &HealthStatus{
			OverallStatus: statusUnknown,
			Warnings:      []string{},
			Errors:        []string{"Failed to convert cached item to PacemakerCluster"},
		}, previous, nil
	}

	// Check if status is populated (LastUpdated is zero means status was never set)
	if pacemakerCR.Status.LastUpdated.IsZero() {
		// Unknown status - don't update previous
		return &HealthStatus{
			OverallStatus: statusUnknown,
			Warnings:      []string{},
			Errors:        []string{"PacemakerCluster CR has no status populated"},
		}, previous, nil
	}

	crLastUpdated := pacemakerCR.Status.LastUpdated.Time

	// Check staleness first - any update (even with errors) clears staleness.
	timeSinceUpdate := time.Since(crLastUpdated)
	if timeSinceUpdate > StatusStalenessThreshold {
		// Unknown status - don't update previous
		// Use absolute timestamp (stable) for event deduplication.
		return &HealthStatus{
			OverallStatus: statusUnknown,
			Warnings:      []string{},
			Errors:        []string{fmt.Sprintf("Pacemaker status is stale (last updated: %s)", crLastUpdated.Format(time.RFC3339))},
		}, previous, nil
	}

	// Check if lastUpdated timestamp has changed since last sync.
	// If unchanged, skip processing to avoid redundant work on timer-triggered syncs.
	// Use previous.CRLastUpdated for comparison (only set for non-Unknown status).
	if previous != nil && !previous.CRLastUpdated.IsZero() && crLastUpdated.Equal(previous.CRLastUpdated) {
		klog.V(4).Infof("Skipping sync: lastUpdated timestamp unchanged (%v)", crLastUpdated)
		return nil, nil, nil
	}

	// Build health status from the CRD status fields
	currentStatus := c.buildHealthStatusFromCR(pacemakerCR)
	currentStatus.CRLastUpdated = crLastUpdated

	// Only update previous for non-Unknown status (preserves last valid for grace period)
	if currentStatus.OverallStatus != statusUnknown {
		c.previousMu.Lock()
		c.previous = currentStatus
		c.previousMu.Unlock()
	}

	return currentStatus, previous, nil
}

// buildHealthStatusFromCR builds a HealthStatus from the PacemakerCluster CR status fields
// Note: This function assumes Status is not nil (checked by caller in getPacemakerStatus)
func (c *HealthCheck) buildHealthStatusFromCR(pacemakerStatus *v1alpha1.PacemakerCluster) *HealthStatus {
	status := &HealthStatus{
		OverallStatus: statusUnknown,
		Warnings:      []string{},
		Errors:        []string{},
	}

	// Defensive check: this should not happen as getPacemakerStatus checks for unpopulated Status
	if pacemakerStatus == nil || pacemakerStatus.Status.LastUpdated.IsZero() {
		klog.Errorf("buildHealthStatusFromCR called with nil PacemakerCluster or unpopulated Status")
		status.Errors = append(status.Errors, "Internal error: unpopulated Status in buildHealthStatusFromCR")
		return status
	}

	// Check cluster-level configuration issues FIRST (node count, maintenance mode)
	// These are often root causes (e.g., "excessive nodes" causes resource failures on extra node)
	c.checkClusterConditions(pacemakerStatus, status)

	// Check node statuses (node-level conditions and resource health)
	c.checkNodeStatuses(pacemakerStatus, status)

	// Determine overall status: errors > warnings > healthy
	if len(status.Errors) > 0 {
		status.OverallStatus = statusError
	} else if len(status.Warnings) > 0 {
		status.OverallStatus = statusWarning
	} else {
		status.OverallStatus = statusHealthy
	}

	return status
}

// checkClusterConditions checks cluster-level configuration issues (node count, maintenance mode).
// These are ALWAYS reported regardless of node-level errors, as they often represent root causes.
// For example, "excessive nodes" causes resource failures on the extra node.
func (c *HealthCheck) checkClusterConditions(pacemakerStatus *v1alpha1.PacemakerCluster, status *HealthStatus) {
	conditions := pacemakerStatus.Status.Conditions
	if len(conditions) == 0 {
		// Missing cluster conditions means we can't verify cluster health configuration
		klog.V(2).Infof("No cluster conditions present in status")
		status.Errors = append(status.Errors, "No cluster conditions available")
		return
	}

	// Always check cluster-level configuration issues - these are root causes
	clusterIssues := c.getClusterConditionIssues(conditions, pacemakerStatus)

	// Add cluster-level error if there are configuration issues
	if len(clusterIssues) > 0 {
		status.Errors = append(status.Errors, fmt.Sprintf(msgClusterUnhealthy, strings.Join(clusterIssues, ", ")))
	}
}

// getClusterConditionIssues returns specific issues from cluster-level conditions (non-summary conditions)
func (c *HealthCheck) getClusterConditionIssues(conditions []metav1.Condition, pacemakerStatus *v1alpha1.PacemakerCluster) []string {
	var issues []string

	// Check NodeCountAsExpected condition
	nodeCountCondition := FindCondition(conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	if nodeCountCondition != nil && nodeCountCondition.Status != metav1.ConditionTrue {
		nodeCount := 0
		if pacemakerStatus.Status.Nodes != nil {
			nodeCount = len(*pacemakerStatus.Status.Nodes)
		}
		switch nodeCountCondition.Reason {
		case v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes:
			issues = append(issues, fmt.Sprintf(msgInsufficientNodes, ExpectedNodeCount, nodeCount))
		case v1alpha1.ClusterNodeCountAsExpectedReasonExcessiveNodes:
			issues = append(issues, fmt.Sprintf(msgExcessiveNodes, ExpectedNodeCount, nodeCount))
		}
	}

	// Check InService condition (cluster not in maintenance mode)
	inServiceCondition := FindCondition(conditions, v1alpha1.ClusterInServiceConditionType)
	if inServiceCondition != nil && inServiceCondition.Status != metav1.ConditionTrue {
		issues = append(issues, msgClusterInMaintenance)
	}

	return issues
}

// checkNodeStatuses checks if all nodes have healthy conditions and resources
func (c *HealthCheck) checkNodeStatuses(pacemakerStatus *v1alpha1.PacemakerCluster, status *HealthStatus) {
	// Nil-guard for Nodes field - missing node data is an error (cannot verify cluster health)
	if pacemakerStatus.Status.Nodes == nil {
		klog.V(2).Infof("Pacemaker.Status.Nodes is nil, cannot determine node status")
		status.Errors = append(status.Errors, msgNoNodesFound)
		return
	}

	nodes := *pacemakerStatus.Status.Nodes

	// Empty nodes list is also an error - cannot verify cluster health without node data
	if len(nodes) == 0 {
		klog.V(2).Infof("Pacemaker has no node information")
		status.Errors = append(status.Errors, msgNoNodesFound)
		return
	}

	// Check each node's conditions and resource health (consolidated into single error per node)
	for _, node := range nodes {
		c.checkNodeConditions(node, status)
	}
}

// checkNodeConditions checks the conditions of a single node and its resources,
// routing issues to errors or warnings based on severity.
// Errors degrade the operator; warnings are informational (e.g., fencing redundancy lost).
func (c *HealthCheck) checkNodeConditions(node v1alpha1.PacemakerClusterNodeStatus, status *HealthStatus) {
	conditions := node.Conditions
	if len(conditions) == 0 {
		klog.V(2).Infof("Node %s has no conditions", node.NodeName)
		return
	}

	// Check Online condition - this is critical for degraded status
	onlineCondition := FindCondition(conditions, v1alpha1.NodeOnlineConditionType)
	if onlineCondition != nil && onlineCondition.Status != metav1.ConditionTrue {
		status.Errors = append(status.Errors, fmt.Sprintf(msgNodeOffline, node.NodeName))
		return // If node is offline, other conditions don't matter
	}

	// Always check for fencing warnings - these are independent of overall node health.
	// Fencing redundancy degraded (FencingHealthy=False but FencingAvailable=True) is a warning
	// that should be reported even when the node is otherwise healthy.
	fencingWarnings := c.getFencingWarnings(conditions)
	for _, warning := range fencingWarnings {
		status.Warnings = append(status.Warnings, fmt.Sprintf("%s: %s", node.NodeName, warning))
	}

	// Check overall node Healthy condition
	healthyCondition := FindCondition(conditions, v1alpha1.NodeHealthyConditionType)
	if healthyCondition == nil || healthyCondition.Status == metav1.ConditionTrue {
		return // Node is healthy (except for warnings already captured above)
	}

	// Node is unhealthy - collect errors (warnings already captured above)
	nodeErrors := c.getNodeConditionErrors(conditions)

	// Collect resource errors (all resource issues are currently errors)
	resourceErrors := c.getNodeResourceSummaries(node)

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
func (c *HealthCheck) getFencingWarnings(conditions []metav1.Condition) []string {
	var warnings []string

	// Check FencingHealthy - if false but FencingAvailable is true, fencing redundancy is degraded (warning)
	// This is a warning because the node CAN still be fenced, just with reduced redundancy.
	fencingAvailableCondition := FindCondition(conditions, v1alpha1.NodeFencingAvailableConditionType)
	fencingHealthyCondition := FindCondition(conditions, v1alpha1.NodeFencingHealthyConditionType)

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
	{v1alpha1.NodeInServiceConditionType, "in maintenance mode"},
	{v1alpha1.NodeActiveConditionType, "in standby mode"},
	{v1alpha1.NodeCleanConditionType, "unclean state (fencing/communication issue)"},
	{v1alpha1.NodeMemberConditionType, "not a cluster member"},
	{v1alpha1.NodeFencingAvailableConditionType, "fencing unavailable (no agents running)"},
}

// getNodeConditionErrors returns errors from node-level conditions.
// Errors require immediate action and cause the operator to degrade.
func (c *HealthCheck) getNodeConditionErrors(conditions []metav1.Condition) []string {
	var errors []string

	for _, check := range nodeConditionChecks {
		if cond := FindCondition(conditions, check.conditionType); cond != nil && cond.Status != metav1.ConditionTrue {
			errors = append(errors, check.errorMessage)
		}
	}

	// Ready/Pending is logged but not reported - it's a temporary transitional state
	if cond := FindCondition(conditions, v1alpha1.NodeReadyConditionType); cond != nil && cond.Status != metav1.ConditionTrue {
		klog.V(2).Infof("Node is pending (temporary state)")
	}

	return errors
}

// getNodeResourceSummaries returns summaries of unhealthy resources on a node.
// Each summary includes the resource name and its specific issue.
func (c *HealthCheck) getNodeResourceSummaries(node v1alpha1.PacemakerClusterNodeStatus) []string {
	var summaries []string

	for _, resource := range node.Resources {
		healthyCondition := FindCondition(resource.Conditions, v1alpha1.ResourceHealthyConditionType)
		if healthyCondition != nil && healthyCondition.Status != metav1.ConditionTrue {
			// Get specific reason for this resource
			reason := c.getResourceIssue(resource.Conditions)
			summaries = append(summaries, fmt.Sprintf("%s %s", resource.Name, reason))
		}
	}

	return summaries
}

// getResourceIssue returns a specific issue description for an unhealthy resource.
//
// All resource-level issues are treated as errors. The Active=True with Started=False state
// (anomalous transitional state) could theoretically be a warning since it might self-resolve,
// but routing resource issues to warnings vs errors would require significant refactoring.
// Additionally: (1) this state is rare and brief and (2) etcd not running causes API server
// degradation anyway, so treating it as an error is appropriate.
func (c *HealthCheck) getResourceIssue(conditions []metav1.Condition) string {
	// Check for specific failure conditions (prioritized by severity)
	operationalCondition := FindCondition(conditions, v1alpha1.ResourceOperationalConditionType)
	if operationalCondition != nil && operationalCondition.Status != metav1.ConditionTrue {
		return "has failed"
	}

	startedCondition := FindCondition(conditions, v1alpha1.ResourceStartedConditionType)
	if startedCondition != nil && startedCondition.Status != metav1.ConditionTrue {
		return "is stopped"
	}

	activeCondition := FindCondition(conditions, v1alpha1.ResourceActiveConditionType)
	if activeCondition != nil && activeCondition.Status != metav1.ConditionTrue {
		return "is not active"
	}

	managedCondition := FindCondition(conditions, v1alpha1.ResourceManagedConditionType)
	if managedCondition != nil && managedCondition.Status != metav1.ConditionTrue {
		return "is unmanaged"
	}

	return "is unhealthy"
}

// updateOperatorStatus processes the HealthStatus and conditionally updates the operator state.
// previous is the last non-Unknown health status (used for grace period calculation).
func (c *HealthCheck) updateOperatorStatus(ctx context.Context, status *HealthStatus, previous *HealthStatus) error {
	klog.V(4).Infof("Updating operator availability based on pacemaker status: %s", status.OverallStatus)

	// Log warnings and errors for visibility
	for _, warning := range status.Warnings {
		klog.Warningf(msgPacemakerWarning, warning)
	}
	for _, err := range status.Errors {
		klog.Errorf(msgPacemakerError, err)
	}

	// Update operator conditions based on pacemaker status
	switch status.OverallStatus {
	case statusError:
		return c.setPacemakerDegradedCondition(ctx, status)
	case statusHealthy, statusWarning:
		// Both healthy and warning states should clear degraded condition
		// Warnings are informational (e.g. recent fencing, node count mismatch) and don't indicate degradation
		if status.OverallStatus == statusWarning {
			klog.V(2).Infof("Pacemaker health check has warnings but cluster is operational: %v", status.Warnings)
		}
		return c.clearPacemakerDegradedCondition(ctx, status)
	case statusUnknown:
		// Unknown status means we cannot determine pacemaker health (CR not found, stale, no status, etc.)
		// Only mark as degraded if we haven't received a valid status in a while (grace period).
		// Use previous.CRLastUpdated which reflects when we last had valid cluster data.
		// If previous is nil (first sync), don't degrade immediately.
		if previous == nil || previous.CRLastUpdated.IsZero() {
			klog.V(2).Infof("Pacemaker health check cannot determine status (no previous valid status), not marking degraded yet: %v",
				status.Errors)
			return nil
		}

		timeSinceLastValid := time.Since(previous.CRLastUpdated)
		if timeSinceLastValid > StatusUnknownDegradedThreshold {
			klog.Warningf("Pacemaker health check cannot determine status for %v (threshold: %v), marking as degraded: %v",
				timeSinceLastValid, StatusUnknownDegradedThreshold, status.Errors)
			return c.setPacemakerDegradedCondition(ctx, status)
		}

		// Still within grace period, just log
		klog.V(2).Infof("Pacemaker health check cannot determine status for %v (threshold: %v), not marking degraded yet: %v",
			timeSinceLastValid, StatusUnknownDegradedThreshold, status.Errors)
		return nil
	default:
		// This should never happen, but log it if it does
		klog.Errorf("Unexpected pacemaker health status: %s (errors: %v, warnings: %v)",
			status.OverallStatus, status.Errors, status.Warnings)
		return nil
	}
}

func (c *HealthCheck) setPacemakerDegradedCondition(ctx context.Context, status *HealthStatus) error {
	message := strings.Join(status.Errors, "; ")
	if message == "" {
		message = msgPacemakerDegraded
	}

	// Check if the condition is already set with the same message
	currentCondition, err := c.getCurrentPacemakerCondition()
	if err != nil {
		return err
	}

	// Only update if the condition is not already set to True with the same message
	if currentCondition != nil &&
		currentCondition.Status == operatorv1.ConditionTrue &&
		currentCondition.Message == message {
		klog.V(4).Infof("Pacemaker degraded condition already set with same message, skipping update")
		return nil
	}

	condition := operatorv1.OperatorCondition{
		Type:    conditionTypePacemakerDegraded,
		Status:  operatorv1.ConditionTrue,
		Reason:  reasonPacemakerUnhealthy,
		Message: message,
	}

	return c.updateOperatorCondition(ctx, condition)
}

func (c *HealthCheck) clearPacemakerDegradedCondition(ctx context.Context, status *HealthStatus) error {
	// Check the current condition state
	currentCondition, err := c.getCurrentPacemakerCondition()
	if err != nil {
		return err
	}

	// Skip if condition is already explicitly set to False (no change needed)
	if currentCondition != nil && currentCondition.Status == operatorv1.ConditionFalse {
		klog.V(4).Infof("Pacemaker degraded condition is already False, skipping update")
		return nil
	}

	// Set condition to False in two cases:
	// 1. Condition doesn't exist yet (first healthy check - make health status explicit)
	// 2. Condition is currently True (transitioning from degraded to healthy)
	condition := operatorv1.OperatorCondition{
		Type:   conditionTypePacemakerDegraded,
		Status: operatorv1.ConditionFalse,
	}

	err = c.updateOperatorCondition(ctx, condition)
	if err != nil {
		return err
	}

	if currentCondition == nil {
		klog.Infof("Pacemaker health check condition initialized: PacemakerHealthCheckDegraded=False")
	} else {
		klog.Infof("Pacemaker degraded condition cleared (transitioned from True to False)")
	}
	return nil
}

// getCurrentPacemakerCondition retrieves the current PacemakerHealthCheckDegraded condition
func (c *HealthCheck) getCurrentPacemakerCondition() (*operatorv1.OperatorCondition, error) {
	_, currentStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return nil, fmt.Errorf("failed to get current operator status: %w", err)
	}

	// Find the current pacemaker degraded condition
	for i := range currentStatus.Conditions {
		if currentStatus.Conditions[i].Type == conditionTypePacemakerDegraded {
			return &currentStatus.Conditions[i], nil
		}
	}

	return nil, nil
}

// updateOperatorCondition updates the operator condition
func (c *HealthCheck) updateOperatorCondition(ctx context.Context, condition operatorv1.OperatorCondition) error {
	_, _, err := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition))
	if err != nil {
		klog.Errorf("Failed to update operator status: %v", err)
		return err
	}

	klog.V(2).Infof("Updated operator condition: %s=%s", condition.Type, condition.Status)
	return nil
}

// cleanupExpiredEvents removes events from the deduplication map that have exceeded their window.
// Fencing events are kept for 24 hours, other events for 5 minutes.
func (c *HealthCheck) cleanupExpiredEvents() {
	c.recordedEventsMu.Lock()
	defer c.recordedEventsMu.Unlock()

	now := time.Now()

	for key, timestamp := range c.recordedEvents {
		// Determine the appropriate window based on event type
		// Fencing events have longer window
		window := EventDeduplicationWindowDefault
		if strings.Contains(key, warningPrefixFencingEvent) {
			window = EventDeduplicationWindowFencing
		}

		// Remove if expired
		if now.Sub(timestamp) > window {
			delete(c.recordedEvents, key)
		}
	}
}

// shouldRecordEvent checks if an event should be recorded based on deduplication logic.
// This should be called after cleanupExpiredEvents() has been called.
func (c *HealthCheck) shouldRecordEvent(eventKey string, deduplicationWindow time.Duration) bool {
	c.recordedEventsMu.Lock()
	defer c.recordedEventsMu.Unlock()

	now := time.Now()

	// Check if this event was recently recorded
	if lastRecorded, exists := c.recordedEvents[eventKey]; exists {
		if now.Sub(lastRecorded) < deduplicationWindow {
			return false
		}
	}

	// Record this event
	c.recordedEvents[eventKey] = now
	return true
}

// recordHealthCheckEvents records events for warnings, errors, and health transitions.
// current is the newly computed health status, previous is the last health status (may be nil on first sync).
func (c *HealthCheck) recordHealthCheckEvents(current *HealthStatus, previous *HealthStatus) {
	// Clean up expired events before processing new ones
	c.cleanupExpiredEvents()

	// Record events for warnings with appropriate deduplication window
	for _, warning := range current.Warnings {
		eventKey := fmt.Sprintf(eventKeyPrefixWarning+"%s", warning)
		// Use longer deduplication window for fencing events
		deduplicationWindow := EventDeduplicationWindowDefault
		if strings.Contains(warning, warningPrefixFencingEvent) {
			deduplicationWindow = EventDeduplicationWindowFencing
		}
		if c.shouldRecordEvent(eventKey, deduplicationWindow) {
			c.recordWarningEvent(warning)
		}
	}

	// Record events for errors with default deduplication window
	for _, err := range current.Errors {
		eventKey := fmt.Sprintf(eventKeyPrefixError+"%s", err)
		if c.shouldRecordEvent(eventKey, EventDeduplicationWindowDefault) {
			c.recordErrorEvent(err)
		}
	}

	// Track and detect health transitions
	c.recordHealthTransitionEvents(current, previous)
}

// recordHealthTransitionEvents detects and records health state transitions.
// - PacemakerHealthy: fires on transition to operationally healthy from degraded OR unknown state
// - PacemakerWarningsCleared: fires when warnings are resolved, regardless of overall health
func (c *HealthCheck) recordHealthTransitionEvents(current *HealthStatus, previous *HealthStatus) {
	// Determine previous state from the previous HealthStatus we computed.
	// This is derived from the previous PacemakerCluster CR we processed.
	previousWasUnknown := previous == nil || previous.OverallStatus == statusUnknown
	previousWasDegraded := previous != nil && previous.OverallStatus == statusError
	previousHadWarnings := previous != nil && len(previous.Warnings) > 0

	// Determine current state
	operationallyHealthy := current.OverallStatus == statusHealthy || current.OverallStatus == statusWarning
	currentHasNoWarnings := len(current.Warnings) == 0

	// Record PacemakerHealthy when transitioning to operationally healthy from:
	// 1. Error state (previousWasDegraded) - recovering from known problems
	// 2. Unknown state (previousWasUnknown) - initial healthy status or recovering from stale/missing CR
	// Both Healthy and Warning are "operationally healthy" (cluster is functional).
	if operationallyHealthy && (previousWasDegraded || previousWasUnknown) {
		c.eventRecorder.Eventf(EventReasonHealthy, msgPacemakerHealthy)
		klog.Infof("Pacemaker cluster transitioned to healthy (status: %s, fromDegraded: %v, fromUnknown: %v)",
			current.OverallStatus, previousWasDegraded, previousWasUnknown)
	}

	// Record PacemakerWarningsCleared when warnings are resolved.
	// This fires whenever the previous status had warnings and the current one doesn't,
	// regardless of overall cluster health. Warnings are independent of error state.
	// Examples:
	// - Warning → Healthy: warnings resolved, cluster fully healthy
	// - Error+Warning → Error: warnings resolved, but cluster still degraded for other reasons
	// - Error+Warning → Healthy: warnings resolved AND cluster recovered (fires both events)
	if previousHadWarnings && currentHasNoWarnings {
		c.eventRecorder.Eventf(EventReasonWarningsCleared, msgPacemakerWarningsCleared)
		klog.Infof("Pacemaker cluster warnings cleared")
	}
}

// recordWarningEvent records appropriate warning events based on warning type.
// Note: Failed action events (warningPrefixFailedAction) are recorded by the status collector
// directly from pacemaker XML, not by the healthcheck controller. The PacemakerCluster CR
// only contains conditions, not raw failed action history.
func (c *HealthCheck) recordWarningEvent(warning string) {
	switch {
	case strings.Contains(warning, warningPrefixFencingEvent):
		c.eventRecorder.Warningf(EventReasonFencingEvent, msgDetectedFencing, warning)
	default:
		c.eventRecorder.Warningf(EventReasonWarning, msgPacemakerWarning, warning)
	}
}

// getEventReasonForError determines the appropriate event reason based on error content.
// Used for both deduplication (key) and event recording.
func getEventReasonForError(errorMsg string) string {
	for _, entry := range errorPatternsByPriority {
		if strings.Contains(errorMsg, entry.pattern) {
			return entry.reason
		}
	}
	return EventReasonGenericError
}

// recordErrorEvent records appropriate error events with specific reasons based on error content.
// This allows operators to filter and alert on specific types of degrading conditions.
func (c *HealthCheck) recordErrorEvent(errorMsg string) {
	eventReason := getEventReasonForError(errorMsg)
	c.eventRecorder.Warningf(eventReason, msgPacemakerError, errorMsg)
}
