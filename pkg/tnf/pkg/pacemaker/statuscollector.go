package pacemaker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

const (
	// Commands are hardcoded (no user input) and run with sudo -n (non-interactive).
	// They are whitelisted in sudoers for the service account.
	pcsStatusXMLCommand     = "sudo -n pcs status xml"
	pcsClusterConfigCommand = "sudo -n pcs cluster config show --output-format json"

	execTimeout      = 10 * time.Second
	collectorTimeout = 30 * time.Second
	maxXMLSize       = 10 * 1024 * 1024 // Prevents XML bombs

	statusSubresource = "status"

	// Pacemaker timestamp formats
	pacemakerTimeFormat      = "Mon Jan 2 15:04:05 2006"
	pacemakerFenceTimeFormat = "2006-01-02 15:04:05.000000Z"
)

// ResourceStatePerNode tracks resource state per node for condition building.
type ResourceStatePerNode struct {
	KubeletRunning  bool
	EtcdRunning     bool
	KubeletResource *Resource
	EtcdResource    *Resource
}

// FencingAgentInfo tracks a fencing agent parsed from pacemaker resources.
// Fencing agents are named like "master-0_redfish" where the prefix is the target node.
type FencingAgentInfo struct {
	Name       string
	TargetNode string
	Method     v1alpha1.FencingMethod
	Resource   *Resource
	IsRunning  bool
}

func NewPacemakerStatusCollectorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pacemaker-status-collector",
		Short: "Collects pacemaker status and updates PacemakerCluster CR",
		Run: func(cmd *cobra.Command, args []string) {
			if err := runCollector(); err != nil {
				klog.Errorf("Failed to collect pacemaker status: %v", err)
				os.Exit(1)
			}
		},
	}
	return cmd
}

// runCollector fetches pacemaker status, builds the CR, records events, and updates the API.
func runCollector() error {
	ctx, cancel := context.WithTimeout(context.Background(), collectorTimeout)
	defer cancel()

	klog.Info("Starting pacemaker status collection...")

	kubeClient, err := createKubeClient()
	if err != nil {
		return fmt.Errorf("failed to create kube client: %w", err)
	}

	// Cluster config provides authoritative node list with IPs
	clusterConfig, err := fetchClusterConfig(ctx)
	if err != nil {
		klog.Warningf("Failed to fetch cluster config: %v (continuing with limited node data)", err)
	}

	result, err := collectPacemakerStatus(ctx)
	if err != nil {
		recordCollectionErrorEvent(ctx, kubeClient, err)
		return fmt.Errorf("failed to collect pacemaker status: %w", err)
	}

	clusterStatus := buildClusterStatus(result, clusterConfig)
	recordFencingEvents(ctx, kubeClient, result)
	recordFailedActionEvents(ctx, kubeClient, result)

	if err := updatePacemakerStatusCR(ctx, clusterStatus); err != nil {
		return fmt.Errorf("failed to update Pacemaker CR: %w", err)
	}

	klog.Info("Successfully updated Pacemaker CR")
	return nil
}

func createKubeClient() (kubernetes.Interface, error) {
	restConfig, err := getKubeConfig()
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}

	return kubeClient, nil
}

// fetchClusterConfig returns the authoritative node list with IP addresses.
func fetchClusterConfig(ctx context.Context) (*ClusterConfig, error) {
	ctxExec, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	stdout, stderr, err := exec.Execute(ctxExec, pcsClusterConfigCommand)
	if err != nil {
		return nil, fmt.Errorf("failed to execute pcs cluster config command: %w", err)
	}

	if stderr != "" {
		klog.V(4).Infof("pcs cluster config command produced stderr: %s", stderr)
	}

	var config ClusterConfig
	if err := json.Unmarshal([]byte(stdout), &config); err != nil {
		return nil, fmt.Errorf("failed to parse cluster config JSON: %w", err)
	}

	klog.V(2).Infof("Cluster config: name='%s', uuid='%s', nodes=%d", config.ClusterName, config.ClusterUUID, len(config.Nodes))
	for _, node := range config.Nodes {
		if len(node.Addrs) > 0 {
			klog.V(4).Infof("  Node %s (ID %s): %s", node.Name, node.NodeID, node.Addrs[0].Addr)
		}
	}

	return &config, nil
}

// collectPacemakerStatus executes "pcs status xml" and parses the result.
func collectPacemakerStatus(ctx context.Context) (*PacemakerResult, error) {
	ctxExec, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	stdout, stderr, execErr := exec.Execute(ctxExec, pcsStatusXMLCommand)
	if execErr != nil {
		return nil, fmt.Errorf("failed to execute pcs status xml command: %w", execErr)
	}

	if stderr != "" {
		klog.Warningf("pcs status xml command produced stderr: %s", stderr)
	}

	if len(stdout) > maxXMLSize {
		return nil, fmt.Errorf("XML output too large: %d bytes (max: %d bytes)", len(stdout), maxXMLSize)
	}

	var result PacemakerResult
	if parseErr := xml.Unmarshal([]byte(stdout), &result); parseErr != nil {
		return nil, fmt.Errorf("failed to parse XML: %w", parseErr)
	}

	return &result, nil
}

func buildClusterStatus(result *PacemakerResult, clusterConfig *ClusterConfig) *v1alpha1.PacemakerClusterStatus {
	now := metav1.Now()

	resourceState := make(map[string]*ResourceStatePerNode)
	processResourcesForState(result, resourceState)

	fencingAgentsByTarget := processFencingAgents(result)

	xmlNodeMap := make(map[string]*Node)
	for i := range result.Nodes.Node {
		xmlNodeMap[result.Nodes.Node[i].Name] = &result.Nodes.Node[i]
	}

	var nodes []v1alpha1.PacemakerClusterNodeStatus

	// Use cluster config as authoritative node list when available
	if clusterConfig != nil && len(clusterConfig.Nodes) > 0 {
		klog.V(2).Infof("Building nodes from cluster config (authoritative source): %d nodes", len(clusterConfig.Nodes))

		for _, configNode := range clusterConfig.Nodes {
			if configNode.Name == "" {
				klog.Warningf("Skipping node with empty name from cluster config")
				continue
			}

			addresses := getNodeAddresses(configNode)
			if len(addresses) == 0 {
				klog.Warningf("Node %s has no valid IP addresses, skipping", configNode.Name)
				continue
			}

			xmlNode := xmlNodeMap[configNode.Name]
			state := resourceState[configNode.Name]
			if state == nil {
				state = &ResourceStatePerNode{}
			}
			fencingAgents := fencingAgentsByTarget[configNode.Name]

			nodeStatus := buildNodeStatus(configNode.Name, addresses, xmlNode, state, fencingAgents, now)
			nodes = append(nodes, nodeStatus)
		}
	} else {
		// Fallback when cluster config is unavailable - uses placeholder IPs that will fail validation
		klog.Warningf("Cluster config unavailable, falling back to XML nodes (IP addresses may be missing)")

		for _, xmlNode := range result.Nodes.Node {
			if xmlNode.Name == "" {
				klog.Warningf("Skipping node with empty name from XML")
				continue
			}

			state := resourceState[xmlNode.Name]
			if state == nil {
				state = &ResourceStatePerNode{}
			}
			fencingAgents := fencingAgentsByTarget[xmlNode.Name]

			placeholderAddresses := []v1alpha1.PacemakerNodeAddress{{Type: v1alpha1.PacemakerNodeInternalIP, Address: "0.0.0.0"}}
			nodeStatus := buildNodeStatus(xmlNode.Name, placeholderAddresses, &xmlNode, state, fencingAgents, now)
			nodes = append(nodes, nodeStatus)
		}
	}

	clusterInMaintenance := isClusterInMaintenance(result)
	clusterConditions := buildClusterConditions(len(nodes), clusterInMaintenance, nodes, now)

	return &v1alpha1.PacemakerClusterStatus{
		Conditions:  clusterConditions,
		LastUpdated: now,
		Nodes:       &nodes,
	}
}

func buildNodeStatus(name string, addresses []v1alpha1.PacemakerNodeAddress, xmlNode *Node, state *ResourceStatePerNode, fencingAgents []FencingAgentInfo, now metav1.Time) v1alpha1.PacemakerClusterNodeStatus {
	fencingAgentStatuses := buildFencingAgentStatuses(fencingAgents, now)
	nodeConditions := buildNodeConditions(xmlNode, state, fencingAgentStatuses, now)
	resources := buildResourceStatuses(state, now)

	return v1alpha1.PacemakerClusterNodeStatus{
		Conditions:    nodeConditions,
		NodeName:      name,
		Addresses:     addresses,
		Resources:     resources,
		FencingAgents: fencingAgentStatuses,
	}
}

func buildClusterConditions(nodeCount int, inMaintenance bool, nodes []v1alpha1.PacemakerClusterNodeStatus, now metav1.Time) []metav1.Condition {
	nodeCountCondition := buildNodeCountCondition(nodeCount, now)
	inServiceCondition := buildCondition(clusterInServiceSpec, !inMaintenance, now)
	healthyCondition := buildClusterHealthyCondition(nodeCountCondition, inServiceCondition, nodes, now)

	return []metav1.Condition{nodeCountCondition, inServiceCondition, healthyCondition}
}

func buildNodeCountCondition(nodeCount int, now metav1.Time) metav1.Condition {
	if nodeCount == ExpectedNodeCount {
		return metav1.Condition{
			Type:               v1alpha1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected,
			Message:            fmt.Sprintf("Expected %d nodes, found %d", ExpectedNodeCount, nodeCount),
			LastTransitionTime: now,
		}
	}

	reason := v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes
	if nodeCount > ExpectedNodeCount {
		reason = v1alpha1.ClusterNodeCountAsExpectedReasonExcessiveNodes
	}

	return metav1.Condition{
		Type:               v1alpha1.ClusterNodeCountAsExpectedConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            fmt.Sprintf("Expected %d nodes, found %d", ExpectedNodeCount, nodeCount),
		LastTransitionTime: now,
	}
}

// buildClusterHealthyCondition aggregates node count, maintenance mode, and per-node health.
func buildClusterHealthyCondition(nodeCountCondition, inServiceCondition metav1.Condition, nodes []v1alpha1.PacemakerClusterNodeStatus, now metav1.Time) metav1.Condition {
	allHealthy := nodeCountCondition.Status == metav1.ConditionTrue &&
		inServiceCondition.Status == metav1.ConditionTrue

	for _, node := range nodes {
		if getConditionStatus(node.Conditions, v1alpha1.NodeHealthyConditionType) != metav1.ConditionTrue {
			allHealthy = false
			break
		}
	}

	return buildCondition(clusterHealthySpec, allHealthy, now)
}

func buildNodeConditions(xmlNode *Node, state *ResourceStatePerNode, fencingAgents []v1alpha1.PacemakerClusterFencingAgentStatus, now metav1.Time) []metav1.Condition {
	online := xmlNode != nil && xmlNode.Online == "true"
	inMaintenance := xmlNode != nil && xmlNode.Maintenance == "true"
	standby := xmlNode != nil && (xmlNode.Standby == "true" || xmlNode.StandbyOnFail == "true")
	pending := xmlNode != nil && xmlNode.Pending == "true"
	unclean := xmlNode != nil && xmlNode.Unclean == "true"
	isMember := xmlNode != nil && xmlNode.Type == "member"

	fencingAvailable, fencingHealthy := calculateFencingHealth(fencingAgents)

	// Node healthy = all conditions good + resources running + fencing available
	healthy := online && !inMaintenance && !standby && !pending && !unclean && isMember && fencingAvailable
	if state != nil {
		healthy = healthy && state.KubeletRunning && state.EtcdRunning
	}

	return []metav1.Condition{
		buildCondition(nodeOnlineSpec, online, now),
		buildCondition(nodeInServiceSpec, !inMaintenance, now),
		buildCondition(nodeActiveSpec, !standby, now),
		buildCondition(nodeReadySpec, !pending, now),
		buildCondition(nodeCleanSpec, !unclean, now),
		buildCondition(nodeMemberSpec, isMember, now),
		buildCondition(nodeFencingAvailableSpec, fencingAvailable, now),
		buildCondition(nodeFencingHealthySpec, fencingHealthy, now),
		buildCondition(nodeHealthySpec, healthy, now),
	}
}

// calculateFencingHealth returns (available, allHealthy).
// Available means at least one agent can fence right now (is running).
// AllHealthy means all agents are fully managed (will be recovered if they fail).
func calculateFencingHealth(fencingAgents []v1alpha1.PacemakerClusterFencingAgentStatus) (bool, bool) {
	if len(fencingAgents) == 0 {
		return false, false
	}

	availableCount := 0
	healthyCount := 0
	for _, agent := range fencingAgents {
		if isAgentAvailable(agent) {
			availableCount++
		}
		if isAgentHealthy(agent) {
			healthyCount++
		}
	}

	return availableCount > 0, healthyCount == len(fencingAgents)
}

// isAgentAvailable returns true if the agent can fence right now (is running).
// An agent on a maintenance node is still available - it's running and can fence.
func isAgentAvailable(agent v1alpha1.PacemakerClusterFencingAgentStatus) bool {
	conditions := agent.Conditions
	active := getConditionStatus(conditions, v1alpha1.ResourceActiveConditionType) == metav1.ConditionTrue
	started := getConditionStatus(conditions, v1alpha1.ResourceStartedConditionType) == metav1.ConditionTrue
	operational := getConditionStatus(conditions, v1alpha1.ResourceOperationalConditionType) == metav1.ConditionTrue
	return active && started && operational
}

// isAgentHealthy returns true if the agent is fully managed (will be recovered if it fails).
func isAgentHealthy(agent v1alpha1.PacemakerClusterFencingAgentStatus) bool {
	return getConditionStatus(agent.Conditions, v1alpha1.ResourceHealthyConditionType) == metav1.ConditionTrue
}

func buildResourceStatuses(state *ResourceStatePerNode, now metav1.Time) []v1alpha1.PacemakerClusterResourceStatus {
	return []v1alpha1.PacemakerClusterResourceStatus{
		buildResourceStatus(v1alpha1.PacemakerClusterResourceNameKubelet, state.KubeletResource, state.KubeletRunning, now),
		buildResourceStatus(v1alpha1.PacemakerClusterResourceNameEtcd, state.EtcdResource, state.EtcdRunning, now),
	}
}

func buildFencingAgentStatuses(agents []FencingAgentInfo, now metav1.Time) []v1alpha1.PacemakerClusterFencingAgentStatus {
	statuses := make([]v1alpha1.PacemakerClusterFencingAgentStatus, 0, len(agents))

	for _, agent := range agents {
		// Fencing agents use the same condition structure as resources
		conditions := buildResourceConditions(agent.Resource, agent.IsRunning, now)
		statuses = append(statuses, v1alpha1.PacemakerClusterFencingAgentStatus{
			Conditions: conditions,
			Name:       agent.Name,
			Method:     agent.Method,
		})
	}

	return statuses
}

// methodStringToEnum converts method string to FencingMethod enum, defaulting to Redfish.
func methodStringToEnum(method string) v1alpha1.FencingMethod {
	switch strings.ToLower(method) {
	case "ipmi", "ipmilan", "ipmitool":
		return v1alpha1.FencingMethodIPMI
	case "redfish":
		return v1alpha1.FencingMethodRedfish
	default:
		// Log unknown method so it's visible, but default to Redfish
		// since that's the primary TNF fencing method
		klog.Warningf("Unknown fencing method %q, defaulting to Redfish", method)
		return v1alpha1.FencingMethodRedfish
	}
}

func buildResourceStatus(name v1alpha1.PacemakerClusterResourceName, resource *Resource, running bool, now metav1.Time) v1alpha1.PacemakerClusterResourceStatus {
	return v1alpha1.PacemakerClusterResourceStatus{
		Conditions: buildResourceConditions(resource, running, now),
		Name:       name,
	}
}

func buildResourceConditions(resource *Resource, running bool, now metav1.Time) []metav1.Condition {
	var inMaintenance, managed, enabled, operational, active, started, schedulable bool

	if resource != nil {
		// Pacemaker sets maintenance="true" on resources running on a node in maintenance mode.
		// These resources also have managed="false" because pacemaker won't manage them for
		// recovery. This is a degraded state: the resource may be running, but if it fails,
		// pacemaker won't restart it.
		inMaintenance = resource.Maintenance == "true"
		managed = resource.Managed == "true"
		enabled = resource.TargetRole != "Stopped"
		operational = resource.Failed != "true"
		active = resource.Active == "true"
		started = resource.Role == "Started"
		schedulable = resource.Blocked != "true"
	}

	// A resource is healthy only when all operational conditions are met AND it's managed.
	// Resources in maintenance mode are NOT healthy - they won't be managed for recovery.
	healthy := managed && enabled && operational && active && started && schedulable

	return []metav1.Condition{
		buildCondition(resourceInServiceSpec, !inMaintenance, now),
		buildCondition(resourceManagedSpec, managed, now),
		buildCondition(resourceEnabledSpec, enabled, now),
		buildCondition(resourceOperationalSpec, operational, now),
		buildCondition(resourceActiveSpec, active, now),
		buildCondition(resourceStartedSpec, started, now),
		buildCondition(resourceSchedulableSpec, schedulable, now),
		buildCondition(resourceHealthySpec, healthy, now),
	}
}

// getNodeAddresses extracts and validates IP addresses from cluster config.
func getNodeAddresses(configNode ClusterConfigNode) []v1alpha1.PacemakerNodeAddress {
	if len(configNode.Addrs) == 0 {
		klog.Warningf("Node %s in cluster config has no addresses", configNode.Name)
		return nil
	}

	var addresses []v1alpha1.PacemakerNodeAddress
	for _, addr := range configNode.Addrs {
		parsedIP := net.ParseIP(addr.Addr)
		if parsedIP == nil {
			klog.Warningf("Failed to parse IP address '%s' for node %s: not a valid IP", addr.Addr, configNode.Name)
			continue
		}
		if !parsedIP.IsGlobalUnicast() {
			klog.Warningf("IP address '%s' for node %s is not a global unicast address", addr.Addr, configNode.Name)
			continue
		}
		addresses = append(addresses, v1alpha1.PacemakerNodeAddress{
			Type:    v1alpha1.PacemakerNodeInternalIP,
			Address: parsedIP.String(),
		})
	}

	return addresses
}

// isClusterInMaintenance returns true if cluster-wide maintenance mode is enabled
// (pcs property set maintenance-mode=true). Node-level maintenance (pcs node maintenance <node>)
// is handled separately via buildNodeConditions.
func isClusterInMaintenance(result *PacemakerResult) bool {
	return result.Summary.ClusterOptions.MaintenanceMode == "true"
}

// processResourcesForState populates resourceState map. Fencing agents are handled separately.
func processResourcesForState(result *PacemakerResult, resourceState map[string]*ResourceStatePerNode) {
	processResource := func(resource Resource) {
		if resource.Node.Name == "" {
			return
		}

		// Skip fencing resources
		if strings.HasPrefix(resource.ResourceAgent, ResourceAgentFencing) ||
			strings.Contains(resource.ResourceAgent, ResourceAgentFenceAWS) {
			return
		}

		if _, exists := resourceState[resource.Node.Name]; !exists {
			resourceState[resource.Node.Name] = &ResourceStatePerNode{}
		}
		state := resourceState[resource.Node.Name]
		isRunning := resource.Role == "Started" && resource.Active == "true"

		switch {
		case strings.HasPrefix(resource.ResourceAgent, ResourceAgentKubelet):
			state.KubeletRunning = isRunning
			state.KubeletResource = &resource
		case strings.HasPrefix(resource.ResourceAgent, ResourceAgentEtcd):
			state.EtcdRunning = isRunning
			state.EtcdResource = &resource
		}
	}

	for _, clone := range result.Resources.Clone {
		for _, resource := range clone.Resource {
			processResource(resource)
		}
	}
	for _, resource := range result.Resources.Resource {
		processResource(resource)
	}
}

// processFencingAgents returns fencing agents grouped by their target node.
// Agents are named "<nodename>_<method>" (e.g., "master-0_redfish").
func processFencingAgents(result *PacemakerResult) map[string][]FencingAgentInfo {
	fencingAgents := make(map[string][]FencingAgentInfo)

	processFencingResource := func(resource Resource) {
		if !strings.HasPrefix(resource.ResourceAgent, ResourceAgentFencing) &&
			!strings.Contains(resource.ResourceAgent, ResourceAgentFenceAWS) {
			return
		}

		targetNode, methodStr := parseFencingResourceID(resource.ID)
		if targetNode == "" {
			klog.Warningf("Could not parse target node from fencing resource ID: %s", resource.ID)
			return
		}

		isRunning := resource.Role == "Started" && resource.Active == "true"
		resourceCopy := resource

		fencingAgents[targetNode] = append(fencingAgents[targetNode], FencingAgentInfo{
			Name:       resource.ID,
			TargetNode: targetNode,
			Method:     methodStringToEnum(methodStr),
			Resource:   &resourceCopy,
			IsRunning:  isRunning,
		})
	}

	for _, resource := range result.Resources.Resource {
		processFencingResource(resource)
	}
	for _, clone := range result.Resources.Clone {
		for _, resource := range clone.Resource {
			processFencingResource(resource)
		}
	}

	return fencingAgents
}

// parseFencingResourceID parses "<nodename>_<method>" format, returns ("", "") if invalid.
func parseFencingResourceID(resourceID string) (string, string) {
	lastUnderscore := strings.LastIndex(resourceID, "_")
	if lastUnderscore == -1 || lastUnderscore == 0 || lastUnderscore == len(resourceID)-1 {
		return "", ""
	}
	return resourceID[:lastUnderscore], resourceID[lastUnderscore+1:]
}

// RecentFencingEvent is a fencing event that passed time-window filtering.
type RecentFencingEvent struct {
	Action, Target, Status, ExitReason, Completed string
}

// filterRecentFencingEvents returns fencing events within the time window. Separated for testability.
func filterRecentFencingEvents(result *PacemakerResult, cutoffTime time.Time) []RecentFencingEvent {
	var events []RecentFencingEvent

	for _, fenceEvent := range result.FenceHistory.FenceEvent {
		if fenceEvent.Target == "" {
			continue
		}

		t, err := time.Parse(pacemakerFenceTimeFormat, fenceEvent.Completed)
		if err != nil {
			klog.V(4).Infof("Skipping fencing event due to timestamp parse error: %v", err)
			continue
		}
		if !t.After(cutoffTime) {
			continue
		}

		events = append(events, RecentFencingEvent{
			Action:     fenceEvent.Action,
			Target:     fenceEvent.Target,
			Status:     fenceEvent.Status,
			ExitReason: fenceEvent.ExitReason,
			Completed:  fenceEvent.Completed,
		})
	}

	return events
}

// recordFencingEvents records K8s events for recent fencing. Success=Normal, failure=Warning.
func recordFencingEvents(ctx context.Context, kubeClient kubernetes.Interface, result *PacemakerResult) {
	cutoffTime := time.Now().Add(-FencingEventTimeWindow)
	events := filterRecentFencingEvents(result, cutoffTime)

	for _, event := range events {
		var message string
		if event.Status != "success" && event.ExitReason != "" {
			message = fmt.Sprintf("Fencing event: %s of %s completed with status %s (%s) at %s",
				event.Action, event.Target, event.Status, event.ExitReason, event.Completed)
		} else {
			message = fmt.Sprintf("Fencing event: %s of %s completed with status %s at %s",
				event.Action, event.Target, event.Status, event.Completed)
		}

		eventType := corev1.EventTypeNormal
		if event.Status != "success" {
			eventType = corev1.EventTypeWarning
		}
		recordEvent(ctx, kubeClient, EventReasonFencingEvent, message, eventType)
	}
}

// RecentFailedAction is a failed resource action that passed time-window filtering.
type RecentFailedAction struct {
	ResourceID, Task, NodeName, RC, RCText, LastRCChange string
}

// filterRecentFailedActions returns failed actions (rc != 0) within the time window. Separated for testability.
func filterRecentFailedActions(result *PacemakerResult, cutoffTime time.Time) []RecentFailedAction {
	var actions []RecentFailedAction

	for _, node := range result.NodeHistory.Node {
		for _, resourceHistory := range node.ResourceHistory {
			for _, operation := range resourceHistory.OperationHistory {
				if operation.RC == "0" {
					continue
				}

				t, err := time.Parse(pacemakerTimeFormat, operation.LastRCChange)
				if err != nil {
					klog.V(4).Infof("Skipping operation history due to timestamp parse error: %v", err)
					continue
				}
				if !t.After(cutoffTime) {
					continue
				}

				actions = append(actions, RecentFailedAction{
					ResourceID:   resourceHistory.ID,
					Task:         operation.Task,
					NodeName:     node.Name,
					RC:           operation.RC,
					RCText:       operation.RCText,
					LastRCChange: operation.LastRCChange,
				})
			}
		}
	}

	return actions
}

func recordFailedActionEvents(ctx context.Context, kubeClient kubernetes.Interface, result *PacemakerResult) {
	cutoffTime := time.Now().Add(-FailedActionTimeWindow)
	actions := filterRecentFailedActions(result, cutoffTime)

	for _, action := range actions {
		message := fmt.Sprintf("Failed resource action: %s %s on node %s (rc=%s: %s) at %s",
			action.ResourceID, action.Task, action.NodeName, action.RC, action.RCText, action.LastRCChange)
		recordEvent(ctx, kubeClient, EventReasonFailedAction, message, corev1.EventTypeWarning)
	}
}

func recordCollectionErrorEvent(ctx context.Context, kubeClient kubernetes.Interface, err error) {
	message := fmt.Sprintf("Failed to collect pacemaker status: %v", err)
	recordEvent(ctx, kubeClient, EventReasonCollectionError, message, corev1.EventTypeWarning)
}

// generateEventName creates a hash-based name for deduplication.
func generateEventName(reason, message string) string {
	hash := sha256.Sum256([]byte(reason + message))
	shortHash := hex.EncodeToString(hash[:])[:12]
	return fmt.Sprintf("pacemaker-%s-%s", strings.ToLower(reason), shortHash)
}

// recordEventWithDeduplication patches existing events within the window, otherwise creates new ones.
// Old events are left to age out via K8s garbage collection.
func recordEventWithDeduplication(ctx context.Context, kubeClient kubernetes.Interface, reason, message, eventType string, deduplicationWindow time.Duration) {
	eventName := generateEventName(reason, message)

	existingEvent, err := kubeClient.CoreV1().Events(TargetNamespace).Get(ctx, eventName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		// Log unexpected errors (not NotFound) for debugging
		klog.V(4).Infof("Failed to get existing event %s: %v (will create new)", eventName, err)
	}
	if err == nil && existingEvent != nil {
		if time.Since(existingEvent.LastTimestamp.Time) < deduplicationWindow {
			now := metav1.Now()
			patch := fmt.Sprintf(`{"count":%d,"lastTimestamp":"%s"}`, existingEvent.Count+1, now.Format(time.RFC3339))
			_, patchErr := kubeClient.CoreV1().Events(TargetNamespace).Patch(ctx, eventName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
			if patchErr != nil {
				klog.Warningf("Failed to patch existing event %s: %v (will create new)", eventName, patchErr)
			} else {
				klog.V(4).Infof("Patched existing event: %s (count=%d)", eventName, existingEvent.Count+1)
				return
			}
		}
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: TargetNamespace,
		},
		// NOTE: CronJob workaround for cluster-scoped resource events.
		// PacemakerCluster is cluster-scoped (no namespace), but Kubernetes requires
		// involvedObject.namespace to match event.namespace for namespaced events.
		// We use the status collector CronJob as the InvolvedObject since it runs
		// in the target namespace and is the logical source of these events.
		// Events will appear as: `kubectl get events -n openshift-etcd`
		InvolvedObject: corev1.ObjectReference{
			Kind:       "CronJob",
			Name:       StatusCollectorCronJobName,
			Namespace:  TargetNamespace,
			APIVersion: "batch/v1",
		},
		Reason:  reason,
		Message: message,
		Type:    eventType,
		Source: corev1.EventSource{
			Component: "pacemaker-status-collector",
		},
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
		Count:          1,
	}

	_, createErr := kubeClient.CoreV1().Events(TargetNamespace).Create(ctx, event, metav1.CreateOptions{})
	if createErr != nil {
		if apierrors.IsAlreadyExists(createErr) {
			// Race: another collector created it; patch instead
			klog.V(4).Infof("Event %s already exists, attempting patch", eventName)
			now := metav1.Now()
			patch := fmt.Sprintf(`{"count":2,"lastTimestamp":"%s"}`, now.Format(time.RFC3339))
			_, patchErr := kubeClient.CoreV1().Events(TargetNamespace).Patch(ctx, eventName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
			if patchErr != nil {
				klog.V(4).Infof("Failed to patch event %s after race: %v", eventName, patchErr)
			}
		} else {
			klog.Warningf("Failed to create event: %v", createErr)
		}
	} else {
		klog.V(2).Infof("Recorded event: %s - %s", reason, message)
	}
}

// recordEvent uses appropriate deduplication windows based on event type.
func recordEvent(ctx context.Context, kubeClient kubernetes.Interface, reason, message, eventType string) {
	window := FailedActionTimeWindow
	if reason == EventReasonFencingEvent {
		window = FencingEventTimeWindow
	}
	recordEventWithDeduplication(ctx, kubeClient, reason, message, eventType, window)
}

// updatePacemakerStatusCR creates or updates the singleton "cluster" PacemakerCluster CR.
func updatePacemakerStatusCR(ctx context.Context, status *v1alpha1.PacemakerClusterStatus) error {
	config, err := getKubeConfig()
	if err != nil {
		return err
	}

	restClient, err := createPacemakerRESTClient(config)
	if err != nil {
		return err
	}

	var existing v1alpha1.PacemakerCluster
	err = restClient.Get().
		Resource(PacemakerResourceName).
		Name(PacemakerClusterResourceName).
		Do(ctx).
		Into(&existing)

	nodeCount := 0
	if status.Nodes != nil {
		nodeCount = len(*status.Nodes)
	}
	klog.V(2).Infof("Preparing to update PacemakerCluster CR with %d nodes", nodeCount)
	if status.Nodes != nil {
		for i, node := range *status.Nodes {
			klog.V(2).Infof("  Node[%d]: NodeName='%s', Addresses=%v", i, node.NodeName, node.Addresses)
		}
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("PacemakerCluster CR not found, creating new one")
			newPacemakerCluster := &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: PacemakerClusterResourceName,
				},
				Status: *status,
			}

			// Capture the server response to get the resourceVersion populated by the API server
			createdPacemakerCluster := &v1alpha1.PacemakerCluster{}
			err = restClient.Post().
				Resource(PacemakerResourceName).
				Body(newPacemakerCluster).
				Do(ctx).
				Into(createdPacemakerCluster)

			if err != nil {
				return fmt.Errorf("failed to create PacemakerCluster: %w", err)
			}

			// Status subresource requires separate PUT after create.
			// Use the created object which has the server-populated resourceVersion.
			createdPacemakerCluster.Status = *status
			statusResult := restClient.Put().
				Resource(PacemakerResourceName).
				Name(PacemakerClusterResourceName).
				SubResource(statusSubresource).
				Body(createdPacemakerCluster).
				Do(ctx)
			if statusResult.Error() != nil {
				return fmt.Errorf("failed to initialize Pacemaker status: %w", statusResult.Error())
			}
			klog.Info("Created and initialized Pacemaker CR")
			return nil
		}
		return fmt.Errorf("failed to get PacemakerCluster: %w", err)
	}

	// Skip if another collector already updated with a newer timestamp
	if !existing.Status.LastUpdated.IsZero() && !status.LastUpdated.After(existing.Status.LastUpdated.Time) {
		klog.Warningf("Skipping update: timestamp %s not newer than existing %s",
			status.LastUpdated.Format(time.RFC3339), existing.Status.LastUpdated.Format(time.RFC3339))
		return nil
	}

	existing.Status = *status

	result := restClient.Put().
		Resource(PacemakerResourceName).
		Name(PacemakerClusterResourceName).
		SubResource(statusSubresource).
		Body(&existing).
		Do(ctx)

	if result.Error() != nil {
		return fmt.Errorf("failed to update Pacemaker: %w", result.Error())
	}

	klog.Info("Updated existing Pacemaker CR")
	return nil
}
