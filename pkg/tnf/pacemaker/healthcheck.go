package pacemaker

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

// Constants for time windows and status strings
const (
	// Time windows for detecting recent events
	FailedActionTimeWindow = 5 * time.Minute
	FencingEventTimeWindow = 24 * time.Hour

	// Expected number of nodes in ExternalEtcd cluster
	ExpectedNodeCount = 2

	// Status strings
	StatusHealthy  = "Healthy"
	StatusWarning  = "Warning"
	StatusDegraded = "Degraded"
	StatusError    = "Error"
	StatusUnknown  = "Unknown"

	// Resource names
	ResourceKubelet = "kubelet"
	ResourceEtcd    = "etcd"

	// Node status indicators
	NodeStatusOnline  = "true"
	NodeStatusStarted = "Started"

	// Operator condition types
	ConditionTypeDegraded = "PacemakerHealthCheckDegraded"

	// Event reasons
	EventReasonFailedAction = "PacemakerFailedResourceAction"
	EventReasonFencingEvent = "PacemakerFencingEvent"
	EventReasonWarning      = "PacemakerWarning"
	EventReasonError        = "PacemakerError"
	EventReasonHealthy      = "PacemakerHealthy"
)

// XML structures for parsing pacemaker status
type PacemakerResult struct {
	XMLName     xml.Name     `xml:"pacemaker-result"`
	Summary     Summary      `xml:"summary"`
	Nodes       Nodes        `xml:"nodes"`
	Resources   Resources    `xml:"resources"`
	NodeHistory NodeHistory  `xml:"node_history"`
	FenceHistory FenceHistory `xml:"fence_history"`
	Status      Status       `xml:"status"`
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
	Number  string `xml:"number,attr"`
	Disabled string `xml:"disabled,attr"`
	Blocked  string `xml:"blocked,attr"`
}

type ClusterOptions struct {
	StonithEnabled            string `xml:"stonith-enabled,attr"`
	SymmetricCluster          string `xml:"symmetric-cluster,attr"`
	NoQuorumPolicy            string `xml:"no-quorum-policy,attr"`
	MaintenanceMode           string `xml:"maintenance-mode,attr"`
	StopAllResources          string `xml:"stop-all-resources,attr"`
	StonithTimeoutMs          string `xml:"stonith-timeout-ms,attr"`
	PriorityFencingDelayMs    string `xml:"priority-fencing-delay-ms,attr"`
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
	ID               string  `xml:"id,attr"`
	ResourceAgent    string  `xml:"resource_agent,attr"`
	Role             string  `xml:"role,attr"`
	Active           string  `xml:"active,attr"`
	Orphaned         string  `xml:"orphaned,attr"`
	Blocked          string  `xml:"blocked,attr"`
	Maintenance      string  `xml:"maintenance,attr"`
	Managed          string  `xml:"managed,attr"`
	Failed           string  `xml:"failed,attr"`
	FailureIgnored   string  `xml:"failure_ignored,attr"`
	NodesRunningOn   string  `xml:"nodes_running_on,attr"`
	Node             NodeRef `xml:"node"`
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
	Name             string            `xml:"name,attr"`
	ResourceHistory  []ResourceHistory `xml:"resource_history"`
}

type ResourceHistory struct {
	ID                  string             `xml:"id,attr"`
	Orphan              string             `xml:"orphan,attr"`
	MigrationThreshold  string             `xml:"migration-threshold,attr"`
	OperationHistory    []OperationHistory `xml:"operation_history"`
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

// HealthCheck monitors pacemaker status in ExternalEtcd topology clusters
type HealthCheck struct {
	operatorClient v1helpers.StaticPodOperatorClient
	kubeClient     kubernetes.Interface
	eventRecorder  events.Recorder
}

// NewHealthCheck creates a new HealthCheck for monitoring pacemaker status
// in clusters that use ExternalEtcd clusters
func NewHealthCheck(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &HealthCheck{
		operatorClient: operatorClient,
		kubeClient:     kubeClient,
		eventRecorder:  eventRecorder,
	}

	syncCtx := factory.NewSyncContext("PacemakerHealthCheck", eventRecorder.WithComponentSuffix("pacemaker-health-check"))

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("PacemakerHealthCheck", syncer)

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(30*time.Second).
		WithSync(syncer.Sync).
		WithInformers(
			operatorClient.Informer(),
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

	// Update operator availability based on pacemaker status
	if err := c.updateOperatorAvailability(ctx, healthStatus); err != nil {
		return err
	}

	// Record pacemaker health check events
	c.recordHealthCheckEvents(healthStatus)

	return nil
}

// getPacemakerStatus collects pacemaker status information and returns a HealthStatus struct
func (c *HealthCheck) getPacemakerStatus(ctx context.Context) (*HealthStatus, error) {
	klog.V(4).Infof("Collecting pacemaker status...")

	// Execute the pcs status xml command
	stdout, stderr, err := exec.Execute(ctx, "sudo pcs status xml")
	if err != nil {
		return &HealthStatus{
			OverallStatus: StatusError,
			Warnings:      []string{},
			Errors:        []string{fmt.Sprintf("Failed to execute pcs status xml command: %v", err)},
		}, nil
	}

	if stderr != "" {
		klog.Warningf("pcs status xml command produced stderr: %s", stderr)
	}

	// Parse the XML output
	status, err := c.parsePacemakerStatusXML(stdout)
	if err != nil {
		return &HealthStatus{
			OverallStatus: StatusError,
			Warnings:      []string{},
			Errors:        []string{fmt.Sprintf("Failed to parse pacemaker XML status: %v", err)},
		}, nil
	}

	return status, nil
}

// parsePacemakerStatusXML parses the XML output of "pcs status xml" command
func (c *HealthCheck) parsePacemakerStatusXML(xmlOutput string) (*HealthStatus, error) {
	status := &HealthStatus{
		OverallStatus: StatusUnknown,
		Warnings:      []string{},
		Errors:        []string{},
	}

	// Parse XML
	var result PacemakerResult
	if err := xml.Unmarshal([]byte(xmlOutput), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal XML: %v", err)
	}

	// Check if pacemaker is running
	if result.Summary.Stack.PacemakerdState != "running" {
		status.Errors = append(status.Errors, "Pacemaker is not running")
		return status, nil
	}

	// Check quorum
	if result.Summary.CurrentDC.WithQuorum != "true" {
		status.Errors = append(status.Errors, "Cluster does not have quorum")
	}

	// Parse nodes
	nodesOnline := c.parseNodes(&result, status)

	// Parse resources
	resourcesStarted := c.parseResources(&result, status)

	// Parse node history for recent failures
	c.parseNodeHistory(&result, status)

	// Parse fence history for recent fencing events
	c.parseFenceHistory(&result, status)

	// Determine overall status
	status.OverallStatus = c.determineOverallStatus(nodesOnline, resourcesStarted, status)

	return status, nil
}

// parseNodes checks if all nodes are online
func (c *HealthCheck) parseNodes(result *PacemakerResult, status *HealthStatus) bool {
	allNodesOnline := true

	for _, node := range result.Nodes.Node {
		if node.Online != NodeStatusOnline {
			allNodesOnline = false
			status.Errors = append(status.Errors, fmt.Sprintf("Node %s is not online", node.Name))
		}
	}

	// Check if we have the expected number of nodes
	if len(result.Nodes.Node) != ExpectedNodeCount {
		status.Errors = append(status.Errors, fmt.Sprintf("Expected %d nodes, found %d", ExpectedNodeCount, len(result.Nodes.Node)))
		allNodesOnline = false
	}

	return allNodesOnline
}

// parseResources checks if kubelet and etcd resources are started on both nodes
func (c *HealthCheck) parseResources(result *PacemakerResult, status *HealthStatus) bool {
	resourcesStarted := make(map[string]map[string]bool)
	resourcesStarted[ResourceKubelet] = make(map[string]bool)
	resourcesStarted[ResourceEtcd] = make(map[string]bool)

	// Helper function to process a resource
	processResource := func(resource Resource) {
		if resource.Role == NodeStatusStarted && resource.Active == "true" {
			nodeName := resource.Node.Name
			if nodeName != "" {
				// Check if this is a kubelet or etcd resource
				if strings.Contains(resource.ID, ResourceKubelet) {
					resourcesStarted[ResourceKubelet][nodeName] = true
				} else if strings.Contains(resource.ID, ResourceEtcd) {
					resourcesStarted[ResourceEtcd][nodeName] = true
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
	kubeletCount := len(resourcesStarted[ResourceKubelet])
	etcdCount := len(resourcesStarted[ResourceEtcd])

	if kubeletCount < ExpectedNodeCount {
		status.Errors = append(status.Errors, fmt.Sprintf("%s resource not started on all nodes (started on %d/%d nodes)", ResourceKubelet, kubeletCount, ExpectedNodeCount))
	}

	if etcdCount < ExpectedNodeCount {
		status.Errors = append(status.Errors, fmt.Sprintf("%s resource not started on all nodes (started on %d/%d nodes)", ResourceEtcd, etcdCount, ExpectedNodeCount))
	}

	return kubeletCount >= ExpectedNodeCount && etcdCount >= ExpectedNodeCount
}

// parseNodeHistory checks for recent failed resource actions
func (c *HealthCheck) parseNodeHistory(result *PacemakerResult, status *HealthStatus) {
	cutoffTime := time.Now().Add(-FailedActionTimeWindow)

	for _, node := range result.NodeHistory.Node {
		for _, resourceHistory := range node.ResourceHistory {
			for _, operation := range resourceHistory.OperationHistory {
				// Check for failed operations (rc != "0")
				if operation.RC != "0" && operation.RCText != "ok" {
					// Parse the timestamp
					if t, err := time.Parse("Mon Jan 2 15:04:05 2006", operation.LastRCChange); err == nil {
						if t.After(cutoffTime) {
							status.Warnings = append(status.Warnings, fmt.Sprintf("Recent failed resource action: %s %s on %s failed (rc=%s, %s)",
								resourceHistory.ID, operation.Task, node.Name, operation.RC, operation.LastRCChange))
						}
					}
				}
			}
		}
	}
}

// parseFenceHistory checks for recent fencing events
func (c *HealthCheck) parseFenceHistory(result *PacemakerResult, status *HealthStatus) {
	cutoffTime := time.Now().Add(-FencingEventTimeWindow)

	for _, fenceEvent := range result.FenceHistory.FenceEvent {
		// Parse the timestamp
		if t, err := time.Parse("2006-01-02 15:04:05.000000Z", fenceEvent.Completed); err == nil {
			if t.After(cutoffTime) {
				status.Warnings = append(status.Warnings, fmt.Sprintf("Recent fencing event: %s of %s %s",
					fenceEvent.Action, fenceEvent.Target, fenceEvent.Status))
			}
		}
	}
}

// determineOverallStatus determines the overall health status based on parsed information
func (c *HealthCheck) determineOverallStatus(nodesOnline, resourcesStarted bool, status *HealthStatus) string {
	if !nodesOnline {
		status.Errors = append(status.Errors, "One or more nodes are offline")
		return StatusDegraded
	}

	if !resourcesStarted {
		status.Errors = append(status.Errors, "One or more critical resources are not started")
		return StatusDegraded
	}

	if len(status.Errors) > 0 {
		return StatusDegraded
	}

	if len(status.Warnings) > 0 {
		return StatusWarning
	}

	return StatusHealthy
}

// updateOperatorAvailability processes the HealthStatus and conditionally updates the operator state
func (c *HealthCheck) updateOperatorAvailability(ctx context.Context, status *HealthStatus) error {
	klog.V(4).Infof("Updating operator availability based on pacemaker status: %s", status.OverallStatus)

	// Log warnings and errors
	c.logStatusMessages(status)

	// Update operator conditions based on pacemaker status
	if status.OverallStatus == StatusDegraded {
		return c.setDegradedCondition(ctx, status)
	} else if status.OverallStatus == StatusHealthy {
		return c.clearDegradedCondition(ctx)
	}

	// For Warning or Error status, we don't update the degraded condition
	return nil
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

// setDegradedCondition sets the degraded condition to true
func (c *HealthCheck) setDegradedCondition(ctx context.Context, status *HealthStatus) error {
	message := strings.Join(status.Errors, "; ")
	if message == "" {
		message = "Pacemaker cluster is in degraded state"
	}

	condition := operatorv1.OperatorCondition{
		Type:    ConditionTypeDegraded,
		Status:  operatorv1.ConditionTrue,
		Reason:  "PacemakerUnhealthy",
		Message: message,
	}

	return c.updateOperatorCondition(ctx, condition)
}

// clearDegradedCondition clears the degraded condition
func (c *HealthCheck) clearDegradedCondition(ctx context.Context) error {
	condition := operatorv1.OperatorCondition{
		Type:   ConditionTypeDegraded,
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

// recordHealthCheckEvents records events for pacemaker warnings and fencing history
func (c *HealthCheck) recordHealthCheckEvents(status *HealthStatus) {
	// Record events for warnings
	for _, warning := range status.Warnings {
		c.recordWarningEvent(warning)
	}

	// Record events for errors
	for _, err := range status.Errors {
		c.eventRecorder.Warningf(EventReasonError, "Pacemaker error: %s", err)
	}

	// Record a normal event for healthy status
	if status.OverallStatus == StatusHealthy {
		c.eventRecorder.Eventf(EventReasonHealthy, "Pacemaker cluster is healthy - all nodes online and critical resources started")
	}
}

// recordWarningEvent records appropriate warning events based on warning type
func (c *HealthCheck) recordWarningEvent(warning string) {
	switch {
	case strings.Contains(warning, "Recent failed resource action:"):
		c.eventRecorder.Warningf(EventReasonFailedAction, "Pacemaker detected a recent failed resource action: %s", warning)
	case strings.Contains(warning, "Recent fencing event:"):
		c.eventRecorder.Warningf(EventReasonFencingEvent, "Pacemaker detected a recent fencing event: %s", warning)
	default:
		c.eventRecorder.Warningf(EventReasonWarning, "Pacemaker warning: %s", warning)
	}
}
