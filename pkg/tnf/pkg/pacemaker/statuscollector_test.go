package pacemaker

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// =============================================================================
// Test Helpers
// =============================================================================

// loadTestXML loads a test XML file from the testdata directory
func loadTestXML(t *testing.T, filename string) string {
	path := filepath.Join("testdata", filename)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "Failed to read test XML file: %s", filename)
	return string(data)
}

// Helper function to create a test cluster config with two nodes
func createTestClusterConfig() *ClusterConfig {
	return &ClusterConfig{
		ClusterName: "TNF",
		ClusterUUID: "f9c7bea3633c4943ab7d6d245837e427",
		Nodes: []ClusterConfigNode{
			{
				Name:   "master-0",
				NodeID: "1",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "192.168.111.20", Link: "0", Type: "IPv4"},
				},
			},
			{
				Name:   "master-1",
				NodeID: "2",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "192.168.111.21", Link: "0", Type: "IPv4"},
				},
			},
		},
	}
}

// Helper function to create a test cluster config with specific nodes
func createTestClusterConfigWithNodes(nodes []ClusterConfigNode) *ClusterConfig {
	return &ClusterConfig{
		ClusterName: "TNF",
		ClusterUUID: "test-uuid",
		Nodes:       nodes,
	}
}

// Helper function to generate recent failures XML with dynamic timestamps
func getRecentFailuresXML(t *testing.T) string {
	// Load the template from testdata
	templateXML := loadTestXML(t, "recent_failures_template.xml")

	// Generate recent timestamps that are definitely within the 5-minute window
	now := time.Now()
	recentTime := now.Add(-1 * time.Minute).UTC().Format("Mon Jan 2 15:04:05 2006")
	recentTimeISO := now.Add(-1 * time.Minute).UTC().Format("2006-01-02 15:04:05.000000Z")

	// Replace the placeholders in the template
	xmlContent := strings.ReplaceAll(templateXML, "{{RECENT_TIMESTAMP}}", recentTime)
	xmlContent = strings.ReplaceAll(xmlContent, "{{RECENT_TIMESTAMP_ISO}}", recentTimeISO)

	return xmlContent
}

// Note: findConditionInList and findResourceInList are defined in test_fixtures_test.go

// createBasicPacemakerResult creates a basic healthy PacemakerResult with the given nodes.
// This reduces boilerplate in tests that don't need to customize Summary or other fields.
func createBasicPacemakerResult(nodes []Node) *PacemakerResult {
	return &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{Node: nodes},
	}
}

// =============================================================================
// Unit Tests - buildClusterStatus
// =============================================================================

func TestBuildClusterStatus_HealthyCluster(t *testing.T) {
	// Test with healthy cluster XML
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(healthyXML), &result)
	require.NoError(t, err)

	status := buildClusterStatus(&result, createTestClusterConfig())

	// Verify cluster conditions
	require.NotEmpty(t, status.Conditions, "Should have cluster conditions")
	nodeCountCondition := findConditionInList(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionTrue, nodeCountCondition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected, nodeCountCondition.Reason)

	// Verify nodes
	require.Len(t, status.Nodes, 2)
	for _, node := range status.Nodes {
		require.NotEmpty(t, node.Name, "Node name should not be empty")
		require.NotEmpty(t, node.IPAddresses, "Node IPs should not be empty")
		require.NotEmpty(t, node.Conditions, "Node should have conditions")
		require.NotEmpty(t, node.Resources, "Node should have resources")
	}
}

func TestBuildClusterStatus_OfflineNode(t *testing.T) {
	// Test with offline node XML
	offlineXML := loadTestXML(t, "offline_node.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(offlineXML), &result)
	require.NoError(t, err)

	status := buildClusterStatus(&result, createTestClusterConfig())

	// Verify cluster conditions
	nodeCountCondition := findConditionInList(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionTrue, nodeCountCondition.Status) // Still 2 nodes configured

	// Verify nodes
	require.Len(t, status.Nodes, 2)
}

func TestBuildClusterStatus_InsufficientNodes(t *testing.T) {
	// Test with single node cluster config
	singleNodeConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "192.168.111.20", Link: "0", Type: "IPv4"},
			},
		},
	})

	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true", Type: "member"},
			},
		},
	}

	status := buildClusterStatus(result, singleNodeConfig)

	// Verify node count condition is False (insufficient nodes)
	nodeCountCondition := findConditionInList(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionFalse, nodeCountCondition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes, nodeCountCondition.Reason)

	// Verify nodes
	require.Len(t, status.Nodes, 1)
}

func TestBuildClusterStatus_ResourceHealth(t *testing.T) {
	// Test resource health conditions based on Started/Active status
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true", Type: "member"},
				{Name: "master-1", Online: "true", Type: "member"},
			},
		},
		Resources: Resources{
			Clone: []Clone{
				{
					Resource: []Resource{
						// Kubelet healthy on master-0
						{
							ID:            "kubelet-0",
							ResourceAgent: ResourceAgentKubelet,
							Role:          "Started",
							Active:        "true",
							Managed:       "true",
							Node:          NodeRef{Name: "master-0"},
						},
						// Kubelet unhealthy on master-1
						{
							ID:            "kubelet-1",
							ResourceAgent: ResourceAgentKubelet,
							Role:          "Stopped",
							Active:        "false",
							Managed:       "true",
							Node:          NodeRef{Name: "master-1"},
						},
					},
				},
			},
			Resource: []Resource{
				// Etcd healthy on both nodes
				{
					ID:            "etcd-0",
					ResourceAgent: ResourceAgentEtcd,
					Role:          "Started",
					Active:        "true",
					Managed:       "true",
					Node:          NodeRef{Name: "master-0"},
				},
				{
					ID:            "etcd-1",
					ResourceAgent: ResourceAgentEtcd,
					Role:          "Started",
					Active:        "true",
					Managed:       "true",
					Node:          NodeRef{Name: "master-1"},
				},
				// Fencing only healthy on master-0
				{
					ID:            "fence-master-1",
					ResourceAgent: "stonith:fence_redfish",
					Role:          "Started",
					Active:        "true",
					Managed:       "true",
					Node:          NodeRef{Name: "master-0"},
				},
			},
		},
	}

	status := buildClusterStatus(result, createTestClusterConfig())

	require.Len(t, status.Nodes, 2)

	// Find master-0 and master-1
	var master0, master1 *v1alpha1.PacemakerClusterNodeStatus
	for i := range status.Nodes {
		if status.Nodes[i].Name == "master-0" {
			master0 = &status.Nodes[i]
		} else if status.Nodes[i].Name == "master-1" {
			master1 = &status.Nodes[i]
		}
	}

	require.NotNil(t, master0, "Should find master-0")
	require.NotNil(t, master1, "Should find master-1")

	// Master-0 should have healthy kubelet, etcd, and fencing
	kubeletResource := findResourceInList(master0.Resources, v1alpha1.PacemakerClusterResourceNameKubelet)
	require.NotNil(t, kubeletResource)
	kubeletHealthy := findConditionInList(kubeletResource.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, kubeletHealthy)
	require.Equal(t, metav1.ConditionTrue, kubeletHealthy.Status)

	etcdResource := findResourceInList(master0.Resources, v1alpha1.PacemakerClusterResourceNameEtcd)
	require.NotNil(t, etcdResource)
	etcdHealthy := findConditionInList(etcdResource.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, etcdHealthy)
	require.Equal(t, metav1.ConditionTrue, etcdHealthy.Status)

	fencingResource := findResourceInList(master0.Resources, v1alpha1.PacemakerClusterResourceNameFencingAgent)
	require.NotNil(t, fencingResource)
	fencingHealthy := findConditionInList(fencingResource.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, fencingHealthy)
	require.Equal(t, metav1.ConditionTrue, fencingHealthy.Status)

	// Master-1 should have unhealthy kubelet, healthy etcd, unhealthy fencing
	kubeletResource1 := findResourceInList(master1.Resources, v1alpha1.PacemakerClusterResourceNameKubelet)
	require.NotNil(t, kubeletResource1)
	kubeletHealthy1 := findConditionInList(kubeletResource1.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, kubeletHealthy1)
	require.Equal(t, metav1.ConditionFalse, kubeletHealthy1.Status)

	etcdResource1 := findResourceInList(master1.Resources, v1alpha1.PacemakerClusterResourceNameEtcd)
	require.NotNil(t, etcdResource1)
	etcdHealthy1 := findConditionInList(etcdResource1.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, etcdHealthy1)
	require.Equal(t, metav1.ConditionTrue, etcdHealthy1.Status)

	fencingResource1 := findResourceInList(master1.Resources, v1alpha1.PacemakerClusterResourceNameFencingAgent)
	require.NotNil(t, fencingResource1)
	fencingHealthy1 := findConditionInList(fencingResource1.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, fencingHealthy1)
	require.Equal(t, metav1.ConditionFalse, fencingHealthy1.Status)
}

func TestBuildClusterStatus_NodeIPExtraction(t *testing.T) {
	// Test with custom cluster config to verify IPs come from cluster config
	customConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{Name: "master-0", NodeID: "1", Addrs: []ClusterConfigNodeAddr{{Addr: "192.168.1.10", Link: "0", Type: "IPv4"}}},
	})
	result := createBasicPacemakerResult([]Node{{Name: "master-0", Online: "true", Type: "member"}})

	status := buildClusterStatus(result, customConfig)

	require.Len(t, status.Nodes, 1)
	require.Equal(t, "master-0", status.Nodes[0].Name)
	require.Contains(t, status.Nodes[0].IPAddresses, "192.168.1.10", "IP address should come from cluster config")
}

func TestBuildClusterStatus_ClusterConfigPriority(t *testing.T) {
	// Test that cluster config takes priority over XML
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{Name: "master-0", NodeID: "1", Addrs: []ClusterConfigNodeAddr{{Addr: "10.0.0.10", Link: "0", Type: "IPv4"}}},
		{Name: "master-1", NodeID: "2", Addrs: []ClusterConfigNodeAddr{{Addr: "10.0.0.11", Link: "0", Type: "IPv4"}}},
	})
	result := createBasicPacemakerResult([]Node{
		{Name: "master-0", Online: "true", Type: "member"},
		{Name: "master-1", Online: "false", Type: "member"},
	})

	status := buildClusterStatus(result, clusterConfig)

	require.Len(t, status.Nodes, 2, "Should have 2 nodes from cluster config")
	require.Equal(t, "master-0", status.Nodes[0].Name)
	require.Contains(t, status.Nodes[0].IPAddresses, "10.0.0.10")
	require.Equal(t, "master-1", status.Nodes[1].Name)
	require.Contains(t, status.Nodes[1].IPAddresses, "10.0.0.11")
}

func TestBuildClusterStatus_ClusterConfigExtraNodes(t *testing.T) {
	// Test when cluster config has more nodes than XML (e.g., node is down)
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{Name: "master-0", NodeID: "1", Addrs: []ClusterConfigNodeAddr{{Addr: "10.0.0.10", Link: "0", Type: "IPv4"}}},
		{Name: "master-1", NodeID: "2", Addrs: []ClusterConfigNodeAddr{{Addr: "10.0.0.11", Link: "0", Type: "IPv4"}}},
	})
	// master-1 is missing from XML (node is down/unreachable)
	result := createBasicPacemakerResult([]Node{{Name: "master-0", Online: "true", Type: "member"}})

	status := buildClusterStatus(result, clusterConfig)

	require.Len(t, status.Nodes, 2, "Should have 2 nodes from cluster config")
	require.Equal(t, "master-1", status.Nodes[1].Name)
	require.Contains(t, status.Nodes[1].IPAddresses, "10.0.0.11")
}

func TestBuildClusterStatus_ClusterConfigIPv6(t *testing.T) {
	// Test IPv6 addresses in cluster config
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "2001:db8::1", Link: "0", Type: "IPv6"},
			},
		},
		{
			Name:   "master-1",
			NodeID: "2",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "fe80::1", Link: "0", Type: "IPv6"}, // Link-local (should fail validation)
			},
		},
	})

	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true", Type: "member"},
				{Name: "master-1", Online: "true", Type: "member"},
			},
		},
	}

	status := buildClusterStatus(result, clusterConfig)

	require.Len(t, status.Nodes, 1, "Only master-0 should be included (master-1 has invalid IP)")

	// Valid IPv6
	require.Equal(t, "master-0", status.Nodes[0].Name)
	require.Contains(t, status.Nodes[0].IPAddresses, "2001:db8::1", "Valid IPv6 should be preserved")
}

func TestBuildClusterStatus_FallbackModeWithoutClusterConfig(t *testing.T) {
	// Test fallback behavior when cluster config is nil
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true", Standby: "false", Type: "member"},
				{Name: "master-1", Online: "false", Standby: "true", Type: "member"},
			},
		},
	}

	status := buildClusterStatus(result, nil)

	require.Len(t, status.Nodes, 2, "Fallback mode should include all XML nodes")

	// All nodes should have placeholder IPs in fallback mode
	require.Equal(t, "master-0", status.Nodes[0].Name)
	require.Contains(t, status.Nodes[0].IPAddresses, "0.0.0.0", "Fallback mode should use placeholder IP")

	require.Equal(t, "master-1", status.Nodes[1].Name)
	require.Contains(t, status.Nodes[1].IPAddresses, "0.0.0.0", "Fallback mode should use placeholder IP")
}

func TestBuildNodeCountCondition(t *testing.T) {
	now := metav1.Now()

	// Test with expected node count
	condition := buildNodeCountCondition(2, now)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedConditionType, condition.Type)
	require.Equal(t, metav1.ConditionTrue, condition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected, condition.Reason)

	// Test with insufficient nodes
	condition = buildNodeCountCondition(1, now)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedConditionType, condition.Type)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes, condition.Reason)

	// Test with no nodes
	condition = buildNodeCountCondition(0, now)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes, condition.Reason)

	// Test with excessive nodes
	condition = buildNodeCountCondition(3, now)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonExcessiveNodes, condition.Reason)
}

func TestCollectPacemakerStatus_XMLSizeValidation(t *testing.T) {
	// This test verifies that the maxXMLSize constant is reasonable
	require.Equal(t, 10*1024*1024, maxXMLSize, "Max XML size should be 10MB")
}

func TestCollectPacemakerStatus_InvalidXMLHandling(t *testing.T) {
	invalidXML := "<invalid><unclosed>"
	var result PacemakerResult
	err := xml.Unmarshal([]byte(invalidXML), &result)

	// Should fail to unmarshal but not panic
	require.Error(t, err, "Should return error for invalid XML")
}

func TestProcessResourcesForState(t *testing.T) {
	result := &PacemakerResult{
		Resources: Resources{
			Clone: []Clone{
				{
					Resource: []Resource{
						{
							ID:            "kubelet-0",
							ResourceAgent: ResourceAgentKubelet,
							Role:          "Started",
							Active:        "true",
							Managed:       "true",
							Node:          NodeRef{Name: "master-0"},
						},
						{
							ID:            "kubelet-1",
							ResourceAgent: ResourceAgentKubelet,
							Role:          "Started",
							Active:        "true",
							Managed:       "true",
							Node:          NodeRef{Name: "master-1"},
						},
					},
				},
			},
			Resource: []Resource{
				{
					ID:            "etcd-0",
					ResourceAgent: ResourceAgentEtcd,
					Role:          "Started",
					Active:        "true",
					Managed:       "true",
					Node:          NodeRef{Name: "master-0"},
				},
				{
					ID:            "etcd-1",
					ResourceAgent: ResourceAgentEtcd,
					Role:          "Stopped",
					Active:        "false",
					Managed:       "true",
					Node:          NodeRef{Name: "master-1"},
				},
				{
					ID:            "fence-master-1",
					ResourceAgent: "stonith:fence_redfish",
					Role:          "Started",
					Active:        "true",
					Managed:       "true",
					Node:          NodeRef{Name: "master-0"},
				},
			},
		},
	}

	resourceState := make(map[string]*ResourceStatePerNode)
	processResourcesForState(result, resourceState)

	// Check master-0
	require.NotNil(t, resourceState["master-0"])
	require.True(t, resourceState["master-0"].KubeletRunning)
	require.True(t, resourceState["master-0"].EtcdRunning)
	require.True(t, resourceState["master-0"].FencingRunning)

	// Check master-1
	require.NotNil(t, resourceState["master-1"])
	require.True(t, resourceState["master-1"].KubeletRunning)
	require.False(t, resourceState["master-1"].EtcdRunning, "Stopped etcd should not be running")
	require.False(t, resourceState["master-1"].FencingRunning, "No fencing resource on master-1")
}

func TestGetCanonicalIPs(t *testing.T) {
	tests := []struct {
		name        string
		configNode  ClusterConfigNode
		expectedIPs []string
	}{
		{
			name: "valid_ipv4",
			configNode: ClusterConfigNode{
				Name: "master-0",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "192.168.1.10", Link: "0", Type: "IPv4"},
				},
			},
			expectedIPs: []string{"192.168.1.10"},
		},
		{
			name: "valid_ipv6",
			configNode: ClusterConfigNode{
				Name: "master-0",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "2001:db8::1", Link: "0", Type: "IPv6"},
				},
			},
			expectedIPs: []string{"2001:db8::1"},
		},
		{
			name: "no_addresses",
			configNode: ClusterConfigNode{
				Name:  "master-0",
				Addrs: []ClusterConfigNodeAddr{},
			},
			expectedIPs: nil,
		},
		{
			name: "invalid_ip",
			configNode: ClusterConfigNode{
				Name: "master-0",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "not-an-ip", Link: "0", Type: "IPv4"},
				},
			},
			expectedIPs: nil,
		},
		{
			name: "loopback_ip",
			configNode: ClusterConfigNode{
				Name: "master-0",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "127.0.0.1", Link: "0", Type: "IPv4"},
				},
			},
			expectedIPs: nil,
		},
		{
			name: "link_local_ip",
			configNode: ClusterConfigNode{
				Name: "master-0",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "fe80::1", Link: "0", Type: "IPv6"},
				},
			},
			expectedIPs: nil,
		},
		{
			name: "multiple_addresses",
			configNode: ClusterConfigNode{
				Name: "master-0",
				Addrs: []ClusterConfigNodeAddr{
					{Addr: "192.168.1.10", Link: "0", Type: "IPv4"},
					{Addr: "192.168.1.11", Link: "1", Type: "IPv4"},
				},
			},
			expectedIPs: []string{"192.168.1.10", "192.168.1.11"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ips := getCanonicalIPs(tt.configNode)
			require.Equal(t, tt.expectedIPs, ips)
		})
	}
}

func TestBuildResourceConditions(t *testing.T) {
	now := metav1.Now()

	// Test healthy resource
	resource := &Resource{
		ID:            "kubelet-0",
		ResourceAgent: ResourceAgentKubelet,
		Role:          "Started",
		Active:        "true",
		Managed:       "true",
		Blocked:       "false",
		Failed:        "false",
	}
	conditions := buildResourceConditions(resource, true, now)

	require.Len(t, conditions, 8, "Should have 8 resource conditions")

	healthyCondition := findConditionInList(conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, healthyCondition)
	require.Equal(t, metav1.ConditionTrue, healthyCondition.Status)

	// Test unhealthy resource (stopped)
	stoppedResource := &Resource{
		ID:            "kubelet-0",
		ResourceAgent: ResourceAgentKubelet,
		Role:          "Stopped",
		Active:        "false",
		Managed:       "true",
		Blocked:       "false",
		Failed:        "false",
	}
	stoppedConditions := buildResourceConditions(stoppedResource, false, now)

	stoppedHealthy := findConditionInList(stoppedConditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, stoppedHealthy)
	require.Equal(t, metav1.ConditionFalse, stoppedHealthy.Status)

	startedCondition := findConditionInList(stoppedConditions, v1alpha1.ResourceStartedConditionType)
	require.NotNil(t, startedCondition)
	require.Equal(t, metav1.ConditionFalse, startedCondition.Status)
}

func TestBuildNodeConditions(t *testing.T) {
	now := metav1.Now()

	// Test healthy node
	healthyNode := &Node{
		Name:       "master-0",
		Online:     "true",
		Standby:    "false",
		Maintenance: "false",
		Pending:    "false",
		Unclean:    "false",
		Type:       "member",
	}
	state := &ResourceStatePerNode{
		KubeletRunning: true,
		EtcdRunning:    true,
		FencingRunning: true,
	}
	conditions := buildNodeConditions(healthyNode, state, now)

	require.Len(t, conditions, 7, "Should have 7 node conditions")

	healthyCondition := findConditionInList(conditions, v1alpha1.NodeHealthyConditionType)
	require.NotNil(t, healthyCondition)
	require.Equal(t, metav1.ConditionTrue, healthyCondition.Status)

	onlineCondition := findConditionInList(conditions, v1alpha1.NodeOnlineConditionType)
	require.NotNil(t, onlineCondition)
	require.Equal(t, metav1.ConditionTrue, onlineCondition.Status)

	// Test offline node
	offlineNode := &Node{
		Name:       "master-1",
		Online:     "false",
		Standby:    "false",
		Maintenance: "false",
		Pending:    "false",
		Unclean:    "false",
		Type:       "member",
	}
	offlineConditions := buildNodeConditions(offlineNode, nil, now)

	offlineHealthy := findConditionInList(offlineConditions, v1alpha1.NodeHealthyConditionType)
	require.NotNil(t, offlineHealthy)
	require.Equal(t, metav1.ConditionFalse, offlineHealthy.Status)

	offlineOnline := findConditionInList(offlineConditions, v1alpha1.NodeOnlineConditionType)
	require.NotNil(t, offlineOnline)
	require.Equal(t, metav1.ConditionFalse, offlineOnline.Status)
}

func TestBuildClusterConditions(t *testing.T) {
	now := metav1.Now()

	// Create healthy nodes
	healthyNode := v1alpha1.PacemakerClusterNodeStatus{
		Name:        "master-0",
		IPAddresses: []string{"192.168.1.10"},
		Conditions: []metav1.Condition{
			{
				Type:               v1alpha1.NodeHealthyConditionType,
				Status:             metav1.ConditionTrue,
				Reason:             v1alpha1.NodeHealthyReasonHealthy,
				LastTransitionTime: now,
			},
		},
	}
	nodes := []v1alpha1.PacemakerClusterNodeStatus{healthyNode, healthyNode}

	// Test healthy cluster
	conditions := buildClusterConditions(2, false, nodes, now)

	require.Len(t, conditions, 3, "Should have 3 cluster conditions")

	healthyCondition := findConditionInList(conditions, v1alpha1.ClusterHealthyConditionType)
	require.NotNil(t, healthyCondition)
	require.Equal(t, metav1.ConditionTrue, healthyCondition.Status)

	inServiceCondition := findConditionInList(conditions, v1alpha1.ClusterInServiceConditionType)
	require.NotNil(t, inServiceCondition)
	require.Equal(t, metav1.ConditionTrue, inServiceCondition.Status)

	nodeCountCondition := findConditionInList(conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionTrue, nodeCountCondition.Status)

	// Test cluster in maintenance
	maintenanceConditions := buildClusterConditions(2, true, nodes, now)

	maintenanceInService := findConditionInList(maintenanceConditions, v1alpha1.ClusterInServiceConditionType)
	require.NotNil(t, maintenanceInService)
	require.Equal(t, metav1.ConditionFalse, maintenanceInService.Status)
}
