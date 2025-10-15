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
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

const (
	// pcsStatusXMLCommand is the command to get pacemaker status in XML format.
	// Security: This is a hardcoded string (no user input) and runs with sudo -n (non-interactive).
	// The command is whitelisted in sudoers configuration for the service account.
	pcsStatusXMLCommand = "sudo -n pcs status xml"

	// pcsClusterConfigCommand is the command to get cluster configuration including node IPs.
	// This provides the authoritative list of nodes and their addresses.
	pcsClusterConfigCommand = "sudo -n pcs cluster config show --output-format json"

	// execTimeout is the timeout for executing the pcs command to prevent hanging
	execTimeout = 10 * time.Second

	// collectorTimeout is the overall timeout for the collector run
	collectorTimeout = 30 * time.Second

	// maxXMLSize prevents XML bombs and excessive memory consumption (10MB limit)
	maxXMLSize = 10 * 1024 * 1024

	// Time windows for detecting recent events
	failedActionTimeWindow = 5 * time.Minute
	fencingEventTimeWindow = 24 * time.Hour

	// Expected number of nodes in a TNF cluster
	expectedNodeCountTNF = 2

	// Kubernetes API constants (kubernetesAPIPath and pacemakerResourceName shared with healthcheck.go)
	statusSubresource = "status"

	// Event reasons for the status collector (uses shared eventReasonFencingEvent and eventReasonFailedAction from healthcheck.go)
	eventReasonCollectionError   = "PacemakerStatusCollectionError"
	eventReasonCollectionSuccess = "PacemakerStatusCollectionSuccess"

	// Time formats for parsing Pacemaker timestamps
	pacemakerTimeFormat      = "Mon Jan 2 15:04:05 2006"
	pacemakerFenceTimeFormat = "2006-01-02 15:04:05.000000Z"

	// Namespace for events
	targetNamespace = "openshift-etcd"

	// Resource agent prefixes (uses shared resourceAgentKubelet and resourceAgentEtcd from healthcheck.go)
	resourceAgentFencing  = "stonith:"
	resourceAgentFenceAWS = "fence_aws"
	resourceAgentRedfish  = "stonith:fence_redfish"
)

// XML structures for parsing pacemaker status from "pcs status xml" command output.
// The healthcheck controller does not parse XML - it reads structured data from the Pacemaker CR.
type PacemakerResult struct {
	XMLName        xml.Name       `xml:"pacemaker-result"`
	Summary        Summary        `xml:"summary"`
	Nodes          Nodes          `xml:"nodes"`
	Resources      Resources      `xml:"resources"`
	NodeAttributes NodeAttributes `xml:"node_attributes"`
	NodeHistory    NodeHistory    `xml:"node_history"`
	FenceHistory   FenceHistory   `xml:"fence_history"`
}

type Summary struct {
	Stack               Stack               `xml:"stack"`
	CurrentDC           CurrentDC           `xml:"current_dc"`
	NodesConfigured     NodesConfigured     `xml:"nodes_configured"`
	ResourcesConfigured ResourcesConfigured `xml:"resources_configured"`
}

type Stack struct {
	PacemakerdState string `xml:"pacemakerd-state,attr"`
}

type CurrentDC struct {
	WithQuorum string `xml:"with_quorum,attr"`
}

type NodesConfigured struct {
	Number string `xml:"number,attr"`
}

type ResourcesConfigured struct {
	Number string `xml:"number,attr"`
}

type Nodes struct {
	Node []Node `xml:"node"`
}

type Node struct {
	Name             string `xml:"name,attr"`
	ID               string `xml:"id,attr"`
	Online           string `xml:"online,attr"`
	Standby          string `xml:"standby,attr"`
	StandbyOnFail    string `xml:"standby_onfail,attr"`
	Maintenance      string `xml:"maintenance,attr"`
	Pending          string `xml:"pending,attr"`
	Unclean          string `xml:"unclean,attr"`
	Shutdown         string `xml:"shutdown,attr"`
	ExpectedUp       string `xml:"expected_up,attr"`
	IsDC             string `xml:"is_dc,attr"`
	ResourcesRunning string `xml:"resources_running,attr"`
	Type             string `xml:"type,attr"`
}

type NodeAttributes struct {
	Node []NodeAttributeSet `xml:"node"`
}

type NodeAttributeSet struct {
	Name      string          `xml:"name,attr"`
	Attribute []NodeAttribute `xml:"attribute"`
}

type NodeAttribute struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type Resources struct {
	Clone    []Clone    `xml:"clone"`
	Resource []Resource `xml:"resource"`
}

type Clone struct {
	Resource []Resource `xml:"resource"`
}

type Resource struct {
	ID             string  `xml:"id,attr"`
	ResourceAgent  string  `xml:"resource_agent,attr"`
	Role           string  `xml:"role,attr"`
	TargetRole     string  `xml:"target_role,attr"`
	Active         string  `xml:"active,attr"`
	Orphaned       string  `xml:"orphaned,attr"`
	Blocked        string  `xml:"blocked,attr"`
	Managed        string  `xml:"managed,attr"`
	Failed         string  `xml:"failed,attr"`
	FailureIgnored string  `xml:"failure_ignored,attr"`
	NodesRunningOn string  `xml:"nodes_running_on,attr"`
	Node           NodeRef `xml:"node"`
}

type NodeRef struct {
	Name string `xml:"name,attr"`
}

type NodeHistory struct {
	Node []NodeHistoryNode `xml:"node"`
}

type NodeHistoryNode struct {
	Name            string            `xml:"name,attr"`
	ResourceHistory []ResourceHistory `xml:"resource_history"`
}

type ResourceHistory struct {
	ID               string             `xml:"id,attr"`
	OperationHistory []OperationHistory `xml:"operation_history"`
}

type OperationHistory struct {
	Call         string `xml:"call,attr"`
	Task         string `xml:"task,attr"`
	RC           string `xml:"rc,attr"`
	RCText       string `xml:"rc_text,attr"`
	ExitReason   string `xml:"exit-reason,attr"`
	LastRCChange string `xml:"last-rc-change,attr"`
	ExecTime     string `xml:"exec-time,attr"`
	QueueTime    string `xml:"queue-time,attr"`
}

type FenceHistory struct {
	FenceEvent []FenceEvent `xml:"fence_event"`
}

type FenceEvent struct {
	Target     string `xml:"target,attr"`
	Action     string `xml:"action,attr"`
	Delegate   string `xml:"delegate,attr"`
	Client     string `xml:"client,attr"`
	Origin     string `xml:"origin,attr"`
	Status     string `xml:"status,attr"`
	ExitReason string `xml:"exit-reason,attr"`
	Completed  string `xml:"completed,attr"`
}

// JSON structures for parsing "pcs cluster config show --output-format json" output.
// This provides the authoritative list of nodes and their IP addresses.
type ClusterConfig struct {
	ClusterName string              `json:"cluster_name"`
	ClusterUUID string              `json:"cluster_uuid"`
	Nodes       []ClusterConfigNode `json:"nodes"`
}

type ClusterConfigNode struct {
	Name   string                  `json:"name"`
	NodeID string                  `json:"nodeid"`
	Addrs  []ClusterConfigNodeAddr `json:"addrs"`
}

type ClusterConfigNodeAddr struct {
	Addr string `json:"addr"`
	Link string `json:"link"`
	Type string `json:"type"`
}

// ResourceStatePerNode tracks resource state per node for building conditions
type ResourceStatePerNode struct {
	// Resource running states
	KubeletRunning bool
	EtcdRunning    bool
	FencingRunning bool

	// Resource details for conditions
	KubeletResource *Resource
	EtcdResource    *Resource
	FencingResource *Resource
}

// NewPacemakerStatusCollectorCommand creates a new command for collecting pacemaker status
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

// runCollector executes the full pacemaker status collection workflow:
// 1. Fetches cluster config (nodes and IPs)
// 2. Executes "sudo -n pcs status xml" to get cluster status
// 3. Parses and processes the data
// 4. Creates events for fencing history and failed actions
// 5. Updates or creates the Pacemaker CR with the collected information
func runCollector() error {
	ctx, cancel := context.WithTimeout(context.Background(), collectorTimeout)
	defer cancel()

	klog.Info("Starting pacemaker status collection...")

	// Create kube client for events
	kubeClient, err := createKubeClient()
	if err != nil {
		return fmt.Errorf("failed to create kube client: %w", err)
	}

	// First, fetch cluster config to get authoritative node list and IPs
	clusterConfig, err := fetchClusterConfig(ctx)
	if err != nil {
		klog.Warningf("Failed to fetch cluster config: %v (will continue with limited node data)", err)
		// Don't fail collection entirely - we can still get some data from status XML
	}

	// Collect pacemaker status from XML
	result, err := collectPacemakerStatus(ctx)
	if err != nil {
		// Record an event for collection failure
		recordCollectionErrorEvent(ctx, kubeClient, err)
		return fmt.Errorf("failed to collect pacemaker status: %w", err)
	}

	// Build cluster status with conditions from the parsed data
	clusterStatus := buildClusterStatus(result, clusterConfig)

	// Create events for fencing history and failed actions
	recordFencingEvents(ctx, kubeClient, result)
	recordFailedActionEvents(ctx, kubeClient, result)

	// Update Pacemaker CR
	if err := updatePacemakerStatusCR(ctx, clusterStatus); err != nil {
		return fmt.Errorf("failed to update Pacemaker CR: %w", err)
	}

	klog.Info("Successfully updated Pacemaker CR")
	return nil
}

// createKubeClient creates a kubernetes client for recording events
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

// fetchClusterConfig executes "pcs cluster config show --output-format json" and parses the cluster configuration.
// Returns the parsed cluster config and any error encountered.
// This provides the authoritative list of nodes and their IP addresses.
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

// collectPacemakerStatus executes "pcs status xml" and parses the output into structured data.
// Returns the parsed XML result and any error encountered during collection.
func collectPacemakerStatus(ctx context.Context) (*PacemakerResult, error) {
	// Execute the pcs status xml command with a timeout
	ctxExec, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	stdout, stderr, execErr := exec.Execute(ctxExec, pcsStatusXMLCommand)
	if execErr != nil {
		return nil, fmt.Errorf("failed to execute pcs status xml command: %w", execErr)
	}

	if stderr != "" {
		klog.Warningf("pcs status xml command produced stderr: %s", stderr)
	}

	// Validate XML size to prevent XML bombs
	if len(stdout) > maxXMLSize {
		return nil, fmt.Errorf("XML output too large: %d bytes (max: %d bytes)", len(stdout), maxXMLSize)
	}

	// Parse the XML
	var result PacemakerResult
	if parseErr := xml.Unmarshal([]byte(stdout), &result); parseErr != nil {
		return nil, fmt.Errorf("failed to parse XML: %w", parseErr)
	}

	return &result, nil
}

// buildClusterStatus builds the complete PacemakerClusterStatus from parsed data
func buildClusterStatus(result *PacemakerResult, clusterConfig *ClusterConfig) *v1alpha1.PacemakerClusterStatus {
	now := metav1.Now()

	// Build resource state map per node
	resourceState := make(map[string]*ResourceStatePerNode)
	processResourcesForState(result, resourceState)

	// Create XML node lookup map
	xmlNodeMap := make(map[string]*Node)
	for i := range result.Nodes.Node {
		xmlNodeMap[result.Nodes.Node[i].Name] = &result.Nodes.Node[i]
	}

	// Build node statuses
	var nodes []v1alpha1.PacemakerClusterNodeStatus

	// If we have cluster config, use it as the authoritative node list
	if clusterConfig != nil && len(clusterConfig.Nodes) > 0 {
		klog.V(2).Infof("Building nodes from cluster config (authoritative source): %d nodes", len(clusterConfig.Nodes))

		for _, configNode := range clusterConfig.Nodes {
			if configNode.Name == "" {
				klog.Warningf("Skipping node with empty name from cluster config")
				continue
			}

			// Get IPs from cluster config
			ipAddresses := getCanonicalIPs(configNode)
			if len(ipAddresses) == 0 {
				klog.Warningf("Node %s has no valid IP addresses, skipping", configNode.Name)
				continue
			}

			// Get XML node data for status
			xmlNode := xmlNodeMap[configNode.Name]

			// Get resource state for this node
			state := resourceState[configNode.Name]
			if state == nil {
				state = &ResourceStatePerNode{}
			}

			// Build node status
			nodeStatus := buildNodeStatus(configNode.Name, ipAddresses, xmlNode, state, now)
			nodes = append(nodes, nodeStatus)
		}
	} else {
		// Fallback: use XML nodes if cluster config is unavailable
		klog.Warningf("Cluster config unavailable, falling back to XML nodes (IP addresses may be missing)")

		for _, xmlNode := range result.Nodes.Node {
			if xmlNode.Name == "" {
				klog.Warningf("Skipping node with empty name from XML")
				continue
			}

			// Get resource state for this node
			state := resourceState[xmlNode.Name]
			if state == nil {
				state = &ResourceStatePerNode{}
			}

			// Without cluster config, we don't have IP addresses
			// Use a placeholder that will fail validation - this is intentional
			// to surface the error that cluster config is unavailable
			nodeStatus := buildNodeStatus(xmlNode.Name, []string{"0.0.0.0"}, &xmlNode, state, now)
			nodes = append(nodes, nodeStatus)
		}
	}

	// Check if cluster is in maintenance mode
	clusterInMaintenance := isClusterInMaintenance(result)

	// Build cluster-level conditions
	clusterConditions := buildClusterConditions(len(nodes), clusterInMaintenance, nodes, now)

	return &v1alpha1.PacemakerClusterStatus{
		Conditions:  clusterConditions,
		LastUpdated: now,
		Nodes:       nodes,
	}
}

// buildNodeStatus builds a single node status with all conditions
func buildNodeStatus(name string, ipAddresses []string, xmlNode *Node, state *ResourceStatePerNode, now metav1.Time) v1alpha1.PacemakerClusterNodeStatus {
	// Build node conditions
	nodeConditions := buildNodeConditions(xmlNode, state, now)

	// Build resource statuses
	resources := buildResourceStatuses(state, now)

	return v1alpha1.PacemakerClusterNodeStatus{
		Conditions:  nodeConditions,
		Name:        name,
		IPAddresses: ipAddresses,
		Resources:   resources,
	}
}

// buildClusterConditions builds cluster-level conditions
func buildClusterConditions(nodeCount int, inMaintenance bool, nodes []v1alpha1.PacemakerClusterNodeStatus, now metav1.Time) []metav1.Condition {
	conditions := make([]metav1.Condition, 0, 3)

	// NodeCountAsExpected condition
	nodeCountCondition := buildNodeCountCondition(nodeCount, now)
	conditions = append(conditions, nodeCountCondition)

	// InService condition (not in maintenance mode)
	inServiceCondition := buildClusterInServiceCondition(inMaintenance, now)
	conditions = append(conditions, inServiceCondition)

	// Healthy condition (aggregate)
	healthyCondition := buildClusterHealthyCondition(nodeCountCondition, inServiceCondition, nodes, now)
	conditions = append(conditions, healthyCondition)

	return conditions
}

// buildNodeCountCondition builds the NodeCountAsExpected condition
func buildNodeCountCondition(nodeCount int, now metav1.Time) metav1.Condition {
	if nodeCount == expectedNodeCountTNF {
		return metav1.Condition{
			Type:               v1alpha1.ClusterNodeCountAsExpectedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected,
			Message:            fmt.Sprintf("Expected %d nodes, found %d", expectedNodeCountTNF, nodeCount),
			LastTransitionTime: now,
		}
	}

	reason := v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes
	if nodeCount > expectedNodeCountTNF {
		reason = v1alpha1.ClusterNodeCountAsExpectedReasonExcessiveNodes
	}

	return metav1.Condition{
		Type:               v1alpha1.ClusterNodeCountAsExpectedConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            fmt.Sprintf("Expected %d nodes, found %d", expectedNodeCountTNF, nodeCount),
		LastTransitionTime: now,
	}
}

// buildClusterInServiceCondition builds the cluster InService condition
func buildClusterInServiceCondition(inMaintenance bool, now metav1.Time) metav1.Condition {
	if !inMaintenance {
		return metav1.Condition{
			Type:               v1alpha1.ClusterInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterInServiceReasonInService,
			Message:            "Cluster is in service (not in maintenance mode)",
			LastTransitionTime: now,
		}
	}

	return metav1.Condition{
		Type:               v1alpha1.ClusterInServiceConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ClusterInServiceReasonInMaintenance,
		Message:            "Cluster is in maintenance mode",
		LastTransitionTime: now,
	}
}

// buildClusterHealthyCondition builds the aggregate Healthy condition for the cluster
func buildClusterHealthyCondition(nodeCountCondition, inServiceCondition metav1.Condition, nodes []v1alpha1.PacemakerClusterNodeStatus, now metav1.Time) metav1.Condition {
	// Check if all component conditions are healthy
	allHealthy := nodeCountCondition.Status == metav1.ConditionTrue &&
		inServiceCondition.Status == metav1.ConditionTrue

	// Check if all nodes are healthy
	for _, node := range nodes {
		nodeHealthy := getConditionStatus(node.Conditions, v1alpha1.NodeHealthyConditionType)
		if nodeHealthy != metav1.ConditionTrue {
			allHealthy = false
			break
		}
	}

	if allHealthy {
		return metav1.Condition{
			Type:               v1alpha1.ClusterHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ClusterHealthyReasonHealthy,
			Message:            "Pacemaker cluster is healthy",
			LastTransitionTime: now,
		}
	}

	return metav1.Condition{
		Type:               v1alpha1.ClusterHealthyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ClusterHealthyReasonUnhealthy,
		Message:            "Pacemaker cluster has issues that need investigation",
		LastTransitionTime: now,
	}
}

// buildNodeConditions builds all conditions for a node
func buildNodeConditions(xmlNode *Node, state *ResourceStatePerNode, now metav1.Time) []metav1.Condition {
	conditions := make([]metav1.Condition, 0, 7)

	// Online condition
	online := xmlNode != nil && xmlNode.Online == "true"
	conditions = append(conditions, buildNodeOnlineCondition(online, now))

	// InService condition (not in maintenance)
	inMaintenance := xmlNode != nil && xmlNode.Maintenance == "true"
	conditions = append(conditions, buildNodeInServiceCondition(!inMaintenance, now))

	// Active condition (not in standby)
	standby := xmlNode != nil && (xmlNode.Standby == "true" || xmlNode.StandbyOnFail == "true")
	conditions = append(conditions, buildNodeActiveCondition(!standby, now))

	// Ready condition (not pending)
	pending := xmlNode != nil && xmlNode.Pending == "true"
	conditions = append(conditions, buildNodeReadyCondition(!pending, now))

	// Clean condition
	unclean := xmlNode != nil && xmlNode.Unclean == "true"
	conditions = append(conditions, buildNodeCleanCondition(!unclean, now))

	// Member condition
	isMember := xmlNode != nil && xmlNode.Type == "member"
	conditions = append(conditions, buildNodeMemberCondition(isMember, now))

	// Healthy condition (aggregate)
	healthy := online && !inMaintenance && !standby && !pending && !unclean && isMember
	// Also check resource health
	if state != nil {
		healthy = healthy && state.KubeletRunning && state.EtcdRunning && state.FencingRunning
	}
	conditions = append(conditions, buildNodeHealthyCondition(healthy, now))

	return conditions
}

func buildNodeOnlineCondition(online bool, now metav1.Time) metav1.Condition {
	if online {
		return metav1.Condition{
			Type:               v1alpha1.NodeOnlineConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeOnlineReasonOnline,
			Message:            "Node is online",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeOnlineConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeOnlineReasonOffline,
		Message:            "Node is offline",
		LastTransitionTime: now,
	}
}

func buildNodeInServiceCondition(inService bool, now metav1.Time) metav1.Condition {
	if inService {
		return metav1.Condition{
			Type:               v1alpha1.NodeInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeInServiceReasonInService,
			Message:            "Node is in service",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeInServiceConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeInServiceReasonInMaintenance,
		Message:            "Node is in maintenance mode",
		LastTransitionTime: now,
	}
}

func buildNodeActiveCondition(active bool, now metav1.Time) metav1.Condition {
	if active {
		return metav1.Condition{
			Type:               v1alpha1.NodeActiveConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeActiveReasonActive,
			Message:            "Node is active",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeActiveConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeActiveReasonStandby,
		Message:            "Node is in standby mode",
		LastTransitionTime: now,
	}
}

func buildNodeReadyCondition(ready bool, now metav1.Time) metav1.Condition {
	if ready {
		return metav1.Condition{
			Type:               v1alpha1.NodeReadyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeReadyReasonReady,
			Message:            "Node is ready",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeReadyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeReadyReasonPending,
		Message:            "Node is pending",
		LastTransitionTime: now,
	}
}

func buildNodeCleanCondition(clean bool, now metav1.Time) metav1.Condition {
	if clean {
		return metav1.Condition{
			Type:               v1alpha1.NodeCleanConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeCleanReasonClean,
			Message:            "Node is in a clean state",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeCleanConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeCleanReasonUnclean,
		Message:            "Node is in an unclean state",
		LastTransitionTime: now,
	}
}

func buildNodeMemberCondition(isMember bool, now metav1.Time) metav1.Condition {
	if isMember {
		return metav1.Condition{
			Type:               v1alpha1.NodeMemberConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeMemberReasonMember,
			Message:            "Node is a cluster member",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeMemberConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeMemberReasonNotMember,
		Message:            "Node is not a cluster member",
		LastTransitionTime: now,
	}
}

func buildNodeHealthyCondition(healthy bool, now metav1.Time) metav1.Condition {
	if healthy {
		return metav1.Condition{
			Type:               v1alpha1.NodeHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.NodeHealthyReasonHealthy,
			Message:            "Node is healthy",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.NodeHealthyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.NodeHealthyReasonUnhealthy,
		Message:            "Node has issues that need investigation",
		LastTransitionTime: now,
	}
}

// buildResourceStatuses builds resource statuses for a node
func buildResourceStatuses(state *ResourceStatePerNode, now metav1.Time) []v1alpha1.PacemakerClusterResourceStatus {
	resources := make([]v1alpha1.PacemakerClusterResourceStatus, 0, 3)

	// Kubelet resource
	resources = append(resources, buildResourceStatus(
		v1alpha1.PacemakerClusterResourceNameKubelet,
		state.KubeletResource,
		state.KubeletRunning,
		now,
	))

	// Etcd resource
	resources = append(resources, buildResourceStatus(
		v1alpha1.PacemakerClusterResourceNameEtcd,
		state.EtcdResource,
		state.EtcdRunning,
		now,
	))

	// Fencing agent resource
	resources = append(resources, buildResourceStatus(
		v1alpha1.PacemakerClusterResourceNameFencingAgent,
		state.FencingResource,
		state.FencingRunning,
		now,
	))

	return resources
}

// buildResourceStatus builds a resource status with all conditions
func buildResourceStatus(name v1alpha1.PacemakerClusterResourceName, resource *Resource, running bool, now metav1.Time) v1alpha1.PacemakerClusterResourceStatus {
	conditions := buildResourceConditions(resource, running, now)

	return v1alpha1.PacemakerClusterResourceStatus{
		Conditions: conditions,
		Name:       name,
	}
}

// buildResourceConditions builds all conditions for a resource
func buildResourceConditions(resource *Resource, running bool, now metav1.Time) []metav1.Condition {
	conditions := make([]metav1.Condition, 0, 8)

	// Derive states from resource or use defaults
	var inMaintenance, managed, enabled, operational, active, started, schedulable bool

	if resource != nil {
		// Resource is in maintenance if maintenance="true"
		inMaintenance = false // Pacemaker XML doesn't expose this per-resource, only per-node
		managed = resource.Managed == "true"
		enabled = resource.TargetRole != "Stopped"
		operational = resource.Failed != "true"
		active = resource.Active == "true"
		started = resource.Role == "Started"
		schedulable = resource.Blocked != "true"
	} else {
		// Default values when resource not found
		inMaintenance = false
		managed = false
		enabled = false
		operational = false
		active = false
		started = false
		schedulable = false
	}

	// InService condition
	conditions = append(conditions, buildResourceInServiceCondition(!inMaintenance, now))

	// Managed condition
	conditions = append(conditions, buildResourceManagedCondition(managed, now))

	// Enabled condition
	conditions = append(conditions, buildResourceEnabledCondition(enabled, now))

	// Operational condition
	conditions = append(conditions, buildResourceOperationalCondition(operational, now))

	// Active condition
	conditions = append(conditions, buildResourceActiveCondition(active, now))

	// Started condition
	conditions = append(conditions, buildResourceStartedCondition(started, now))

	// Schedulable condition
	conditions = append(conditions, buildResourceSchedulableCondition(schedulable, now))

	// Healthy condition (aggregate)
	healthy := !inMaintenance && managed && enabled && operational && active && started && schedulable
	conditions = append(conditions, buildResourceHealthyCondition(healthy, now))

	return conditions
}

func buildResourceInServiceCondition(inService bool, now metav1.Time) metav1.Condition {
	if inService {
		return metav1.Condition{
			Type:               v1alpha1.ResourceInServiceConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceInServiceReasonInService,
			Message:            "Resource is in service",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceInServiceConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceInServiceReasonInMaintenance,
		Message:            "Resource is in maintenance mode",
		LastTransitionTime: now,
	}
}

func buildResourceManagedCondition(managed bool, now metav1.Time) metav1.Condition {
	if managed {
		return metav1.Condition{
			Type:               v1alpha1.ResourceManagedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceManagedReasonManaged,
			Message:            "Resource is managed by pacemaker",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceManagedConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceManagedReasonUnmanaged,
		Message:            "Resource is not managed by pacemaker",
		LastTransitionTime: now,
	}
}

func buildResourceEnabledCondition(enabled bool, now metav1.Time) metav1.Condition {
	if enabled {
		return metav1.Condition{
			Type:               v1alpha1.ResourceEnabledConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceEnabledReasonEnabled,
			Message:            "Resource is enabled",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceEnabledConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceEnabledReasonDisabled,
		Message:            "Resource is disabled",
		LastTransitionTime: now,
	}
}

func buildResourceOperationalCondition(operational bool, now metav1.Time) metav1.Condition {
	if operational {
		return metav1.Condition{
			Type:               v1alpha1.ResourceOperationalConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceOperationalReasonOperational,
			Message:            "Resource is operational",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceOperationalConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceOperationalReasonFailed,
		Message:            "Resource has failed",
		LastTransitionTime: now,
	}
}

func buildResourceActiveCondition(active bool, now metav1.Time) metav1.Condition {
	if active {
		return metav1.Condition{
			Type:               v1alpha1.ResourceActiveConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceActiveReasonActive,
			Message:            "Resource is active",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceActiveConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceActiveReasonInactive,
		Message:            "Resource is not active",
		LastTransitionTime: now,
	}
}

func buildResourceStartedCondition(started bool, now metav1.Time) metav1.Condition {
	if started {
		return metav1.Condition{
			Type:               v1alpha1.ResourceStartedConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceStartedReasonStarted,
			Message:            "Resource is started",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceStartedConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceStartedReasonStopped,
		Message:            "Resource is stopped",
		LastTransitionTime: now,
	}
}

func buildResourceSchedulableCondition(schedulable bool, now metav1.Time) metav1.Condition {
	if schedulable {
		return metav1.Condition{
			Type:               v1alpha1.ResourceSchedulableConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceSchedulableReasonSchedulable,
			Message:            "Resource is schedulable",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceSchedulableConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceSchedulableReasonUnschedulable,
		Message:            "Resource is unschedulable (blocked)",
		LastTransitionTime: now,
	}
}

func buildResourceHealthyCondition(healthy bool, now metav1.Time) metav1.Condition {
	if healthy {
		return metav1.Condition{
			Type:               v1alpha1.ResourceHealthyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ResourceHealthyReasonHealthy,
			Message:            "Resource is healthy",
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               v1alpha1.ResourceHealthyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ResourceHealthyReasonUnhealthy,
		Message:            "Resource has issues that need investigation",
		LastTransitionTime: now,
	}
}

// getCanonicalIPs extracts and validates IP addresses from cluster config node
func getCanonicalIPs(configNode ClusterConfigNode) []string {
	if len(configNode.Addrs) == 0 {
		klog.Warningf("Node %s in cluster config has no addresses", configNode.Name)
		return nil
	}

	var ips []string
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
		ips = append(ips, parsedIP.String())
	}

	return ips
}

// isClusterInMaintenance checks if the cluster is in maintenance mode
func isClusterInMaintenance(result *PacemakerResult) bool {
	// Check if all nodes are in maintenance mode
	for _, node := range result.Nodes.Node {
		if node.Maintenance == "true" {
			return true
		}
	}
	return false
}

// processResourcesForState processes all resources and builds state map per node
func processResourcesForState(result *PacemakerResult, resourceState map[string]*ResourceStatePerNode) {
	// Helper function to process a single resource
	processResource := func(resource Resource) {
		if resource.Node.Name == "" {
			return // Skip resources without a node assignment
		}

		// Get or create state entry for this node
		if _, exists := resourceState[resource.Node.Name]; !exists {
			resourceState[resource.Node.Name] = &ResourceStatePerNode{}
		}
		state := resourceState[resource.Node.Name]

		// A resource is running if it's Started and Active
		isRunning := resource.Role == "Started" && resource.Active == "true"

		// Determine which resource type this is and store both running state and resource details
		switch {
		case strings.HasPrefix(resource.ResourceAgent, resourceAgentKubelet):
			state.KubeletRunning = isRunning
			state.KubeletResource = &resource
		case strings.HasPrefix(resource.ResourceAgent, resourceAgentEtcd):
			state.EtcdRunning = isRunning
			state.EtcdResource = &resource
		case strings.HasPrefix(resource.ResourceAgent, resourceAgentFencing) ||
			strings.Contains(resource.ResourceAgent, resourceAgentFenceAWS):
			// Fencing resources (stonith: prefix or fence_aws)
			state.FencingRunning = isRunning
			state.FencingResource = &resource
		}
	}

	// Process clone resources
	for _, clone := range result.Resources.Clone {
		for _, resource := range clone.Resource {
			processResource(resource)
		}
	}

	// Process standalone resources
	for _, resource := range result.Resources.Resource {
		processResource(resource)
	}
}

// getConditionStatus gets the status of a condition by type from a list of conditions
func getConditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}

// recordFencingEvents records Kubernetes events for recent fencing events
func recordFencingEvents(ctx context.Context, kubeClient kubernetes.Interface, result *PacemakerResult) {
	fenceCutoffTime := time.Now().Add(-fencingEventTimeWindow)

	for _, fenceEvent := range result.FenceHistory.FenceEvent {
		if fenceEvent.Target == "" {
			continue
		}

		// Parse the timestamp
		t, err := time.Parse(pacemakerFenceTimeFormat, fenceEvent.Completed)
		if err != nil {
			klog.V(4).Infof("Skipping fencing event due to timestamp parse error: %v", err)
			continue
		}

		// Only include recent fencing events
		if !t.After(fenceCutoffTime) {
			continue
		}

		// Create event
		message := fmt.Sprintf("Fencing event: %s of %s completed with status %s at %s",
			fenceEvent.Action, fenceEvent.Target, fenceEvent.Status, fenceEvent.Completed)
		recordEvent(ctx, kubeClient, eventReasonFencingEvent, message, corev1.EventTypeWarning)
	}
}

// recordFailedActionEvents records Kubernetes events for recent failed resource actions
func recordFailedActionEvents(ctx context.Context, kubeClient kubernetes.Interface, result *PacemakerResult) {
	cutoffTime := time.Now().Add(-failedActionTimeWindow)

	for _, node := range result.NodeHistory.Node {
		for _, resourceHistory := range node.ResourceHistory {
			for _, operation := range resourceHistory.OperationHistory {
				// Only record failed operations (rc != 0)
				if operation.RC == "0" {
					continue
				}

				// Parse the timestamp
				t, err := time.Parse(pacemakerTimeFormat, operation.LastRCChange)
				if err != nil {
					klog.V(4).Infof("Skipping operation history due to timestamp parse error: %v", err)
					continue
				}

				// Only include recent operations
				if !t.After(cutoffTime) {
					continue
				}

				// Create event
				message := fmt.Sprintf("Failed resource action: %s %s on node %s (rc=%s: %s) at %s",
					resourceHistory.ID, operation.Task, node.Name, operation.RC, operation.RCText, operation.LastRCChange)
				recordEvent(ctx, kubeClient, eventReasonFailedAction, message, corev1.EventTypeWarning)
			}
		}
	}
}

// recordCollectionErrorEvent records an event for collection errors
func recordCollectionErrorEvent(ctx context.Context, kubeClient kubernetes.Interface, err error) {
	message := fmt.Sprintf("Failed to collect pacemaker status: %v", err)
	recordEvent(ctx, kubeClient, eventReasonCollectionError, message, corev1.EventTypeWarning)
}

// generateEventName generates a consistent event name based on content for deduplication
// The name is based on a hash of the reason and message, allowing Kubernetes to aggregate
// events with the same content.
func generateEventName(reason, message string) string {
	hash := sha256.Sum256([]byte(reason + message))
	shortHash := hex.EncodeToString(hash[:])[:12]
	return fmt.Sprintf("pacemaker-%s-%s", strings.ToLower(reason), shortHash)
}

// recordEventWithDeduplication records a Kubernetes event with deduplication.
// If an event with the same name exists and was created recently, it updates the count instead of creating a new one.
func recordEventWithDeduplication(ctx context.Context, kubeClient kubernetes.Interface, reason, message, eventType string, deduplicationWindow time.Duration) {
	eventName := generateEventName(reason, message)

	// Try to get existing event
	existingEvent, err := kubeClient.CoreV1().Events(targetNamespace).Get(ctx, eventName, metav1.GetOptions{})
	if err == nil && existingEvent != nil {
		// Event exists - check if it's within the deduplication window
		if time.Since(existingEvent.LastTimestamp.Time) < deduplicationWindow {
			// Update the existing event (increment count and update timestamp)
			existingEvent.Count++
			existingEvent.LastTimestamp = metav1.Now()

			_, updateErr := kubeClient.CoreV1().Events(targetNamespace).Update(ctx, existingEvent, metav1.UpdateOptions{})
			if updateErr != nil {
				klog.V(4).Infof("Failed to update existing event %s: %v (will create new)", eventName, updateErr)
			} else {
				klog.V(4).Infof("Updated existing event: %s (count=%d)", eventName, existingEvent.Count)
				return
			}
		}
		// Event exists but is outside the window - delete it and create fresh
		_ = kubeClient.CoreV1().Events(targetNamespace).Delete(ctx, eventName, metav1.DeleteOptions{})
	}

	// Create new event
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: targetNamespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "PacemakerCluster",
			Name:       PacemakerClusterResourceName,
			Namespace:  "", // Cluster-scoped
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
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

	_, createErr := kubeClient.CoreV1().Events(targetNamespace).Create(ctx, event, metav1.CreateOptions{})
	if createErr != nil {
		if apierrors.IsAlreadyExists(createErr) {
			// Race condition - another collector created the event; try to update instead
			klog.V(4).Infof("Event %s already exists, attempting update", eventName)
			existingEvent, getErr := kubeClient.CoreV1().Events(targetNamespace).Get(ctx, eventName, metav1.GetOptions{})
			if getErr == nil {
				existingEvent.Count++
				existingEvent.LastTimestamp = metav1.Now()
				_, _ = kubeClient.CoreV1().Events(targetNamespace).Update(ctx, existingEvent, metav1.UpdateOptions{})
			}
		} else {
			klog.Warningf("Failed to create event: %v", createErr)
		}
	} else {
		klog.V(2).Infof("Recorded event: %s - %s", reason, message)
	}
}

// recordEvent records a Kubernetes event for the PacemakerCluster (legacy, no deduplication)
func recordEvent(ctx context.Context, kubeClient kubernetes.Interface, reason, message, eventType string) {
	// Use default deduplication windows based on event type
	var window time.Duration
	switch reason {
	case eventReasonFencingEvent:
		window = fencingEventTimeWindow // 24 hours for fencing events
	default:
		window = failedActionTimeWindow // 5 minutes for other events
	}
	recordEventWithDeduplication(ctx, kubeClient, reason, message, eventType, window)
}

// updatePacemakerStatusCR updates or creates the PacemakerCluster custom resource
// with the collected status information. The CR is named "cluster" and is cluster-scoped.
// If the CR doesn't exist, it will be created; otherwise, its status subresource is updated.
func updatePacemakerStatusCR(ctx context.Context, status *v1alpha1.PacemakerClusterStatus) error {
	// Create REST client for the Pacemaker CRD
	config, err := getKubeConfig()
	if err != nil {
		return err
	}

	restClient, err := createPacemakerRESTClient(config)
	if err != nil {
		return err
	}

	// Try to get existing Pacemaker
	var existing v1alpha1.PacemakerCluster
	err = restClient.Get().
		Resource(pacemakerResourceName).
		Name(PacemakerClusterResourceName).
		Do(ctx).
		Into(&existing)

	// Log what we're about to send to the API
	klog.V(2).Infof("Preparing to update PacemakerCluster CR with %d nodes", len(status.Nodes))
	for i, node := range status.Nodes {
		klog.V(2).Infof("  Node[%d]: Name='%s', IPAddresses=%v", i, node.Name, node.IPAddresses)
	}

	if err != nil {
		// Create new Pacemaker if it doesn't exist
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
				Status: status,
			}

			result := restClient.Post().
				Resource(pacemakerResourceName).
				Body(newPacemakerCluster).
				Do(ctx)

			if result.Error() != nil {
				return fmt.Errorf("failed to create PacemakerCluster: %w", result.Error())
			}

			// Ensure status is set on initial create when CRD uses the status subresource
			result = restClient.Put().
				Resource(pacemakerResourceName).
				Name(PacemakerClusterResourceName).
				SubResource(statusSubresource).
				Body(newPacemakerCluster).
				Do(ctx)
			if result.Error() != nil {
				return fmt.Errorf("failed to initialize Pacemaker status: %w", result.Error())
			}
			klog.Info("Created and initialized Pacemaker CR")

			return nil
		}
		return fmt.Errorf("failed to get PacemakerCluster: %w", err)
	}

	// Initialize Status field if it's nil to avoid nil pointer dereference
	if existing.Status == nil {
		existing.Status = &v1alpha1.PacemakerClusterStatus{}
	}

	// Check if our timestamp is newer than the existing one to prevent race conditions
	// This can happen when multiple collector jobs run concurrently (Replace or Allow concurrency policy)
	if !existing.Status.LastUpdated.IsZero() && !status.LastUpdated.After(existing.Status.LastUpdated.Time) {
		klog.Warningf("Skipping update: our timestamp (%s) is not newer than existing timestamp (%s). This likely means another collector job has already updated the status.",
			status.LastUpdated.Format(time.RFC3339), existing.Status.LastUpdated.Format(time.RFC3339))
		return nil
	}

	// Update existing Pacemaker CR
	existing.Status = status

	result := restClient.Put().
		Resource(pacemakerResourceName).
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
