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
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
)

// Constants for time windows and status strings
const (
	// Resync interval for health check controller
	healthCheckResyncInterval = 30 * time.Second

	// Event deduplication windows - avoid recording the same event within these times
	eventDeduplicationWindowFencing = 24 * time.Hour  // Fencing events logged once within 24 hour window
	eventDeduplicationWindowDefault = 5 * time.Minute // Resource actions and general warnings

	// Expected number of nodes in ExternalEtcd cluster
	expectedNodeCount = 2

	// Status strings
	statusHealthy = "Healthy"
	statusWarning = "Warning"
	statusError   = "Error"
	statusUnknown = "Unknown"

	// Resource names
	resourceKubelet = "kubelet"
	resourceEtcd    = "etcd"

	// Resource agent names
	resourceAgentKubelet = "systemd:kubelet"
	resourceAgentEtcd    = "ocf:heartbeat:podman-etcd"
	resourceAgentIPAddr  = "ocf:heartbeat:IPaddr2"

	// Degraded condition reason
	reasonPacemakerUnhealthy = "PacemakerUnhealthy"

	// PacemakerCluster CR name
	PacemakerClusterResourceName = "cluster"

	// Warning message prefixes for categorizing events
	warningPrefixFailedAction = "Recent failed resource action:"
	warningPrefixFencingEvent = "Recent fencing event:"

	// Operator condition types
	conditionTypePacemakerDegraded = "PacemakerHealthCheckDegraded"

	// Event reasons for non-degrading conditions (informational/historical)
	eventReasonFailedAction = "PacemakerFailedResourceAction"
	eventReasonFencingEvent = "PacemakerFencingEvent"
	eventReasonWarning      = "PacemakerWarning"
	eventReasonHealthy      = "PacemakerHealthy"

	// Event reasons for degrading conditions (current operational problems)
	eventReasonNoQuorum          = "PacemakerNoQuorum"
	eventReasonNodeOffline       = "PacemakerNodeOffline"
	eventReasonResourceStopped   = "PacemakerResourceStopped"
	eventReasonDaemonNotRunning  = "PacemakerDaemonNotRunning"
	eventReasonStatusStale       = "PacemakerStatusStale"
	eventReasonCollectionError   = "PacemakerCollectionError"
	eventReasonCRNotFound        = "PacemakerCRNotFound"
	eventReasonGenericError      = "PacemakerError" // Fallback for unclassified errors

	// Kubernetes API constants
	kubernetesAPIPath     = "/apis"
	pacemakerResourceName = "pacemakerclusters"

	// Time thresholds
	statusStalenessThreshold       = 5 * time.Minute // Status is stale if not updated in 5 minutes
	statusUnknownDegradedThreshold = 5 * time.Minute // How long to wait before marking degraded when status is unknown

	// Event key prefixes
	eventKeyPrefixWarning = "warning:"
	eventKeyPrefixError   = "error:"

	// Error messages
	msgPacemakerNotRunning = "Pacemaker is not running"
	msgClusterNoQuorum     = "Cluster does not have quorum"
	msgNoNodesFound        = "No nodes found"
	msgNodeOffline         = "Node %s is not online"
	msgNodeStandby         = "Node %s is in standby (unexpected behavior; treated as offline)"
	msgResourceNotStarted = "%s resource not started on all nodes (started on %d/%d nodes)"
	msgPacemakerDegraded  = "Pacemaker cluster is in degraded state"
	msgPacemakerHealthy   = "Pacemaker cluster is healthy - all nodes online and critical resources started"

	// Error message substrings for event categorization (used in recordErrorEvent)
	// These are unique substrings that identify specific error types
	errorSubstringResourceNotStarted  = "resource not started"
	errorSubstringStatusStale         = "status is stale"
	errorSubstringCollectionError     = "Status collection error"
	errorSubstringCRNotFound          = "Failed to get PacemakerCluster CR"
	errorSubstringCRNoStatus          = "CR has no status"
	errorSubstringNodeOffline         = "is not online"
	errorSubstringNodeStandby         = "is in standby"

	// Event message templates
	msgDetectedFailedAction = "Pacemaker detected a recent failed resource action: %s"
	msgDetectedFencing      = "Pacemaker detected a recent fencing event: %s"
	msgPacemakerWarning     = "Pacemaker warning: %s"
	msgPacemakerError       = "Pacemaker error: %s"
)

// HealthStatus represents the overall status of pacemaker in the ExternalEtcd cluster
type HealthStatus struct {
	OverallStatus string
	Warnings      []string
	Errors        []string
}

// newUnknownHealthStatus creates a HealthStatus with Unknown status and an error message
func newUnknownHealthStatus(errMsg string) *HealthStatus {
	return &HealthStatus{
		OverallStatus: statusUnknown,
		Warnings:      []string{},
		Errors:        []string{errMsg},
	}
}

// HealthCheck monitors pacemaker status in ExternalEtcd topology clusters
type HealthCheck struct {
	operatorClient      v1helpers.StaticPodOperatorClient
	kubeClient          kubernetes.Interface
	eventRecorder       events.Recorder
	pacemakerRESTClient rest.Interface
	pacemakerInformer   cache.SharedIndexInformer

	// Track recently recorded events to avoid duplicates
	recordedEventsMu sync.Mutex
	recordedEvents   map[string]time.Time

	// Track previous health status to determine if we should record healthy events
	previousStatusMu sync.Mutex
	previousStatus   string

	// Track last processed Pacemaker to detect changes
	lastProcessedStatusMu sync.Mutex
	lastProcessedStatus   *v1alpha1.PacemakerCluster

	// Track last time we successfully retrieved a valid (non-Unknown) status
	lastValidStatusTimeMu sync.Mutex
	lastValidStatusTime   time.Time
}

// NewHealthCheck creates a new HealthCheck for monitoring pacemaker status
// in clusters that use ExternalEtcd clusters.
// Returns the controller and the PacemakerCluster informer (which must be started separately).
func NewHealthCheck(
	livenessChecker *health.MultiAlivenessChecker,
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
	klog.Infof("Creating PacemakerCluster informer for group %s, resource %s", v1alpha1.SchemeGroupVersion.String(), pacemakerResourceName)
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				klog.V(4).Infof("PacemakerCluster informer ListFunc called for resource %s", pacemakerResourceName)
				result := &v1alpha1.PacemakerClusterList{}
				err := restClient.Get().
					Resource(pacemakerResourceName).
					VersionedParams(&options, runtime.NewParameterCodec(scheme)).
					Do(context.Background()).
					Into(result)
				if err != nil {
					klog.Errorf("Failed to list PacemakerCluster resources (%s): %v", pacemakerResourceName, err)
				} else {
					klog.V(4).Infof("Successfully listed PacemakerCluster resources, found %d items", len(result.Items))
				}
				return result, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				klog.V(4).Infof("PacemakerCluster informer WatchFunc called for resource %s", pacemakerResourceName)
				watcher, err := restClient.Get().
					Resource(pacemakerResourceName).
					VersionedParams(&options, runtime.NewParameterCodec(scheme)).
					Watch(context.Background())
				if err != nil {
					klog.Errorf("Failed to watch PacemakerCluster resources (%s): %v", pacemakerResourceName, err)
				}
				return watcher, err
			},
		},
		&v1alpha1.PacemakerCluster{},
		healthCheckResyncInterval,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	c := &HealthCheck{
		operatorClient:      operatorClient,
		kubeClient:          kubeClient,
		eventRecorder:       eventRecorder,
		pacemakerRESTClient: restClient,
		pacemakerInformer:   informer,
		recordedEvents:      make(map[string]time.Time),
		previousStatus:      statusUnknown,
		lastValidStatusTime: time.Now(), // Initialize to now to avoid immediate degraded status on startup
	}

	syncCtx := factory.NewSyncContext("PacemakerHealthCheck", eventRecorder.WithComponentSuffix("pacemaker-health-check"))

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("PacemakerHealthCheck", syncer)

	klog.Infof("PacemakerHealthCheck controller created, waiting for informers to sync before starting")
	klog.Infof("PacemakerHealthCheck will watch: operatorClient and %s/%s resource", v1alpha1.SchemeGroupVersion.String(), pacemakerResourceName)

	// ResyncEvery ensures the sync function is called at regular intervals (30s)
	// even if no informer events are detected. This prevents the aliveness checker
	// from timing out when the PacemakerStatus CR isn't being updated.
	controller := factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(healthCheckResyncInterval).
		WithSync(syncer.Sync).
		WithInformers(
			operatorClient.Informer(),
			informer,
		).ToController("PacemakerHealthCheck", syncCtx.Recorder())

	klog.Infof("PacemakerHealthCheck controller successfully created and ready to start")
	klog.Infof("PacemakerHealthCheck informers to sync: 1) operatorClient.Informer(), 2) PacemakerCluster informer")
	klog.Infof("Note: PacemakerCluster informer must be started separately before controller.Run() is called")
	klog.Infof("Note: factory.Controller will wait up to 10 minutes for informers to sync, then exit if they fail to sync")
	return controller, informer, nil
}

// sync is the main sync function that gets called periodically to check pacemaker status
func (c *HealthCheck) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("PacemakerHealthCheck sync started")
	defer klog.V(4).Infof("PacemakerHealthCheck sync completed")

	// Get pacemaker status
	healthStatus, err := c.getPacemakerStatus(ctx)
	if err != nil {
		klog.Errorf("Failed to get pacemaker status: %v", err)
		return err
	}

	// Defensive check: ensure we always have a valid health status
	if healthStatus == nil {
		klog.Errorf("getPacemakerStatus returned nil health status")
		return fmt.Errorf("internal error: nil health status")
	}

	// Log the determined status for visibility
	klog.V(2).Infof("Pacemaker health status: %s (errors: %d, warnings: %d)",
		healthStatus.OverallStatus, len(healthStatus.Errors), len(healthStatus.Warnings))

	// Update the last valid status time if status is not unknown
	if healthStatus.OverallStatus != statusUnknown {
		c.lastValidStatusTimeMu.Lock()
		c.lastValidStatusTime = time.Now()
		c.lastValidStatusTimeMu.Unlock()
	}

	// Update operator status conditions based on pacemaker status
	if err := c.updateOperatorStatus(ctx, healthStatus); err != nil {
		return err
	}

	// Record pacemaker health check events
	c.recordHealthCheckEvents(healthStatus)

	return nil
}

// getPacemakerStatus retrieves pacemaker status from the PacemakerCluster CR and returns a HealthStatus struct
func (c *HealthCheck) getPacemakerStatus(ctx context.Context) (*HealthStatus, error) {
	klog.V(4).Infof("Retrieving pacemaker status from CR...")

	// Get the PacemakerCluster CR
	pacemakerStatus := &v1alpha1.PacemakerCluster{}
	err := c.pacemakerRESTClient.Get().
		Resource(pacemakerResourceName).
		Name(PacemakerClusterResourceName).
		Do(ctx).
		Into(pacemakerStatus)

	if err != nil {
		return newUnknownHealthStatus(fmt.Sprintf("Failed to get PacemakerCluster CR: %v", err)), nil
	}

	// Check if status is populated
	if pacemakerStatus.Status == nil {
		return newUnknownHealthStatus("PacemakerCluster CR has no status populated"), nil
	}

	// Check if the status is stale (check this FIRST before collection errors)
	// We want to clear staleness whenever we get any update (even with errors)
	timeSinceUpdate := time.Since(pacemakerStatus.Status.LastUpdated.Time)
	if timeSinceUpdate > statusStalenessThreshold {
		return newUnknownHealthStatus(fmt.Sprintf("Pacemaker status is stale (last updated: %v ago)", timeSinceUpdate)), nil
	}

	// Check if there was an error collecting the status
	// Only report this if status is NOT stale (i.e., we got a recent update, but it had an error)
	if pacemakerStatus.Status.CollectionError != "" {
		return newUnknownHealthStatus(fmt.Sprintf("Status collection error: %s", pacemakerStatus.Status.CollectionError)), nil
	}

	// Store the last processed status to detect changes
	c.lastProcessedStatusMu.Lock()
	c.lastProcessedStatus = pacemakerStatus.DeepCopy()
	c.lastProcessedStatusMu.Unlock()

	// Build health status from the CRD status fields
	status := c.buildHealthStatusFromCR(pacemakerStatus)

	return status, nil
}

// buildHealthStatusFromCR builds a HealthStatus from the PacemakerCluster CR status fields
// Note: This function assumes Status is not nil (checked by caller in getPacemakerStatus)
func (c *HealthCheck) buildHealthStatusFromCR(pacemakerStatus *v1alpha1.PacemakerCluster) *HealthStatus {
	status := &HealthStatus{
		OverallStatus: statusUnknown,
		Warnings:      []string{},
		Errors:        []string{},
	}

	// Defensive check: this should not happen as getPacemakerStatus checks for nil Status
	if pacemakerStatus == nil || pacemakerStatus.Status == nil {
		klog.Errorf("buildHealthStatusFromCR called with nil PacemakerCluster or nil Status")
		status.Errors = append(status.Errors, "Internal error: nil Status in buildHealthStatusFromCR")
		return status
	}

	// Check if Summary exists and has required data
	if pacemakerStatus.Status.Summary == nil {
		klog.V(2).Infof("Pacemaker.Status.Summary is nil, status unknown")
		return status
	}

	if pacemakerStatus.Status.Summary.PacemakerDaemonState == "" {
		klog.V(2).Infof("Pacemaker.Status.Summary.PacemakerDaemonState is empty, status unknown")
		return status
	}

	// Check if pacemaker is running
	if pacemakerStatus.Status.Summary.PacemakerDaemonState != v1alpha1.PacemakerDaemonStateRunning {
		status.Errors = append(status.Errors, msgPacemakerNotRunning)
		status.OverallStatus = statusError
		return status
	}

	// Any errors collected beyond this point will set the status to Error instead of Unknown
	// Check quorum
	if pacemakerStatus.Status.Summary.QuorumStatus != v1alpha1.QuorumStatusQuorate {
		status.Errors = append(status.Errors, msgClusterNoQuorum)
	}

	// Check node status
	c.checkNodeStatus(pacemakerStatus, status)

	// Check resource status
	c.checkResourceStatus(pacemakerStatus, status)

	// Check for recent failures
	c.checkRecentFailures(pacemakerStatus, status)

	// Check for recent fencing events
	c.checkFencingEvents(pacemakerStatus, status)

	// Determine overall status
	status.OverallStatus = c.determineOverallStatus(status)

	return status
}

// checkNodeStatus checks if all nodes are online using CRD status fields
func (c *HealthCheck) checkNodeStatus(pacemakerStatus *v1alpha1.PacemakerCluster, status *HealthStatus) {
	// Nil-guard for Nodes field - missing data means status is unknown
	if pacemakerStatus.Status.Nodes == nil {
		klog.V(2).Infof("Pacemaker.Status.Nodes is nil, cannot determine node status")
		// Don't set status to unknown here, let the overall determination handle it
		return
	}

	nodes := pacemakerStatus.Status.Nodes

	// Empty nodes list indicates we don't have node information - status unknown
	if len(nodes) == 0 {
		klog.V(2).Infof("Pacemaker has no node information")
		// Don't set an error here, missing data is not an error
		return
	}

	// Check if we have the expected number of nodes.
	// This should almost always be 2, but 1 node is possible during a control-plane node replacement event.
	if len(nodes) != expectedNodeCount {
		status.Warnings = append(status.Warnings, fmt.Sprintf("Expected %d nodes, found %d", expectedNodeCount, len(nodes)))
	}

	for _, node := range nodes {
		if node.OnlineStatus != v1alpha1.NodeOnlineStatusOnline {
			status.Errors = append(status.Errors, fmt.Sprintf(msgNodeOffline, node.Name))
		}

		if node.Mode == v1alpha1.NodeModeStandby {
			status.Errors = append(status.Errors, fmt.Sprintf(msgNodeStandby, node.Name))
		}
	}
}

// checkResourceStatus checks if kubelet and etcd resources are started on both nodes using CRD status fields
func (c *HealthCheck) checkResourceStatus(pacemakerStatus *v1alpha1.PacemakerCluster, status *HealthStatus) {
	// Nil-guard for Resources field - missing data means we can't check resources
	if pacemakerStatus.Status.Resources == nil {
		klog.V(2).Infof("Pacemaker.Status.Resources is nil, cannot determine resource status")
		return
	}

	resources := pacemakerStatus.Status.Resources

	// Empty resources list means we don't have resource information - don't report errors
	if len(resources) == 0 {
		klog.V(2).Infof("PacemakerCluster has no resource information")
		return
	}

	resourcesStarted := make(map[string]map[string]bool)
	resourcesStarted[resourceKubelet] = make(map[string]bool)
	resourcesStarted[resourceEtcd] = make(map[string]bool)

	for _, resource := range resources {
		// A resource is considered "started" if it has Role="Started" and ActiveStatus="Active"
		if resource.Role == v1alpha1.ResourceRoleStarted && resource.ActiveStatus == v1alpha1.ResourceActiveStatusActive && resource.Node != "" {
			// Check if this is a kubelet or etcd resource
			switch {
			case strings.HasPrefix(resource.ResourceAgent, resourceAgentKubelet):
				resourcesStarted[resourceKubelet][resource.Node] = true
			case strings.HasPrefix(resource.ResourceAgent, resourceAgentEtcd):
				resourcesStarted[resourceEtcd][resource.Node] = true
			}
		}
	}

	// Check if we have both resources started on all nodes
	kubeletCount := len(resourcesStarted[resourceKubelet])
	etcdCount := len(resourcesStarted[resourceEtcd])

	c.validateResourceCount(resourceKubelet, kubeletCount, status)
	c.validateResourceCount(resourceEtcd, etcdCount, status)
}

// validateResourceCount validates that a resource is started on the expected number of nodes
func (c *HealthCheck) validateResourceCount(resourceName string, actualCount int, status *HealthStatus) {
	if actualCount < expectedNodeCount {
		status.Errors = append(status.Errors, fmt.Sprintf(msgResourceNotStarted,
			resourceName, actualCount, expectedNodeCount))
	}
}

// checkRecentFailures checks for recent failed resource actions using CRD status fields
func (c *HealthCheck) checkRecentFailures(pacemakerStatus *v1alpha1.PacemakerCluster, status *HealthStatus) {
	// Nil-guard for NodeHistory field
	if pacemakerStatus.Status.NodeHistory == nil {
		klog.V(4).Infof("Pacemaker.Status.NodeHistory is nil, no recent failures to check")
		return
	}

	// The status collector already filters NodeHistory to recent events (last 5 minutes)
	// and only includes failed operations (RC != 0)
	for _, entry := range pacemakerStatus.Status.NodeHistory {
		// Check for failed operations (rc != 0)
		if entry.RC != nil && *entry.RC != 0 {
			status.Warnings = append(status.Warnings, fmt.Sprintf("%s %s %s on %s failed (rc=%d, %s, %s)",
				warningPrefixFailedAction, entry.Resource, entry.Operation, entry.Node,
				*entry.RC, entry.RCText, entry.LastRCChange.Format(time.RFC3339)))
		}
	}
}

// checkFencingEvents checks for recent fencing events using CRD status fields
func (c *HealthCheck) checkFencingEvents(pacemakerStatus *v1alpha1.PacemakerCluster, status *HealthStatus) {
	// Nil-guard for FencingHistory field
	if pacemakerStatus.Status.FencingHistory == nil {
		klog.V(4).Infof("Pacemaker.Status.FencingHistory is nil, no fencing events to check")
		return
	}

	// The status collector already filters FencingHistory to recent events (last 24 hours)
	for _, fenceEvent := range pacemakerStatus.Status.FencingHistory {
		status.Warnings = append(status.Warnings, fmt.Sprintf("%s %s of %s %s",
			warningPrefixFencingEvent, fenceEvent.Action, fenceEvent.Target, fenceEvent.Status))
	}
}

// determineOverallStatus determines the overall health status based on collected information
func (c *HealthCheck) determineOverallStatus(status *HealthStatus) string {
	// Determine status based on current state
	if len(status.Errors) > 0 {
		return statusError
	}

	if len(status.Warnings) > 0 {
		return statusWarning
	}

	return statusHealthy
}

// updateOperatorStatus processes the HealthStatus and conditionally updates the operator state
func (c *HealthCheck) updateOperatorStatus(ctx context.Context, status *HealthStatus) error {
	klog.V(4).Infof("Updating operator availability based on pacemaker status: %s", status.OverallStatus)

	// Log warnings and errors
	c.logStatusMessages(status)

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
		// Only mark as degraded if we haven't received a valid status in a while (5 minutes)
		// This avoids marking degraded during transient issues or startup
		c.lastValidStatusTimeMu.Lock()
		timeSinceLastValid := time.Since(c.lastValidStatusTime)
		c.lastValidStatusTimeMu.Unlock()

		if timeSinceLastValid > statusUnknownDegradedThreshold {
			klog.Warningf("Pacemaker health check cannot determine status for %v (threshold: %v), marking as degraded: %v",
				timeSinceLastValid, statusUnknownDegradedThreshold, status.Errors)
			return c.setPacemakerDegradedCondition(ctx, status)
		}

		// Still within grace period, just log
		klog.V(2).Infof("Pacemaker health check cannot determine status for %v (threshold: %v), not marking degraded yet: %v",
			timeSinceLastValid, statusUnknownDegradedThreshold, status.Errors)
		return nil
	default:
		// This should never happen, but log it if it does
		klog.Errorf("Unexpected pacemaker health status: %s (errors: %v, warnings: %v)",
			status.OverallStatus, status.Errors, status.Warnings)
		return nil
	}
}

// logStatusMessages logs all warnings and errors
func (c *HealthCheck) logStatusMessages(status *HealthStatus) {
	for _, warning := range status.Warnings {
		klog.Warningf(msgPacemakerWarning, warning)
	}

	for _, err := range status.Errors {
		klog.Errorf(msgPacemakerError, err)
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
	// Check if the condition is currently set to True before attempting to clear it
	currentCondition, err := c.getCurrentPacemakerCondition()
	if err != nil {
		return err
	}

	// Only update if the condition is currently True (degraded)
	if currentCondition == nil || currentCondition.Status != operatorv1.ConditionTrue {
		klog.V(4).Infof("Pacemaker degraded condition is not set or already False, skipping update")
		return nil
	}

	condition := operatorv1.OperatorCondition{
		Type:   conditionTypePacemakerDegraded,
		Status: operatorv1.ConditionFalse,
	}

	err = c.updateOperatorCondition(ctx, condition)
	if err != nil {
		return err
	}

	klog.Infof("Pacemaker degraded condition cleared")
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
// Fencing events are kept for 1 hour, other events for 5 minutes.
func (c *HealthCheck) cleanupExpiredEvents() {
	c.recordedEventsMu.Lock()
	defer c.recordedEventsMu.Unlock()

	now := time.Now()

	for key, timestamp := range c.recordedEvents {
		// Determine the appropriate window based on event type
		// Fencing events have longer window
		window := eventDeduplicationWindowDefault
		if strings.Contains(key, warningPrefixFencingEvent) {
			window = eventDeduplicationWindowFencing
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

// recordHealthCheckEvents records events for pacemaker warnings and fencing history
func (c *HealthCheck) recordHealthCheckEvents(status *HealthStatus) {
	// Clean up expired events before processing new ones
	c.cleanupExpiredEvents()

	// Record events for warnings with appropriate deduplication window
	for _, warning := range status.Warnings {
		eventKey := fmt.Sprintf(eventKeyPrefixWarning+"%s", warning)
		// Use longer deduplication window for fencing events
		deduplicationWindow := eventDeduplicationWindowDefault
		if strings.Contains(warning, warningPrefixFencingEvent) {
			deduplicationWindow = eventDeduplicationWindowFencing
		}
		if c.shouldRecordEvent(eventKey, deduplicationWindow) {
			c.recordWarningEvent(warning)
		}
	}

	// Record events for errors with default deduplication window
	// Use specific event reasons based on error content for better filtering/alerting
	for _, err := range status.Errors {
		eventKey := fmt.Sprintf(eventKeyPrefixError+"%s", err)
		if c.shouldRecordEvent(eventKey, eventDeduplicationWindowDefault) {
			c.recordErrorEvent(err)
		}
	}

	// Record transition events (these bypass deduplication as they indicate state changes)
	// Transition: Error/Unknown → Healthy/Warning: PacemakerHealthy
	// Both Healthy and Warning are considered operationally healthy (non-degraded)
	var previousStatus string
	func() {
		c.previousStatusMu.Lock()
		defer c.previousStatusMu.Unlock()
		previousStatus = c.previousStatus
		c.previousStatus = status.OverallStatus
	}()

	// Detect transitions and record appropriate events (no deduplication)
	// Record PacemakerHealthy when transitioning to operationally healthy state (Healthy or Warning)
	// from a degraded/unknown state (Error or Unknown)
	currentlyHealthy := status.OverallStatus == statusHealthy || status.OverallStatus == statusWarning
	previouslyHealthy := previousStatus == statusHealthy || previousStatus == statusWarning

	if currentlyHealthy && !previouslyHealthy {
		c.eventRecorder.Eventf(eventReasonHealthy, msgPacemakerHealthy)
		klog.Infof("Pacemaker cluster is now operational (status: %s, transition from: %s)", status.OverallStatus, previousStatus)
	}
}

// recordWarningEvent records appropriate warning events based on warning type
func (c *HealthCheck) recordWarningEvent(warning string) {
	switch {
	case strings.Contains(warning, warningPrefixFailedAction):
		c.eventRecorder.Warningf(eventReasonFailedAction, msgDetectedFailedAction, warning)
	case strings.Contains(warning, warningPrefixFencingEvent):
		c.eventRecorder.Warningf(eventReasonFencingEvent, msgDetectedFencing, warning)
	default:
		c.eventRecorder.Warningf(eventReasonWarning, msgPacemakerWarning, warning)
	}
}

// recordErrorEvent records appropriate error events with specific reasons based on error content
// This allows operators to filter and alert on specific types of degrading conditions.
//
// IMPORTANT: The order of checks matters. Cases are ordered from most specific to least specific
// to avoid accidental matches. For example, we check for exact messages before checking substrings.
func (c *HealthCheck) recordErrorEvent(errorMsg string) {
	// Categorize the error and use a specific event reason for better observability
	var eventReason string
	switch {
	// Exact message matches (most specific)
	case strings.Contains(errorMsg, msgPacemakerNotRunning):
		eventReason = eventReasonDaemonNotRunning
	case strings.Contains(errorMsg, msgClusterNoQuorum):
		eventReason = eventReasonNoQuorum

	// CR-related errors (check before more generic patterns)
	case strings.Contains(errorMsg, errorSubstringCRNotFound) || strings.Contains(errorMsg, errorSubstringCRNoStatus):
		eventReason = eventReasonCRNotFound

	// Status collection errors (check before generic status checks)
	case strings.Contains(errorMsg, errorSubstringCollectionError):
		eventReason = eventReasonCollectionError
	case strings.Contains(errorMsg, errorSubstringStatusStale):
		eventReason = eventReasonStatusStale

	// Resource and node errors (check unique substrings)
	case strings.Contains(errorMsg, errorSubstringResourceNotStarted):
		eventReason = eventReasonResourceStopped
	case strings.Contains(errorMsg, errorSubstringNodeOffline) || strings.Contains(errorMsg, errorSubstringNodeStandby):
		eventReason = eventReasonNodeOffline

	// Fallback for unclassified errors
	default:
		eventReason = eventReasonGenericError
	}

	c.eventRecorder.Warningf(eventReason, msgPacemakerError, errorMsg)
}
