package pacemaker

import (
	"context"
	"encoding/xml"
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
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pacemaker/v1alpha1"
)

// Constants for time windows and status strings
const (
	// Time windows for detecting recent events
	failedActionTimeWindow = 5 * time.Minute
	fencingEventTimeWindow = 24 * time.Hour

	// Resync interval for health check controller
	healthCheckResyncInterval = 30 * time.Second

	// Event deduplication windows - avoid recording the same event within these times
	eventDeduplicationWindowFencing = 1 * time.Hour   // Fencing events create reminders every hour for 24 hours
	eventDeduplicationWindowDefault = 5 * time.Minute // Resource actions and general warnings

	// Expected number of nodes in ExternalEtcd cluster
	expectedNodeCount = 2

	// Status strings
	statusHealthy = "Healthy"
	statusWarning = "Warning"
	statusError   = "Error"
	statusUnknown = "Unknown"

	// Pacemaker state
	pacemakerStateRunning = "running"

	// Resource names
	resourceKubelet = "kubelet"
	resourceEtcd    = "etcd"

	// Resource agent names
	resourceAgentKubelet = "systemd:kubelet"
	resourceAgentEtcd    = "ocf:heartbeat:podman-etcd"

	// Node status indicators
	nodeStatusStarted = "Started"

	// Operation status indicators
	operationRCSuccess     = "0"
	operationRCTextSuccess = "ok"

	// Quorum and active status
	booleanValueTrue  = "true"
	booleanValueFalse = "false"

	// Degraded condition reason
	reasonPacemakerUnhealthy = "PacemakerUnhealthy"

	// Maximum XML size to prevent XML bombs (10MB)
	maxXMLSize = 10 * 1024 * 1024

	// Time formats for parsing Pacemaker timestamps
	// Go's reference time: Mon Jan 2 15:04:05 MST 2006 (mnemonic: 1 2 3 4 5 6 7)
	pacemakerTimeFormat      = "Mon Jan 2 15:04:05 2006"     // Format for operation history timestamps
	pacemakerFenceTimeFormat = "2006-01-02 15:04:05.000000Z" // Format for fence event timestamps

	// PacemakerStatus CR name
	pacemakerStatusName = "cluster"

	// Warning message prefixes for categorizing events
	warningPrefixFailedAction = "Recent failed resource action:"
	warningPrefixFencingEvent = "Recent fencing event:"

	// Operator condition types
	conditionTypeDegraded = "PacemakerHealthCheckDegraded"

	// Event reasons
	eventReasonFailedAction = "PacemakerFailedResourceAction"
	eventReasonFencingEvent = "PacemakerFencingEvent"
	eventReasonWarning      = "PacemakerWarning"
	eventReasonError        = "PacemakerError"
	eventReasonHealthy      = "PacemakerHealthy"
)

// XML structures for parsing pacemaker status
type PacemakerResult struct {
	XMLName      xml.Name     `xml:"pacemaker-result"`
	Summary      Summary      `xml:"summary"`
	Nodes        Nodes        `xml:"nodes"`
	Resources    Resources    `xml:"resources"`
	NodeHistory  NodeHistory  `xml:"node_history"`
	FenceHistory FenceHistory `xml:"fence_history"`
	Status       Status       `xml:"status"`
}

type Summary struct {
	Stack               Stack               `xml:"stack"`
	CurrentDC           CurrentDC           `xml:"current_dc"`
	LastUpdate          LastUpdate          `xml:"last_update"`
	LastChange          LastChange          `xml:"last_change"`
	NodesConfigured     NodesConfigured     `xml:"nodes_configured"`
	ResourcesConfigured ResourcesConfigured `xml:"resources_configured"`
	ClusterOptions      ClusterOptions      `xml:"cluster_options"`
}

type Stack struct {
	Type            string `xml:"type,attr"`
	PacemakerdState string `xml:"pacemakerd-state,attr"`
}

type CurrentDC struct {
	Present      string `xml:"present,attr"`
	Version      string `xml:"version,attr"`
	Name         string `xml:"name,attr"`
	ID           string `xml:"id,attr"`
	WithQuorum   string `xml:"with_quorum,attr"`
	MixedVersion string `xml:"mixed_version,attr"`
}

type LastUpdate struct {
	Time   string `xml:"time,attr"`
	Origin string `xml:"origin,attr"`
}

type LastChange struct {
	Time   string `xml:"time,attr"`
	User   string `xml:"user,attr"`
	Client string `xml:"client,attr"`
	Origin string `xml:"origin,attr"`
}

type NodesConfigured struct {
	Number string `xml:"number,attr"`
}

type ResourcesConfigured struct {
	Number   string `xml:"number,attr"`
	Disabled string `xml:"disabled,attr"`
	Blocked  string `xml:"blocked,attr"`
}

type ClusterOptions struct {
	StonithEnabled         string `xml:"stonith-enabled,attr"`
	SymmetricCluster       string `xml:"symmetric-cluster,attr"`
	NoQuorumPolicy         string `xml:"no-quorum-policy,attr"`
	MaintenanceMode        string `xml:"maintenance-mode,attr"`
	StopAllResources       string `xml:"stop-all-resources,attr"`
	StonithTimeoutMs       string `xml:"stonith-timeout-ms,attr"`
	PriorityFencingDelayMs string `xml:"priority-fencing-delay-ms,attr"`
}

type Nodes struct {
	Node []Node `xml:"node"`
}

type Node struct {
	Name             string `xml:"name,attr"`
	ID               string `xml:"id,attr"`
	Online           string `xml:"online,attr"`
	Standby          string `xml:"standby,attr"`
	StandbyOnfail    string `xml:"standby_onfail,attr"`
	Maintenance      string `xml:"maintenance,attr"`
	Pending          string `xml:"pending,attr"`
	Unclean          string `xml:"unclean,attr"`
	Health           string `xml:"health,attr"`
	FeatureSet       string `xml:"feature_set,attr"`
	Shutdown         string `xml:"shutdown,attr"`
	ExpectedUp       string `xml:"expected_up,attr"`
	IsDC             string `xml:"is_dc,attr"`
	ResourcesRunning string `xml:"resources_running,attr"`
	Type             string `xml:"type,attr"`
}

type Resources struct {
	Clone    []Clone    `xml:"clone"`
	Resource []Resource `xml:"resource"`
}

type Clone struct {
	ID             string     `xml:"id,attr"`
	MultiState     string     `xml:"multi_state,attr"`
	Unique         string     `xml:"unique,attr"`
	Maintenance    string     `xml:"maintenance,attr"`
	Managed        string     `xml:"managed,attr"`
	Disabled       string     `xml:"disabled,attr"`
	Failed         string     `xml:"failed,attr"`
	FailureIgnored string     `xml:"failure_ignored,attr"`
	Resource       []Resource `xml:"resource"`
}

type Resource struct {
	ID             string  `xml:"id,attr"`
	ResourceAgent  string  `xml:"resource_agent,attr"`
	Role           string  `xml:"role,attr"`
	Active         string  `xml:"active,attr"`
	Orphaned       string  `xml:"orphaned,attr"`
	Blocked        string  `xml:"blocked,attr"`
	Maintenance    string  `xml:"maintenance,attr"`
	Managed        string  `xml:"managed,attr"`
	Failed         string  `xml:"failed,attr"`
	FailureIgnored string  `xml:"failure_ignored,attr"`
	NodesRunningOn string  `xml:"nodes_running_on,attr"`
	Node           NodeRef `xml:"node"`
}

type NodeRef struct {
	Name   string `xml:"name,attr"`
	ID     string `xml:"id,attr"`
	Cached string `xml:"cached,attr"`
}

type NodeHistory struct {
	Node []NodeHistoryNode `xml:"node"`
}

type NodeHistoryNode struct {
	Name            string            `xml:"name,attr"`
	ResourceHistory []ResourceHistory `xml:"resource_history"`
}

type ResourceHistory struct {
	ID                 string             `xml:"id,attr"`
	Orphan             string             `xml:"orphan,attr"`
	MigrationThreshold string             `xml:"migration-threshold,attr"`
	OperationHistory   []OperationHistory `xml:"operation_history"`
}

type OperationHistory struct {
	Call         string `xml:"call,attr"`
	Task         string `xml:"task,attr"`
	RC           string `xml:"rc,attr"`
	RCText       string `xml:"rc_text,attr"`
	LastRCChange string `xml:"last-rc-change,attr"`
	ExecTime     string `xml:"exec-time,attr"`
	QueueTime    string `xml:"queue-time,attr"`
	Interval     string `xml:"interval,attr"`
}

type FenceHistory struct {
	FenceEvent []FenceEvent `xml:"fence_event"`
}

type FenceEvent struct {
	Action    string `xml:"action,attr"`
	Target    string `xml:"target,attr"`
	Client    string `xml:"client,attr"`
	Origin    string `xml:"origin,attr"`
	Status    string `xml:"status,attr"`
	Delegate  string `xml:"delegate,attr"`
	Completed string `xml:"completed,attr"`
}

type Status struct {
	Code    string `xml:"code,attr"`
	Message string `xml:"message,attr"`
}

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

	// Track last processed PacemakerStatus to detect changes
	lastProcessedStatusMu sync.Mutex
	lastProcessedStatus   *v1alpha1.PacemakerStatus
}

// NewHealthCheck creates a new HealthCheck for monitoring pacemaker status
// in clusters that use ExternalEtcd clusters
func NewHealthCheck(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	restConfig *rest.Config,
) factory.Controller {
	// Create REST client for PacemakerStatus CRs
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(fmt.Errorf("failed to add scheme: %w", err))
	}

	pacemakerConfig := rest.CopyConfig(restConfig)
	pacemakerConfig.GroupVersion = &v1alpha1.SchemeGroupVersion
	pacemakerConfig.APIPath = "/apis"
	pacemakerConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme)

	restClient, err := rest.RESTClientFor(pacemakerConfig)
	if err != nil {
		panic(fmt.Errorf("failed to create REST client: %w", err))
	}

	// Create informer for PacemakerStatus
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				result := &v1alpha1.PacemakerStatusList{}
				err := restClient.Get().
					Resource("pacemakerstatuses").
					VersionedParams(&options, runtime.NewParameterCodec(scheme)).
					Do(context.Background()).
					Into(result)
				return result, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return restClient.Get().
					Resource("pacemakerstatuses").
					VersionedParams(&options, runtime.NewParameterCodec(scheme)).
					Watch(context.Background())
			},
		},
		&v1alpha1.PacemakerStatus{},
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
	}

	syncCtx := factory.NewSyncContext("PacemakerHealthCheck", eventRecorder.WithComponentSuffix("pacemaker-health-check"))

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("PacemakerHealthCheck", syncer)

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(healthCheckResyncInterval).
		WithSync(syncer.Sync).
		WithInformers(
			operatorClient.Informer(),
			informer,
		).ToController("PacemakerHealthCheck", syncCtx.Recorder())
}

// sync is the main sync function that gets called periodically to check pacemaker status
func (c *HealthCheck) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("PacemakerHealthCheck sync started")
	defer klog.V(4).Infof("PacemakerHealthCheck sync completed")

	// Get pacemaker status
	healthStatus, err := c.getPacemakerStatus(ctx)
	if err != nil {
		return err
	}

	// Update operator status conditions based on pacemaker status
	if err := c.updateOperatorStatus(ctx, healthStatus); err != nil {
		return err
	}

	// Record pacemaker health check events
	c.recordHealthCheckEvents(healthStatus)

	return nil
}

// getPacemakerStatus retrieves pacemaker status from the PacemakerStatus CR and returns a HealthStatus struct
func (c *HealthCheck) getPacemakerStatus(ctx context.Context) (*HealthStatus, error) {
	klog.V(4).Infof("Retrieving pacemaker status from CR...")

	// Get the PacemakerStatus CR
	result := &v1alpha1.PacemakerStatus{}
	err := c.pacemakerRESTClient.Get().
		Resource("pacemakerstatuses").
		Name(pacemakerStatusName).
		Do(ctx).
		Into(result)

	if err != nil {
		return newUnknownHealthStatus(fmt.Sprintf("Failed to get PacemakerStatus CR: %v", err)), nil
	}

	// Check if there was an error collecting the status
	if result.Status.CollectionError != "" {
		return newUnknownHealthStatus(fmt.Sprintf("Status collection error: %s", result.Status.CollectionError)), nil
	}

	// Check if the status is stale (older than 2 minutes)
	if time.Since(result.Status.LastUpdated.Time) > 2*time.Minute {
		return newUnknownHealthStatus(fmt.Sprintf("Pacemaker status is stale (last updated: %v)", result.Status.LastUpdated.Time)), nil
	}

	// Store the last processed status to detect changes
	c.lastProcessedStatusMu.Lock()
	c.lastProcessedStatus = result.DeepCopy()
	c.lastProcessedStatusMu.Unlock()

	// Validate XML size to prevent XML bombs
	if len(result.Status.RawXML) > maxXMLSize {
		return newUnknownHealthStatus(fmt.Sprintf("XML output too large: %d bytes (max: %d bytes)", len(result.Status.RawXML), maxXMLSize)), nil
	}

	// Parse the XML output
	status, err := c.parsePacemakerStatusXML(result.Status.RawXML)
	if err != nil {
		return newUnknownHealthStatus(fmt.Sprintf("Failed to parse pacemaker XML status: %v", err)), nil
	}

	return status, nil
}

// parsePacemakerStatusXML parses the XML output of "pcs status xml" command
func (c *HealthCheck) parsePacemakerStatusXML(xmlOutput string) (*HealthStatus, error) {
	status := &HealthStatus{
		OverallStatus: statusUnknown,
		Warnings:      []string{},
		Errors:        []string{},
	}

	// Parse XML
	var result PacemakerResult
	if err := xml.Unmarshal([]byte(xmlOutput), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal XML: %v", err)
	}

	// Validate critical structures are present
	if result.Summary.Stack.PacemakerdState == "" {
		return nil, fmt.Errorf("invalid XML: missing pacemaker state information")
	}

	// Check if pacemaker is running
	if result.Summary.Stack.PacemakerdState != pacemakerStateRunning {
		status.Errors = append(status.Errors, "Pacemaker is not running")
		status.OverallStatus = statusError
		return status, nil
	}

	// Check quorum
	if result.Summary.CurrentDC.WithQuorum != booleanValueTrue {
		status.Errors = append(status.Errors, "Cluster does not have quorum")
	}

	// Collect node status
	c.collectNodeStatus(&result, status)

	// Collect resource status
	c.collectResourceStatus(&result, status)

	// Collect recent failures
	c.collectRecentFailures(&result, status)

	// Collect recent fencing events
	c.collectFencingEvents(&result, status)

	// Determine overall status
	status.OverallStatus = c.determineOverallStatus(status)

	return status, nil
}

// collectNodeStatus checks if all nodes are online
func (c *HealthCheck) collectNodeStatus(result *PacemakerResult, status *HealthStatus) {
	// If this happens, something is seriously wrong with the cluster.
	if len(result.Nodes.Node) == 0 {
		status.Errors = append(status.Errors, "No nodes found")
		return
	}

	// Check if we have the expected number of nodes.
	// This should almost always be 2, but 1 node is possible during a control-plane node replacement event.
	if len(result.Nodes.Node) != expectedNodeCount {
		status.Warnings = append(status.Warnings, fmt.Sprintf("Expected %d nodes, found %d", expectedNodeCount, len(result.Nodes.Node)))
	}

	for _, node := range result.Nodes.Node {
		if node.Online != booleanValueTrue {
			status.Errors = append(status.Errors, fmt.Sprintf("Node %s is not online", node.Name))
		}

		if node.Standby == booleanValueTrue {
			status.Errors = append(status.Errors, fmt.Sprintf("Node %s is in standby (unexpected behavior; treated as offline)", node.Name))
		}
	}
}

// collectResourceStatus checks if kubelet and etcd resources are started on both nodes
func (c *HealthCheck) collectResourceStatus(result *PacemakerResult, status *HealthStatus) {
	resourcesStarted := make(map[string]map[string]bool)
	resourcesStarted[resourceKubelet] = make(map[string]bool)
	resourcesStarted[resourceEtcd] = make(map[string]bool)

	// Helper function to process a resource
	processResource := func(resource Resource) {
		if resource.Role == nodeStatusStarted && resource.Active == booleanValueTrue {
			nodeName := resource.Node.Name
			if nodeName != "" {
				// Check if this is a kubelet or etcd resource
				switch {
				case strings.HasPrefix(resource.ResourceAgent, resourceAgentKubelet):
					resourcesStarted[resourceKubelet][nodeName] = true
				case strings.HasPrefix(resource.ResourceAgent, resourceAgentEtcd):
					resourcesStarted[resourceEtcd][nodeName] = true
				}
			}
		}
	}

	// Check clone resources
	for _, clone := range result.Resources.Clone {
		for _, resource := range clone.Resource {
			processResource(resource)
		}
	}

	// Check standalone resources
	for _, resource := range result.Resources.Resource {
		processResource(resource)
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
		status.Errors = append(status.Errors, fmt.Sprintf("%s resource not started on all nodes (started on %d/%d nodes)",
			resourceName, actualCount, expectedNodeCount))
	}
}

// collectRecentFailures checks for recent failed resource actions
func (c *HealthCheck) collectRecentFailures(result *PacemakerResult, status *HealthStatus) {
	cutoffTime := time.Now().Add(-failedActionTimeWindow)

	for _, node := range result.NodeHistory.Node {
		for _, resourceHistory := range node.ResourceHistory {
			for _, operation := range resourceHistory.OperationHistory {
				// Check for failed operations (rc != "0")
				if operation.RC != operationRCSuccess || operation.RCText != operationRCTextSuccess {
					// Parse the timestamp
					t, err := time.Parse(pacemakerTimeFormat, operation.LastRCChange)
					if err != nil {
						klog.V(4).Infof("Failed to parse timestamp '%s' for operation %s on node %s: %v",
							operation.LastRCChange, operation.Task, node.Name, err)
						continue
					}
					if t.After(cutoffTime) {
						status.Warnings = append(status.Warnings, fmt.Sprintf("%s %s %s on %s failed (rc=%s, %s)",
							warningPrefixFailedAction, resourceHistory.ID, operation.Task, node.Name, operation.RC, operation.LastRCChange))
					}
				}
			}
		}
	}
}

// collectFencingEvents checks for recent fencing events
func (c *HealthCheck) collectFencingEvents(result *PacemakerResult, status *HealthStatus) {
	cutoffTime := time.Now().Add(-fencingEventTimeWindow)

	for _, fenceEvent := range result.FenceHistory.FenceEvent {
		// Parse the timestamp
		t, err := time.Parse(pacemakerFenceTimeFormat, fenceEvent.Completed)
		if err != nil {
			klog.V(4).Infof("Failed to parse fence timestamp '%s' for target %s: %v",
				fenceEvent.Completed, fenceEvent.Target, err)
			continue
		}
		if t.After(cutoffTime) {
			status.Warnings = append(status.Warnings, fmt.Sprintf("%s %s of %s %s",
				warningPrefixFencingEvent, fenceEvent.Action, fenceEvent.Target, fenceEvent.Status))
		}
	}
}

// determineOverallStatus determines the overall health status based on collected information
// Note: This function may add additional error messages to the status for certain conditions
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
		return c.setDegradedCondition(ctx, status)
	case statusHealthy:
		return c.clearDegradedCondition(ctx)
	default:
		// For Warning or Unknown status, we don't update the degraded condition
		return nil
	}
}

// logStatusMessages logs all warnings and errors
func (c *HealthCheck) logStatusMessages(status *HealthStatus) {
	for _, warning := range status.Warnings {
		klog.Warningf("Pacemaker warning: %s", warning)
	}

	for _, err := range status.Errors {
		klog.Errorf("Pacemaker error: %s", err)
	}
}

func (c *HealthCheck) setDegradedCondition(ctx context.Context, status *HealthStatus) error {
	message := strings.Join(status.Errors, "; ")
	if message == "" {
		message = "Pacemaker cluster is in degraded state"
	}

	condition := operatorv1.OperatorCondition{
		Type:    conditionTypeDegraded,
		Status:  operatorv1.ConditionTrue,
		Reason:  reasonPacemakerUnhealthy,
		Message: message,
	}

	return c.updateOperatorCondition(ctx, condition)
}

func (c *HealthCheck) clearDegradedCondition(ctx context.Context) error {
	condition := operatorv1.OperatorCondition{
		Type:   conditionTypeDegraded,
		Status: operatorv1.ConditionFalse,
	}

	return c.updateOperatorCondition(ctx, condition)
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

// shouldRecordEvent checks if an event should be recorded based on deduplication logic
func (c *HealthCheck) shouldRecordEvent(eventKey string, deduplicationWindow time.Duration) bool {
	c.recordedEventsMu.Lock()
	defer c.recordedEventsMu.Unlock()

	now := time.Now()

	// Clean up old entries that are outside the longest deduplication window
	maxWindow := eventDeduplicationWindowFencing
	for key, timestamp := range c.recordedEvents {
		if now.Sub(timestamp) > maxWindow {
			delete(c.recordedEvents, key)
		}
	}

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
	// Record events for warnings with appropriate deduplication window
	for _, warning := range status.Warnings {
		eventKey := fmt.Sprintf("warning:%s", warning)
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
	for _, err := range status.Errors {
		eventKey := fmt.Sprintf("error:%s", err)
		if c.shouldRecordEvent(eventKey, eventDeduplicationWindowDefault) {
			c.eventRecorder.Warningf(eventReasonError, "Pacemaker error: %s", err)
		}
	}

	// Record a normal event for healthy status only when transitioning from unhealthy/unknown
	if status.OverallStatus == statusHealthy {
		c.previousStatusMu.Lock()
		wasUnhealthy := c.previousStatus != statusHealthy
		c.previousStatus = statusHealthy
		c.previousStatusMu.Unlock()

		if wasUnhealthy {
			c.eventRecorder.Eventf(eventReasonHealthy, "Pacemaker cluster is healthy - all nodes online and critical resources started")
		}
	} else {
		// Update previous status for non-healthy states
		c.previousStatusMu.Lock()
		c.previousStatus = status.OverallStatus
		c.previousStatusMu.Unlock()
	}
}

// recordWarningEvent records appropriate warning events based on warning type
func (c *HealthCheck) recordWarningEvent(warning string) {
	switch {
	case strings.Contains(warning, warningPrefixFailedAction):
		c.eventRecorder.Warningf(eventReasonFailedAction, "Pacemaker detected a recent failed resource action: %s", warning)
	case strings.Contains(warning, warningPrefixFencingEvent):
		c.eventRecorder.Warningf(eventReasonFencingEvent, "Pacemaker detected a recent fencing event: %s", warning)
	default:
		c.eventRecorder.Warningf(eventReasonWarning, "Pacemaker warning: %s", warning)
	}
}
