package pacemaker

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// maxRecentEvents is the maximum number of recent events to include in the CR
	// This applies to both node history and fencing history separately
	maxRecentEvents = 16

	// Node attribute names
	nodeAttributeIP = "node_ip"

	// Time formats for parsing Pacemaker timestamps
	pacemakerTimeFormat      = "Mon Jan 2 15:04:05 2006"
	pacemakerFenceTimeFormat = "2006-01-02 15:04:05.000000Z"

	// Kubernetes API constants (kubernetesAPIPath and pacemakerResourceName shared with healthcheck.go)
	statusSubresource = "status"

	// Environment variables
	envKubeconfig         = "KUBECONFIG"
	envHome               = "HOME"
	defaultKubeconfigPath = "/.kube/config"
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
// 3. Parses and merges the data
// 4. Updates or creates the Pacemaker CR with the collected information
func runCollector() error {
	ctx, cancel := context.WithTimeout(context.Background(), collectorTimeout)
	defer cancel()

	klog.Info("Starting pacemaker status collection...")

	// First, fetch cluster config to get authoritative node list and IPs
	clusterConfig, err := fetchClusterConfig(ctx)
	if err != nil {
		klog.Warningf("Failed to fetch cluster config: %v (will continue with limited node data)", err)
		// Don't fail collection entirely - we can still get some data from status XML
	}

	// Collect pacemaker status from XML
	rawXML, summary, nodes, resources, nodeHistory, fencingHistory, collectionError := collectPacemakerStatus(ctx, clusterConfig)

	// Update Pacemaker CR
	if err := updatePacemakerStatusCR(ctx, rawXML, summary, nodes, resources, nodeHistory, fencingHistory, collectionError); err != nil {
		return fmt.Errorf("failed to update Pacemaker CR: %w", err)
	}

	klog.Info("Successfully updated Pacemaker CR")
	return nil
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
// It merges data from the cluster config (nodes, IPs) with runtime status from XML.
// Returns the raw XML, parsed status components, and any error encountered during collection.
// If an error occurs, collectionError will contain the error message and other return values
// will be zero/empty values.
func collectPacemakerStatus(ctx context.Context, clusterConfig *ClusterConfig) (
	rawXML string,
	summary *v1alpha1.PacemakerSummary,
	nodes []v1alpha1.PacemakerNodeStatus,
	resources []v1alpha1.PacemakerResourceStatus,
	nodeHistory []v1alpha1.PacemakerNodeHistoryEntry,
	fencingHistory []v1alpha1.PacemakerFencingEvent,
	collectionError string,
) {
	// Execute the pcs status xml command with a timeout
	ctxExec, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	stdout, stderr, err := exec.Execute(ctxExec, pcsStatusXMLCommand)
	if err != nil {
		collectionError = fmt.Sprintf("Failed to execute pcs status xml command: %v", err)
		klog.Warning(collectionError)
		return "", summary, nil, nil, nil, nil, collectionError
	}

	if stderr != "" {
		klog.Warningf("pcs status xml command produced stderr: %s", stderr)
	}

	// Validate XML size to prevent XML bombs
	if len(stdout) > maxXMLSize {
		collectionError = fmt.Sprintf("XML output too large: %d bytes (max: %d bytes)", len(stdout), maxXMLSize)
		klog.Warning(collectionError)
		return "", summary, nil, nil, nil, nil, collectionError
	}

	rawXML = stdout

	// Parse the XML to create a summary
	var result PacemakerResult
	if err := xml.Unmarshal([]byte(rawXML), &result); err != nil {
		collectionError = fmt.Sprintf("Failed to parse XML: %v", err)
		klog.Warning(collectionError)
		// Still return the raw XML even if parsing fails
		return rawXML, summary, nil, nil, nil, nil, collectionError
	}

	// Build all status components, merging cluster config data with XML status
	summary, nodes, resources, nodeHistory, fencingHistory = buildStatusComponents(&result, clusterConfig)

	return rawXML, summary, nodes, resources, nodeHistory, fencingHistory, ""
}

// buildStatusComponents parses a PacemakerResult (XML) and ClusterConfig (JSON) into structured status components.
// It starts with the authoritative node list from cluster config, then enriches with runtime data from XML.
// All available data is included, even if some fields are missing.
// Historical data is filtered to recent time windows (5 minutes for operations, 24 hours for fencing).
func buildStatusComponents(result *PacemakerResult, clusterConfig *ClusterConfig) (
	summary *v1alpha1.PacemakerSummary,
	nodes []v1alpha1.PacemakerNodeStatus,
	resources []v1alpha1.PacemakerResourceStatus,
	nodeHistory []v1alpha1.PacemakerNodeHistoryEntry,
	fencingHistory []v1alpha1.PacemakerFencingEvent,
) {
	// Build high-level summary
	// Convert quorum boolean to typed enum
	quorumStatus := v1alpha1.QuorumStatusNoQuorum
	if result.Summary.CurrentDC.WithQuorum == "true" {
		quorumStatus = v1alpha1.QuorumStatusQuorate
	}

	// Convert pacemaker daemon state to typed enum
	// - "running" → Running
	// - Any other non-empty value (init, shutting_down, etc.) → KnownNotRunning
	// - Empty → leave empty (unknown)
	var pacemakerDaemonState v1alpha1.PacemakerDaemonStateType
	if result.Summary.Stack.PacemakerdState != "" {
		if result.Summary.Stack.PacemakerdState == "running" {
			pacemakerDaemonState = v1alpha1.PacemakerDaemonStateRunning
		} else {
			pacemakerDaemonState = v1alpha1.PacemakerDaemonStateNotRunning
		}
	}

	summary = &v1alpha1.PacemakerSummary{
		PacemakerDaemonState: pacemakerDaemonState,
		QuorumStatus:         quorumStatus,
	}

	// Build detailed node information starting from cluster config (authoritative source)
	// Then enrich with XML data where available
	onlineCount := int32(0)

	// Create XML node lookup map for enrichment
	xmlNodeMap := make(map[string]*Node)
	for i := range result.Nodes.Node {
		xmlNodeMap[result.Nodes.Node[i].Name] = &result.Nodes.Node[i]
	}

	// If we have cluster config, use it as the authoritative node list
	if clusterConfig != nil && len(clusterConfig.Nodes) > 0 {
		klog.V(2).Infof("Building nodes from cluster config (authoritative source): %d nodes", len(clusterConfig.Nodes))

		for _, configNode := range clusterConfig.Nodes {
			// Get IP from cluster config (first address)
			var ipAddress string
			if len(configNode.Addrs) > 0 {
				ipAddress = configNode.Addrs[0].Addr
				klog.V(4).Infof("Node %s from cluster config: IP=%s", configNode.Name, ipAddress)
			} else {
				klog.Warningf("Node %s in cluster config has no addresses, will be included with empty IP", configNode.Name)
			}

			// Default values (will be enriched from XML if available)
			onlineStatus := v1alpha1.NodeOnlineStatusOffline
			mode := v1alpha1.NodeModeActive

			// Enrich with XML data if available
			if xmlNode, exists := xmlNodeMap[configNode.Name]; exists {
				klog.V(4).Infof("Enriching node %s with XML data: online=%s, standby=%s", configNode.Name, xmlNode.Online, xmlNode.Standby)

				if xmlNode.Online == "true" {
					onlineStatus = v1alpha1.NodeOnlineStatusOnline
					onlineCount++
				}

				if xmlNode.Standby == "true" {
					mode = v1alpha1.NodeModeStandby
				}
			} else {
				klog.V(2).Infof("Node %s from cluster config not found in XML status (may be offline)", configNode.Name)
			}

			// Validate and canonicalize IP if present
			var canonicalIP string
			if ipAddress != "" {
				parsedIP := net.ParseIP(ipAddress)
				if parsedIP == nil {
					klog.Errorf("Failed to parse IP address '%s' for node %s: not a valid IP, will include node with empty IP", ipAddress, configNode.Name)
				} else if !parsedIP.IsGlobalUnicast() {
					klog.Errorf("IP address '%s' for node %s is not a global unicast address, will include node with empty IP", ipAddress, configNode.Name)
				} else {
					canonicalIP = parsedIP.String()
					klog.V(2).Infof("Node %s: validated IP=%s (canonical=%s)", configNode.Name, ipAddress, canonicalIP)
				}
			}

			// Always create node status, even if some fields are missing
			nodeStatus := v1alpha1.PacemakerNodeStatus{
				Name:         configNode.Name,
				OnlineStatus: onlineStatus,
				Mode:         mode,
				IPAddress:    canonicalIP, // May be empty if validation failed
			}

			klog.V(2).Infof("Created nodeStatus for %s: Name='%s', IPAddress='%s', OnlineStatus='%s', Mode='%s'",
				configNode.Name, nodeStatus.Name, nodeStatus.IPAddress, nodeStatus.OnlineStatus, nodeStatus.Mode)

			nodes = append(nodes, nodeStatus)
		}
	} else {
		// Fallback: use XML nodes if cluster config is unavailable
		klog.Warningf("Cluster config unavailable, falling back to XML nodes (IP addresses may be missing)")

		for _, xmlNode := range result.Nodes.Node {
			onlineStatus := v1alpha1.NodeOnlineStatusOffline
			if xmlNode.Online == "true" {
				onlineStatus = v1alpha1.NodeOnlineStatusOnline
				onlineCount++
			}

			mode := v1alpha1.NodeModeActive
			if xmlNode.Standby == "true" {
				mode = v1alpha1.NodeModeStandby
			}

			// Without cluster config, we won't have IPs, but still include the node
			nodeStatus := v1alpha1.PacemakerNodeStatus{
				Name:         xmlNode.Name,
				OnlineStatus: onlineStatus,
				Mode:         mode,
				IPAddress:    "", // No IP available without cluster config
			}

			klog.V(2).Infof("Created nodeStatus from XML for %s (no cluster config): Name='%s', OnlineStatus='%s'",
				xmlNode.Name, nodeStatus.Name, nodeStatus.OnlineStatus)

			nodes = append(nodes, nodeStatus)
		}
	}
	totalNodes := int32(len(result.Nodes.Node))
	summary.NodesOnline = &onlineCount
	summary.NodesTotal = &totalNodes

	// Build resource information
	resourcesStarted := int32(0)

	// Helper function to process a resource
	processResource := func(resource Resource) {
		// Convert active status to typed enum - only set if it's a valid value (Active or Inactive)
		// Other values are left as empty string to indicate unknown status
		var activeStatus v1alpha1.ResourceActiveStatusType
		if resource.Active == "true" {
			activeStatus = v1alpha1.ResourceActiveStatusActive
		} else if resource.Active == "false" {
			activeStatus = v1alpha1.ResourceActiveStatusInactive
		}

		// Convert role to typed enum - only set if it's a valid value (Started or Stopped)
		// Other roles (e.g., promoted, unpromoted) are left as empty string
		var role v1alpha1.ResourceRoleType
		if resource.Role == string(v1alpha1.ResourceRoleStarted) || resource.Role == string(v1alpha1.ResourceRoleStopped) {
			role = v1alpha1.ResourceRoleType(resource.Role)
		}

		started := resource.Role == string(v1alpha1.ResourceRoleStarted) && activeStatus == v1alpha1.ResourceActiveStatusActive
		if started {
			resourcesStarted++
		}

		resources = append(resources, v1alpha1.PacemakerResourceStatus{
			Name:          resource.ID,
			ResourceAgent: resource.ResourceAgent,
			Role:          role,
			ActiveStatus:  activeStatus,
			Node:          resource.Node.Name,
		})
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

	totalResources := int32(len(resources))
	summary.ResourcesStarted = &resourcesStarted
	summary.ResourcesTotal = &totalResources

	// Build node history (recent operations)
	cutoffTime := time.Now().Add(-failedActionTimeWindow)

	for _, node := range result.NodeHistory.Node {
		for _, resourceHistory := range node.ResourceHistory {
			for _, operation := range resourceHistory.OperationHistory {
				// Parse the timestamp
				t, err := time.Parse(pacemakerTimeFormat, operation.LastRCChange)
				if err != nil {
					klog.Warningf("Failed to parse operation timestamp: %v", err)
					continue
				}

				// Only include recent operations
				if !t.After(cutoffTime) {
					continue
				}

				// Parse RC
				rc := int32(0)
				if operation.RC != "" {
					if _, err := fmt.Sscanf(operation.RC, "%d", &rc); err != nil {
						klog.Warningf("Failed to parse RC value '%s': %v", operation.RC, err)
					}
				}

				nodeHistory = append(nodeHistory, v1alpha1.PacemakerNodeHistoryEntry{
					Node:         node.Name,
					Resource:     resourceHistory.ID,
					Operation:    operation.Task,
					RC:           &rc,
					RCText:       operation.RCText,
					LastRCChange: metav1.NewTime(t),
				})
			}
		}
	}

	// Sort by timestamp (most recent first) and limit to maxRecentEvents
	if len(nodeHistory) > 0 {
		sort.Slice(nodeHistory, func(i, j int) bool {
			return nodeHistory[i].LastRCChange.After(nodeHistory[j].LastRCChange.Time)
		})
		if len(nodeHistory) > maxRecentEvents {
			nodeHistory = nodeHistory[:maxRecentEvents]
			klog.V(4).Infof("Limited node history to %d most recent events", maxRecentEvents)
		}
	}

	// Build fencing history
	fenceCutoffTime := time.Now().Add(-fencingEventTimeWindow)

	for _, fenceEvent := range result.FenceHistory.FenceEvent {
		// Parse the timestamp
		t, err := time.Parse(pacemakerFenceTimeFormat, fenceEvent.Completed)
		if err != nil {
			klog.Warningf("Failed to parse fence event timestamp: %v", err)
			continue
		}

		// Only include recent fencing events
		if !t.After(fenceCutoffTime) {
			continue
		}

		// Convert action to typed enum - only set if it's a valid value
		var action v1alpha1.FencingActionType
		if fenceEvent.Action == string(v1alpha1.FencingActionReboot) ||
			fenceEvent.Action == string(v1alpha1.FencingActionOff) ||
			fenceEvent.Action == string(v1alpha1.FencingActionOn) {
			action = v1alpha1.FencingActionType(fenceEvent.Action)
		} else {
			// Unknown action, skip this event
			klog.V(2).Infof("Skipping fencing event with unknown action: %s", fenceEvent.Action)
			continue
		}

		// Convert status to typed enum - only set if it's a valid value
		var status v1alpha1.FencingStatusType
		if fenceEvent.Status == string(v1alpha1.FencingStatusSuccess) ||
			fenceEvent.Status == string(v1alpha1.FencingStatusFailed) ||
			fenceEvent.Status == string(v1alpha1.FencingStatusPending) {
			status = v1alpha1.FencingStatusType(fenceEvent.Status)
		} else {
			// Unknown status, skip this event
			klog.V(2).Infof("Skipping fencing event with unknown status: %s", fenceEvent.Status)
			continue
		}

		fencingHistory = append(fencingHistory, v1alpha1.PacemakerFencingEvent{
			Target:    fenceEvent.Target,
			Action:    action,
			Status:    status,
			Completed: metav1.NewTime(t),
		})
	}

	// Sort by timestamp (most recent first) and limit to maxRecentEvents
	if len(fencingHistory) > 0 {
		sort.Slice(fencingHistory, func(i, j int) bool {
			return fencingHistory[i].Completed.After(fencingHistory[j].Completed.Time)
		})
		if len(fencingHistory) > maxRecentEvents {
			fencingHistory = fencingHistory[:maxRecentEvents]
			klog.V(4).Infof("Limited fencing history to %d most recent events", maxRecentEvents)
		}
	}

	return summary, nodes, resources, nodeHistory, fencingHistory
}

// updatePacemakerStatusCR updates or creates the PacemakerCluster custom resource
// with the collected status information. The CR is named "cluster" and is cluster-scoped.
// If the CR doesn't exist, it will be created; otherwise, its status subresource is updated.
//
// All data collected from Pacemaker is written to the status as-is. The health check controller
// is responsible for handling potentially missing or empty fields.
func updatePacemakerStatusCR(
	ctx context.Context,
	rawXML string,
	summary *v1alpha1.PacemakerSummary,
	nodes []v1alpha1.PacemakerNodeStatus,
	resources []v1alpha1.PacemakerResourceStatus,
	nodeHistory []v1alpha1.PacemakerNodeHistoryEntry,
	fencingHistory []v1alpha1.PacemakerFencingEvent,
	collectionError string,
) error {
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

	now := metav1.Now()

	// Log what we're about to send to the API
	klog.V(2).Infof("Preparing to update PacemakerCluster CR with %d nodes", len(nodes))
	for i, node := range nodes {
		klog.V(2).Infof("  Node[%d]: Name='%s', IPAddress='%s', OnlineStatus='%s'", i, node.Name, node.IPAddress, node.OnlineStatus)
	}

	if err != nil {
		// Create new Pacemaker if it doesn't exist
		if apierrors.IsNotFound(err) {
			klog.Infof("PacemakerCluster CR not found, creating new one")
			newStatus := &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: PacemakerClusterResourceName,
				},
				Status: &v1alpha1.PacemakerClusterStatus{
					LastUpdated:     now,
					RawXML:          rawXML,
					CollectionError: collectionError,
					Summary:         summary,
					Nodes:           nodes,
					Resources:       resources,
					NodeHistory:     nodeHistory,
					FencingHistory:  fencingHistory,
				},
			}

			result := restClient.Post().
				Resource(pacemakerResourceName).
				Body(newStatus).
				Do(ctx)

			if result.Error() != nil {
				return fmt.Errorf("failed to create PacemakerCluster: %w", result.Error())
			}

			// Ensure status is set on initial create when CRD uses the status subresource
			result = restClient.Put().
				Resource(pacemakerResourceName).
				Name(PacemakerClusterResourceName).
				SubResource(statusSubresource).
				Body(newStatus).
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

	// Update existing Pacemaker CR
	existing.Status.LastUpdated = now
	existing.Status.RawXML = rawXML
	existing.Status.CollectionError = collectionError
	existing.Status.Summary = summary
	existing.Status.Nodes = nodes
	existing.Status.Resources = resources
	existing.Status.NodeHistory = nodeHistory
	existing.Status.FencingHistory = fencingHistory

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
