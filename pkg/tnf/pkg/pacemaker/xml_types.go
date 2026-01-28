package pacemaker

import "encoding/xml"

// XML structures for parsing "pcs status xml" output.
// These map directly to pacemaker's XML schema.

// PacemakerResult is the root element of pcs status xml output.
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
	ClusterOptions      ClusterOptions      `xml:"cluster_options"`
}

type ClusterOptions struct {
	MaintenanceMode string `xml:"maintenance-mode,attr"`
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

// Node represents a pacemaker cluster node with its operational state.
// Boolean attributes are strings ("true"/"false") in pacemaker XML.
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
	Type             string `xml:"type,attr"` // "member" or "remote"
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

// Resource represents a pacemaker-managed resource (systemd service, container, etc).
type Resource struct {
	ID             string  `xml:"id,attr"`
	ResourceAgent  string  `xml:"resource_agent,attr"` // e.g., "systemd:kubelet", "stonith:fence_redfish"
	Role           string  `xml:"role,attr"`           // "Started", "Stopped", etc.
	TargetRole     string  `xml:"target_role,attr"`
	Active         string  `xml:"active,attr"`
	Orphaned       string  `xml:"orphaned,attr"`
	Blocked        string  `xml:"blocked,attr"`
	Maintenance    string  `xml:"maintenance,attr"` // "true" when resource is on a node in maintenance mode
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

// OperationHistory tracks resource operation results for failure detection.
type OperationHistory struct {
	Call         string `xml:"call,attr"`
	Task         string `xml:"task,attr"` // "start", "stop", "monitor", etc.
	RC           string `xml:"rc,attr"`   // Return code: "0" = success
	RCText       string `xml:"rc_text,attr"`
	ExitReason   string `xml:"exit-reason,attr"`
	LastRCChange string `xml:"last-rc-change,attr"` // Timestamp: "Mon Jan 2 15:04:05 2006"
	ExecTime     string `xml:"exec-time,attr"`
	QueueTime    string `xml:"queue-time,attr"`
}

type FenceHistory struct {
	FenceEvent []FenceEvent `xml:"fence_event"`
}

// FenceEvent records a fencing operation (node isolation/reboot).
type FenceEvent struct {
	Target     string `xml:"target,attr"`
	Action     string `xml:"action,attr"` // "reboot", "off", etc.
	Delegate   string `xml:"delegate,attr"`
	Client     string `xml:"client,attr"`
	Origin     string `xml:"origin,attr"`
	Status     string `xml:"status,attr"`      // "success", "failed", etc.
	ExitReason string `xml:"exit-reason,attr"` // Populated on failure
	Completed  string `xml:"completed,attr"`   // Timestamp: "2006-01-02 15:04:05.000000Z"
}

// JSON structures for parsing "pcs cluster config show --output-format json".
// This provides the authoritative node list with IP addresses.

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
	Type string `json:"type"` // "IPv4", "IPv6"
}
