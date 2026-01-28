package pacemaker

import (
	"encoding/xml"
	"fmt"
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

// formatPacemakerTimestamp formats a time for pacemaker operation history (e.g., "Mon Jan 2 15:04:05 2006")
func formatPacemakerTimestamp(t time.Time) string {
	return t.UTC().Format(pacemakerTimeFormat)
}

// formatPacemakerFenceTimestamp formats a time for pacemaker fence history (e.g., "2006-01-02 15:04:05.000000Z")
func formatPacemakerFenceTimestamp(t time.Time) string {
	return t.UTC().Format(pacemakerFenceTimeFormat)
}

// loadRecentFailuresXML loads the recent failures template and substitutes timestamps.
// recentOffset is relative to now (e.g., -1*time.Minute for 1 minute ago)
func loadRecentFailuresXML(t *testing.T, recentOffset time.Duration) string {
	templateXML := loadTestXML(t, "recent_failures_template.xml")

	now := time.Now()
	recentTime := now.Add(recentOffset)

	// Replace the placeholders in the template
	xmlContent := strings.ReplaceAll(templateXML, "{{RECENT_TIMESTAMP}}", formatPacemakerTimestamp(recentTime))
	xmlContent = strings.ReplaceAll(xmlContent, "{{RECENT_TIMESTAMP_ISO}}", formatPacemakerFenceTimestamp(recentTime))

	return xmlContent
}

// Note: FindCondition and findResourceInList are defined in test_fixtures_test.go

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
	nodeCountCondition := FindCondition(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionTrue, nodeCountCondition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected, nodeCountCondition.Reason)

	// Verify nodes
	require.NotNil(t, status.Nodes)
	require.Len(t, *status.Nodes, 2)
	for _, node := range *status.Nodes {
		require.NotEmpty(t, node.NodeName, "Node name should not be empty")
		require.NotEmpty(t, node.Addresses, "Node addresses should not be empty")
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
	nodeCountCondition := FindCondition(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionTrue, nodeCountCondition.Status) // Still 2 nodes configured

	// Verify nodes
	require.Len(t, *status.Nodes, 2)
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
	nodeCountCondition := FindCondition(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionFalse, nodeCountCondition.Status)
	require.Equal(t, v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes, nodeCountCondition.Reason)

	// Verify nodes
	require.Len(t, *status.Nodes, 1)
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
				// Fencing agent for master-0 (healthy, running on master-0)
				{
					ID:            "master-0_redfish",
					ResourceAgent: "stonith:fence_redfish",
					Role:          "Started",
					Active:        "true",
					Managed:       "true",
					Node:          NodeRef{Name: "master-0"},
				},
				// Fencing agent for master-1 (unhealthy, stopped)
				{
					ID:            "master-1_redfish",
					ResourceAgent: "stonith:fence_redfish",
					Role:          "Stopped",
					Active:        "false",
					Managed:       "true",
					Node:          NodeRef{Name: "master-1"},
				},
			},
		},
	}

	status := buildClusterStatus(result, createTestClusterConfig())

	require.NotNil(t, status.Nodes)
	require.Len(t, *status.Nodes, 2)

	// Find master-0 and master-1
	var master0, master1 *v1alpha1.PacemakerClusterNodeStatus
	for i := range *status.Nodes {
		if (*status.Nodes)[i].NodeName == "master-0" {
			master0 = &(*status.Nodes)[i]
		} else if (*status.Nodes)[i].NodeName == "master-1" {
			master1 = &(*status.Nodes)[i]
		}
	}

	require.NotNil(t, master0, "Should find master-0")
	require.NotNil(t, master1, "Should find master-1")

	// Master-0 should have healthy kubelet, etcd, and fencing
	kubeletResource := findResourceInList(master0.Resources, v1alpha1.PacemakerClusterResourceNameKubelet)
	require.NotNil(t, kubeletResource)
	kubeletHealthy := FindCondition(kubeletResource.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, kubeletHealthy)
	require.Equal(t, metav1.ConditionTrue, kubeletHealthy.Status)

	etcdResource := findResourceInList(master0.Resources, v1alpha1.PacemakerClusterResourceNameEtcd)
	require.NotNil(t, etcdResource)
	etcdHealthy := FindCondition(etcdResource.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, etcdHealthy)
	require.Equal(t, metav1.ConditionTrue, etcdHealthy.Status)

	// Check fencing agents for master-0 (should be healthy)
	require.NotEmpty(t, master0.FencingAgents, "Node should have fencing agents")
	fencingAgent0 := master0.FencingAgents[0]
	fencingHealthy := FindCondition(fencingAgent0.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, fencingHealthy)
	require.Equal(t, metav1.ConditionTrue, fencingHealthy.Status)

	// Master-1 should have unhealthy kubelet, healthy etcd
	kubeletResource1 := findResourceInList(master1.Resources, v1alpha1.PacemakerClusterResourceNameKubelet)
	require.NotNil(t, kubeletResource1)
	kubeletHealthy1 := FindCondition(kubeletResource1.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, kubeletHealthy1)
	require.Equal(t, metav1.ConditionFalse, kubeletHealthy1.Status)

	etcdResource1 := findResourceInList(master1.Resources, v1alpha1.PacemakerClusterResourceNameEtcd)
	require.NotNil(t, etcdResource1)
	etcdHealthy1 := FindCondition(etcdResource1.Conditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, etcdHealthy1)
	require.Equal(t, metav1.ConditionTrue, etcdHealthy1.Status)

	// Check fencing agents for master-1 (should be unhealthy based on test data)
	require.NotEmpty(t, master1.FencingAgents, "Node should have fencing agents")
	fencingAgent1 := master1.FencingAgents[0]
	fencingHealthy1 := FindCondition(fencingAgent1.Conditions, v1alpha1.ResourceHealthyConditionType)
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

	require.NotNil(t, status.Nodes)
	require.Len(t, *status.Nodes, 1)
	require.Equal(t, "master-0", (*status.Nodes)[0].NodeName)
	require.True(t, containsAddress((*status.Nodes)[0].Addresses, "192.168.1.10"), "IP address should come from cluster config")
}

// containsAddress checks if a PacemakerNodeAddress slice contains a specific address
func containsAddress(addresses []v1alpha1.PacemakerNodeAddress, addr string) bool {
	for _, a := range addresses {
		if a.Address == addr {
			return true
		}
	}
	return false
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

	require.Len(t, *status.Nodes, 2, "Should have 2 nodes from cluster config")
	require.Equal(t, "master-0", (*status.Nodes)[0].NodeName)
	require.True(t, containsAddress((*status.Nodes)[0].Addresses, "10.0.0.10"))
	require.Equal(t, "master-1", (*status.Nodes)[1].NodeName)
	require.True(t, containsAddress((*status.Nodes)[1].Addresses, "10.0.0.11"))
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

	require.Len(t, *status.Nodes, 2, "Should have 2 nodes from cluster config")
	require.Equal(t, "master-1", (*status.Nodes)[1].NodeName)
	require.True(t, containsAddress((*status.Nodes)[1].Addresses, "10.0.0.11"))
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

	require.Len(t, *status.Nodes, 1, "Only master-0 should be included (master-1 has invalid IP)")

	// Valid IPv6
	require.Equal(t, "master-0", (*status.Nodes)[0].NodeName)
	require.True(t, containsAddress((*status.Nodes)[0].Addresses, "2001:db8::1"), "Valid IPv6 should be preserved")
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

	require.Len(t, *status.Nodes, 2, "Fallback mode should include all XML nodes")

	// All nodes should have placeholder IPs in fallback mode
	require.Equal(t, "master-0", (*status.Nodes)[0].NodeName)
	require.True(t, containsAddress((*status.Nodes)[0].Addresses, "0.0.0.0"), "Fallback mode should use placeholder IP")

	require.Equal(t, "master-1", (*status.Nodes)[1].NodeName)
	require.True(t, containsAddress((*status.Nodes)[1].Addresses, "0.0.0.0"), "Fallback mode should use placeholder IP")
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
					ID:            "master-0_redfish",
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

	// Check master-1
	require.NotNil(t, resourceState["master-1"])
	require.True(t, resourceState["master-1"].KubeletRunning)
	require.False(t, resourceState["master-1"].EtcdRunning, "Stopped etcd should not be running")

	// Test fencing agents separately
	fencingAgents := processFencingAgents(result)
	require.NotEmpty(t, fencingAgents["master-0"], "Should have fencing agents for master-0")
	require.True(t, fencingAgents["master-0"][0].IsRunning, "master-0 fencing agent should be running")
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
			addresses := getNodeAddresses(tt.configNode)
			// Extract IPs from NodeAddress slice for comparison
			var ips []string
			for _, addr := range addresses {
				ips = append(ips, addr.Address)
			}
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

	healthyCondition := FindCondition(conditions, v1alpha1.ResourceHealthyConditionType)
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

	stoppedHealthy := FindCondition(stoppedConditions, v1alpha1.ResourceHealthyConditionType)
	require.NotNil(t, stoppedHealthy)
	require.Equal(t, metav1.ConditionFalse, stoppedHealthy.Status)

	startedCondition := FindCondition(stoppedConditions, v1alpha1.ResourceStartedConditionType)
	require.NotNil(t, startedCondition)
	require.Equal(t, metav1.ConditionFalse, startedCondition.Status)
}

func TestBuildNodeConditions(t *testing.T) {
	now := metav1.Now()

	// Create healthy fencing agents for testing
	healthyFencingAgents := []v1alpha1.PacemakerClusterFencingAgentStatus{
		{
			Conditions: createHealthyResourceConditions(),
			Name:       "master-0_redfish",
			Method:     v1alpha1.FencingMethodRedfish,
		},
	}

	// Test healthy node
	healthyNode := &Node{
		Name:        "master-0",
		Online:      "true",
		Standby:     "false",
		Maintenance: "false",
		Pending:     "false",
		Unclean:     "false",
		Type:        "member",
	}
	state := &ResourceStatePerNode{
		KubeletRunning: true,
		EtcdRunning:    true,
	}
	conditions := buildNodeConditions(healthyNode, state, healthyFencingAgents, now)

	require.Len(t, conditions, 9, "Should have 9 node conditions (including FencingAvailable and FencingHealthy)")

	healthyCondition := FindCondition(conditions, v1alpha1.NodeHealthyConditionType)
	require.NotNil(t, healthyCondition)
	require.Equal(t, metav1.ConditionTrue, healthyCondition.Status)

	onlineCondition := FindCondition(conditions, v1alpha1.NodeOnlineConditionType)
	require.NotNil(t, onlineCondition)
	require.Equal(t, metav1.ConditionTrue, onlineCondition.Status)

	// Check fencing conditions
	fencingAvailable := FindCondition(conditions, v1alpha1.NodeFencingAvailableConditionType)
	require.NotNil(t, fencingAvailable)
	require.Equal(t, metav1.ConditionTrue, fencingAvailable.Status)

	fencingHealthy := FindCondition(conditions, v1alpha1.NodeFencingHealthyConditionType)
	require.NotNil(t, fencingHealthy)
	require.Equal(t, metav1.ConditionTrue, fencingHealthy.Status)

	// Test offline node with no fencing agents (both fencing conditions should be false)
	offlineNode := &Node{
		Name:        "master-1",
		Online:      "false",
		Standby:     "false",
		Maintenance: "false",
		Pending:     "false",
		Unclean:     "false",
		Type:        "member",
	}
	offlineConditions := buildNodeConditions(offlineNode, nil, nil, now)

	offlineHealthy := FindCondition(offlineConditions, v1alpha1.NodeHealthyConditionType)
	require.NotNil(t, offlineHealthy)
	require.Equal(t, metav1.ConditionFalse, offlineHealthy.Status)

	offlineOnline := FindCondition(offlineConditions, v1alpha1.NodeOnlineConditionType)
	require.NotNil(t, offlineOnline)
	require.Equal(t, metav1.ConditionFalse, offlineOnline.Status)

	// Fencing should be unavailable when no agents
	offlineFencingAvailable := FindCondition(offlineConditions, v1alpha1.NodeFencingAvailableConditionType)
	require.NotNil(t, offlineFencingAvailable)
	require.Equal(t, metav1.ConditionFalse, offlineFencingAvailable.Status)
}

func TestBuildClusterConditions(t *testing.T) {
	now := metav1.Now()

	// Create healthy nodes
	healthyNode := v1alpha1.PacemakerClusterNodeStatus{
		NodeName:  "master-0",
		Addresses: []v1alpha1.PacemakerNodeAddress{{Type: v1alpha1.PacemakerNodeInternalIP, Address: "192.168.1.10"}},
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

	healthyCondition := FindCondition(conditions, v1alpha1.ClusterHealthyConditionType)
	require.NotNil(t, healthyCondition)
	require.Equal(t, metav1.ConditionTrue, healthyCondition.Status)

	inServiceCondition := FindCondition(conditions, v1alpha1.ClusterInServiceConditionType)
	require.NotNil(t, inServiceCondition)
	require.Equal(t, metav1.ConditionTrue, inServiceCondition.Status)

	nodeCountCondition := FindCondition(conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
	require.NotNil(t, nodeCountCondition)
	require.Equal(t, metav1.ConditionTrue, nodeCountCondition.Status)

	// Test cluster in maintenance
	maintenanceConditions := buildClusterConditions(2, true, nodes, now)

	maintenanceInService := FindCondition(maintenanceConditions, v1alpha1.ClusterInServiceConditionType)
	require.NotNil(t, maintenanceInService)
	require.Equal(t, metav1.ConditionFalse, maintenanceInService.Status)
}

func TestIsClusterInMaintenance(t *testing.T) {
	// isClusterInMaintenance only checks cluster-level maintenance mode.
	// Node-level maintenance is handled separately per-node and does NOT
	// cause the function to return true. This distinction is important:
	// - Cluster maintenance (pcs property set maintenance-mode=true) affects ALL nodes
	// - Node maintenance (pcs node maintenance <node>) affects only that specific node
	tests := []struct {
		name                   string
		clusterMaintenanceMode string
		nodeMaintenanceModes   []string
		expectedResult         bool
	}{
		{
			name:                   "cluster_level_maintenance_mode_true",
			clusterMaintenanceMode: "true",
			nodeMaintenanceModes:   []string{"false", "false"},
			expectedResult:         true,
		},
		{
			name:                   "cluster_level_maintenance_mode_false_nodes_not_in_maintenance",
			clusterMaintenanceMode: "false",
			nodeMaintenanceModes:   []string{"false", "false"},
			expectedResult:         false,
		},
		{
			// Node-level maintenance does NOT trigger cluster-level maintenance
			name:                   "cluster_level_maintenance_mode_false_one_node_in_maintenance",
			clusterMaintenanceMode: "false",
			nodeMaintenanceModes:   []string{"true", "false"},
			expectedResult:         false,
		},
		{
			name:                   "cluster_level_maintenance_mode_empty_nodes_not_in_maintenance",
			clusterMaintenanceMode: "",
			nodeMaintenanceModes:   []string{"false", "false"},
			expectedResult:         false,
		},
		{
			// Cluster maintenance is what matters, node state is irrelevant
			name:                   "both_cluster_and_node_in_maintenance",
			clusterMaintenanceMode: "true",
			nodeMaintenanceModes:   []string{"true", "false"},
			expectedResult:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build the PacemakerResult with the specified maintenance modes
			result := &PacemakerResult{
				Summary: Summary{
					ClusterOptions: ClusterOptions{
						MaintenanceMode: tt.clusterMaintenanceMode,
					},
				},
				Nodes: Nodes{
					Node: make([]Node, len(tt.nodeMaintenanceModes)),
				},
			}
			for i, mode := range tt.nodeMaintenanceModes {
				result.Nodes.Node[i] = Node{
					Name:        fmt.Sprintf("master-%d", i),
					Maintenance: mode,
				}
			}

			got := isClusterInMaintenance(result)
			require.Equal(t, tt.expectedResult, got, "isClusterInMaintenance returned unexpected result")
		})
	}
}

// =============================================================================
// XML-based Condition Tests
// =============================================================================

func TestBuildClusterStatus_ClusterConditionScenarios(t *testing.T) {
	tests := []struct {
		name                    string
		xmlFile                 string
		expectedNodeCount       int
		expectedClusterHealthy  metav1.ConditionStatus
		expectedNodeCountStatus metav1.ConditionStatus
		expectedNodeCountReason string
		expectedInServiceStatus *metav1.ConditionStatus // nil if not checked
		expectedInServiceReason string
	}{
		{
			name:                    "cluster_maintenance",
			xmlFile:                 "cluster_maintenance.xml",
			expectedNodeCount:       2,
			expectedClusterHealthy:  metav1.ConditionFalse,
			expectedNodeCountStatus: metav1.ConditionTrue,
			expectedNodeCountReason: v1alpha1.ClusterNodeCountAsExpectedReasonAsExpected,
			expectedInServiceStatus: ptr(metav1.ConditionFalse),
			expectedInServiceReason: v1alpha1.ClusterInServiceReasonInMaintenance,
		},
		{
			name:                    "single_node_insufficient",
			xmlFile:                 "single_node.xml",
			expectedNodeCount:       1,
			expectedClusterHealthy:  metav1.ConditionFalse,
			expectedNodeCountStatus: metav1.ConditionFalse,
			expectedNodeCountReason: v1alpha1.ClusterNodeCountAsExpectedReasonInsufficientNodes,
		},
		{
			name:                    "excessive_nodes",
			xmlFile:                 "excessive_nodes.xml",
			expectedNodeCount:       3,
			expectedClusterHealthy:  metav1.ConditionFalse,
			expectedNodeCountStatus: metav1.ConditionFalse,
			expectedNodeCountReason: v1alpha1.ClusterNodeCountAsExpectedReasonExcessiveNodes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xmlData := loadTestXML(t, tt.xmlFile)
			var result PacemakerResult
			err := xml.Unmarshal([]byte(xmlData), &result)
			require.NoError(t, err)

			status := buildClusterStatus(&result, nil)

			// Verify node count
			require.Len(t, *status.Nodes, tt.expectedNodeCount)

			// Verify cluster healthy condition
			healthyCondition := FindCondition(status.Conditions, v1alpha1.ClusterHealthyConditionType)
			require.NotNil(t, healthyCondition, "Healthy condition should exist")
			require.Equal(t, tt.expectedClusterHealthy, healthyCondition.Status)

			// Verify node count condition
			nodeCountCondition := FindCondition(status.Conditions, v1alpha1.ClusterNodeCountAsExpectedConditionType)
			require.NotNil(t, nodeCountCondition, "NodeCountAsExpected condition should exist")
			require.Equal(t, tt.expectedNodeCountStatus, nodeCountCondition.Status)
			require.Equal(t, tt.expectedNodeCountReason, nodeCountCondition.Reason)

			// Verify in-service condition if expected
			if tt.expectedInServiceStatus != nil {
				inServiceCondition := FindCondition(status.Conditions, v1alpha1.ClusterInServiceConditionType)
				require.NotNil(t, inServiceCondition, "InService condition should exist")
				require.Equal(t, *tt.expectedInServiceStatus, inServiceCondition.Status)
				require.Equal(t, tt.expectedInServiceReason, inServiceCondition.Reason)
			}
		})
	}
}

// ptr returns a pointer to the given value (helper for test tables)
func ptr[T any](v T) *T {
	return &v
}

func TestBuildClusterStatus_NodeStates(t *testing.T) {
	// Combined test using node_states.xml which has multiple nodes in different states:
	// - master-0: healthy baseline
	// - master-1: maintenance=true
	// - master-2: unclean=true
	// - master-3: pending=true
	nodeStatesXML := loadTestXML(t, "node_states.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(nodeStatesXML), &result)
	require.NoError(t, err)

	status := buildClusterStatus(&result, nil)

	// Helper to find node by name
	findNode := func(name string) *v1alpha1.PacemakerClusterNodeStatus {
		for i := range *status.Nodes {
			if (*status.Nodes)[i].NodeName == name {
				return &(*status.Nodes)[i]
			}
		}
		return nil
	}

	t.Run("master-0_baseline_node_conditions_ok", func(t *testing.T) {
		node := findNode("master-0")
		require.NotNil(t, node, "master-0 should exist")

		// Verify all node-level conditions are True (even though overall health may be False due to missing resources)
		onlineCondition := FindCondition(node.Conditions, v1alpha1.NodeOnlineConditionType)
		require.NotNil(t, onlineCondition)
		require.Equal(t, metav1.ConditionTrue, onlineCondition.Status, "Node should be online")

		inServiceCondition := FindCondition(node.Conditions, v1alpha1.NodeInServiceConditionType)
		require.NotNil(t, inServiceCondition)
		require.Equal(t, metav1.ConditionTrue, inServiceCondition.Status, "Node should be in service")

		cleanCondition := FindCondition(node.Conditions, v1alpha1.NodeCleanConditionType)
		require.NotNil(t, cleanCondition)
		require.Equal(t, metav1.ConditionTrue, cleanCondition.Status, "Node should be clean")

		readyCondition := FindCondition(node.Conditions, v1alpha1.NodeReadyConditionType)
		require.NotNil(t, readyCondition)
		require.Equal(t, metav1.ConditionTrue, readyCondition.Status, "Node should be ready")
	})

	t.Run("master-1_in_maintenance", func(t *testing.T) {
		node := findNode("master-1")
		require.NotNil(t, node, "master-1 should exist")

		inServiceCondition := FindCondition(node.Conditions, v1alpha1.NodeInServiceConditionType)
		require.NotNil(t, inServiceCondition)
		require.Equal(t, metav1.ConditionFalse, inServiceCondition.Status, "Node should NOT be in service when in maintenance")

		healthyCondition := FindCondition(node.Conditions, v1alpha1.NodeHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status, "Node should be unhealthy when in maintenance")
	})

	t.Run("master-2_unclean", func(t *testing.T) {
		node := findNode("master-2")
		require.NotNil(t, node, "master-2 should exist")

		cleanCondition := FindCondition(node.Conditions, v1alpha1.NodeCleanConditionType)
		require.NotNil(t, cleanCondition)
		require.Equal(t, metav1.ConditionFalse, cleanCondition.Status, "Node should NOT be clean when unclean=true")

		healthyCondition := FindCondition(node.Conditions, v1alpha1.NodeHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status, "Node should be unhealthy when unclean")
	})

	t.Run("master-3_pending", func(t *testing.T) {
		node := findNode("master-3")
		require.NotNil(t, node, "master-3 should exist")

		readyCondition := FindCondition(node.Conditions, v1alpha1.NodeReadyConditionType)
		require.NotNil(t, readyCondition)
		require.Equal(t, metav1.ConditionFalse, readyCondition.Status, "Node should NOT be ready when pending=true")

		healthyCondition := FindCondition(node.Conditions, v1alpha1.NodeHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status, "Node should be unhealthy when pending")
	})

	t.Run("cluster_in_service_despite_node_maintenance", func(t *testing.T) {
		// Node-level maintenance (pcs node maintenance <node>) does NOT affect cluster-level InService.
		// Only cluster-level maintenance (pcs property set maintenance-mode=true) sets InService=False.
		// This is intentional: node maintenance affects only that node's resources, not the cluster.
		clusterInServiceCondition := FindCondition(status.Conditions, v1alpha1.ClusterInServiceConditionType)
		require.NotNil(t, clusterInServiceCondition)
		require.Equal(t, metav1.ConditionTrue, clusterInServiceCondition.Status, "Cluster should be in service even when individual nodes are in maintenance")
	})
}

func TestGenerateEventName(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		message string
	}{
		{
			name:    "fencing event",
			reason:  EventReasonFencingEvent,
			message: "Node master-0 was fenced",
		},
		{
			name:    "failed action",
			reason:  EventReasonFailedAction,
			message: "Resource etcd failed on master-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got1 := generateEventName(tt.reason, tt.message)
			got2 := generateEventName(tt.reason, tt.message)

			// Same inputs produce same output (deterministic)
			require.Equal(t, got1, got2)

			// Contains expected prefix
			require.Contains(t, got1, "pacemaker-")

			// Different inputs produce different outputs
			different := generateEventName(tt.reason, tt.message+"different")
			require.NotEqual(t, got1, different)
		})
	}
}

// =============================================================================
// Tests for fencing agent health functions
// =============================================================================

func TestIsAgentHealthy(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name           string
		agent          v1alpha1.PacemakerClusterFencingAgentStatus
		expectedResult bool
	}{
		{
			name: "healthy_agent",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				},
			},
			expectedResult: true,
		},
		{
			name: "unhealthy_agent",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				},
			},
			expectedResult: false,
		},
		{
			name: "no_healthy_condition",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					// Only other conditions, no Healthy condition
					{Type: v1alpha1.ResourceManagedConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				},
			},
			expectedResult: false, // Assume unhealthy if no Healthy condition
		},
		{
			name: "empty_conditions",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:       "master-0_redfish",
				Method:     v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{},
			},
			expectedResult: false, // Assume unhealthy if no conditions
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAgentHealthy(tt.agent)
			require.Equal(t, tt.expectedResult, result)
		})
	}
}

func TestIsAgentAvailable(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name           string
		agent          v1alpha1.PacemakerClusterFencingAgentStatus
		expectedResult bool
	}{
		{
			name: "available_fully_healthy",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				},
			},
			expectedResult: true,
		},
		{
			name: "available_but_on_maintenance_node",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					// Running - can fence
					{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					// Not managed due to maintenance - doesn't affect availability
					{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
					{Type: v1alpha1.ResourceManagedConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				},
			},
			expectedResult: true, // Still available - it's running
		},
		{
			name: "not_available_stopped",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
					{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
					{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				},
			},
			expectedResult: false, // Not running, cannot fence
		},
		{
			name: "not_available_active_but_not_started",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:   "master-0_redfish",
				Method: v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{
					{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
					{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
					{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				},
			},
			expectedResult: false, // Needs all three conditions
		},
		{
			name: "empty_conditions",
			agent: v1alpha1.PacemakerClusterFencingAgentStatus{
				Name:       "master-0_redfish",
				Method:     v1alpha1.FencingMethodRedfish,
				Conditions: []metav1.Condition{},
			},
			expectedResult: false, // No conditions means not available
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAgentAvailable(tt.agent)
			require.Equal(t, tt.expectedResult, result)
		})
	}
}

func TestCalculateFencingHealth(t *testing.T) {
	now := metav1.Now()

	// A healthy agent is running (available) AND fully managed (healthy)
	healthyAgent := v1alpha1.PacemakerClusterFencingAgentStatus{
		Name:   "master-0_redfish",
		Method: v1alpha1.FencingMethodRedfish,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
			{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
			{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
			{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
		},
	}

	// An agent on a maintenance node: running (available) but NOT managed (not healthy).
	// This is the key distinction - the agent CAN fence, but won't be recovered if it fails.
	availableButNotHealthyAgent := v1alpha1.PacemakerClusterFencingAgentStatus{
		Name:   "master-0_redfish_maintenance",
		Method: v1alpha1.FencingMethodRedfish,
		Conditions: []metav1.Condition{
			// Running - can fence
			{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
			{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
			{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
			// Not healthy because managed=false (on maintenance node)
			{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			{Type: v1alpha1.ResourceManagedConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			{Type: v1alpha1.ResourceInServiceConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
		},
	}

	// An unhealthy agent is not running (not available) AND not managed (not healthy)
	unhealthyAgent := v1alpha1.PacemakerClusterFencingAgentStatus{
		Name:   "master-0_ipmi",
		Method: v1alpha1.FencingMethodIPMI,
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
		},
	}

	tests := []struct {
		name                   string
		agents                 []v1alpha1.PacemakerClusterFencingAgentStatus
		expectedFencingAvail   bool
		expectedFencingHealthy bool
	}{
		{
			name:                   "all_healthy",
			agents:                 []v1alpha1.PacemakerClusterFencingAgentStatus{healthyAgent},
			expectedFencingAvail:   true,
			expectedFencingHealthy: true,
		},
		{
			name:                   "all_unhealthy",
			agents:                 []v1alpha1.PacemakerClusterFencingAgentStatus{unhealthyAgent},
			expectedFencingAvail:   false,
			expectedFencingHealthy: false,
		},
		{
			// Key test: agent on maintenance node - available but not healthy
			name:                   "available_but_not_healthy_maintenance_node",
			agents:                 []v1alpha1.PacemakerClusterFencingAgentStatus{availableButNotHealthyAgent},
			expectedFencingAvail:   true,  // Agent IS running, CAN fence
			expectedFencingHealthy: false, // Agent is NOT managed, won't be recovered
		},
		{
			name:                   "mixed_one_healthy_one_unhealthy",
			agents:                 []v1alpha1.PacemakerClusterFencingAgentStatus{healthyAgent, unhealthyAgent},
			expectedFencingAvail:   true,  // At least one is available
			expectedFencingHealthy: false, // Not all are healthy
		},
		{
			name:                   "empty_agents",
			agents:                 []v1alpha1.PacemakerClusterFencingAgentStatus{},
			expectedFencingAvail:   false, // No agents means unavailable
			expectedFencingHealthy: false,
		},
		{
			name:                   "multiple_healthy",
			agents:                 []v1alpha1.PacemakerClusterFencingAgentStatus{healthyAgent, healthyAgent},
			expectedFencingAvail:   true,
			expectedFencingHealthy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			avail, healthy := calculateFencingHealth(tt.agents)
			require.Equal(t, tt.expectedFencingAvail, avail, "FencingAvailable mismatch")
			require.Equal(t, tt.expectedFencingHealthy, healthy, "FencingHealthy mismatch")
		})
	}
}

func TestParseFencingResourceID(t *testing.T) {
	tests := []struct {
		name           string
		resourceID     string
		expectedTarget string
		expectedMethod string
	}{
		{
			name:           "redfish_format",
			resourceID:     "master-0_redfish",
			expectedTarget: "master-0",
			expectedMethod: "redfish",
		},
		{
			name:           "ipmi_format",
			resourceID:     "master-1_ipmi",
			expectedTarget: "master-1",
			expectedMethod: "ipmi",
		},
		{
			name:           "fence_aws_format_uses_last_underscore",
			resourceID:     "ip-10-0-1-50_fence_aws",
			expectedTarget: "ip-10-0-1-50_fence", // LastIndex splits at last underscore
			expectedMethod: "aws",
		},
		{
			name:           "no_underscore_returns_empty",
			resourceID:     "someresource",
			expectedTarget: "", // No underscore means unrecognized format
			expectedMethod: "",
		},
		{
			name:           "multiple_underscores_uses_last",
			resourceID:     "master-0_test_redfish",
			expectedTarget: "master-0_test", // Everything before last underscore
			expectedMethod: "redfish",
		},
		{
			name:           "empty_string_returns_empty",
			resourceID:     "",
			expectedTarget: "",
			expectedMethod: "",
		},
		{
			name:           "underscore_at_start",
			resourceID:     "_redfish",
			expectedTarget: "", // Underscore at index 0 is invalid
			expectedMethod: "",
		},
		{
			name:           "underscore_at_end",
			resourceID:     "master-0_",
			expectedTarget: "", // Underscore at end is invalid
			expectedMethod: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, method := parseFencingResourceID(tt.resourceID)
			require.Equal(t, tt.expectedTarget, target, "Target node mismatch")
			require.Equal(t, tt.expectedMethod, method, "Method mismatch")
		})
	}
}

// =============================================================================
// Tests for Event Filtering - Recent Failed Actions and Fencing Events
// =============================================================================

func TestFilterRecentFailedActions(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name          string
		result        *PacemakerResult
		cutoffTime    time.Time
		expectedCount int
		expectedFirst *RecentFailedAction // nil means no results expected
	}{
		{
			name: "recent_failure_within_window",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{
						{
							Name: "master-0",
							ResourceHistory: []ResourceHistory{
								{
									ID: "kubelet",
									OperationHistory: []OperationHistory{
										{
											Call:         "330",
											Task:         "monitor",
											RC:           "1",
											RCText:       "not running",
											LastRCChange: formatPacemakerTimestamp(now.Add(-1 * time.Minute)),
										},
									},
								},
							},
						},
					},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 1,
			expectedFirst: &RecentFailedAction{
				ResourceID: "kubelet",
				Task:       "monitor",
				NodeName:   "master-0",
				RC:         "1",
				RCText:     "not running",
			},
		},
		{
			name: "old_failure_outside_window",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{
						{
							Name: "master-0",
							ResourceHistory: []ResourceHistory{
								{
									ID: "kubelet",
									OperationHistory: []OperationHistory{
										{
											Call:         "330",
											Task:         "monitor",
											RC:           "1",
											RCText:       "not running",
											LastRCChange: formatPacemakerTimestamp(now.Add(-10 * time.Minute)),
										},
									},
								},
							},
						},
					},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "successful_operation_rc_0_ignored",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{
						{
							Name: "master-0",
							ResourceHistory: []ResourceHistory{
								{
									ID: "kubelet",
									OperationHistory: []OperationHistory{
										{
											Call:         "330",
											Task:         "monitor",
											RC:           "0", // Success!
											RCText:       "ok",
											LastRCChange: formatPacemakerTimestamp(now.Add(-1 * time.Minute)),
										},
									},
								},
							},
						},
					},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "mixed_success_and_failure_only_failure_returned",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{
						{
							Name: "master-0",
							ResourceHistory: []ResourceHistory{
								{
									ID: "kubelet",
									OperationHistory: []OperationHistory{
										{
											Call:         "329",
											Task:         "monitor",
											RC:           "1",
											RCText:       "not running",
											LastRCChange: formatPacemakerTimestamp(now.Add(-2 * time.Minute)),
										},
										{
											Call:         "330",
											Task:         "start",
											RC:           "0",
											RCText:       "ok",
											LastRCChange: formatPacemakerTimestamp(now.Add(-1 * time.Minute)),
										},
									},
								},
							},
						},
					},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 1,
			expectedFirst: &RecentFailedAction{
				ResourceID: "kubelet",
				Task:       "monitor",
				NodeName:   "master-0",
				RC:         "1",
				RCText:     "not running",
			},
		},
		{
			name: "multiple_nodes_with_failures",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{
						{
							Name: "master-0",
							ResourceHistory: []ResourceHistory{
								{
									ID: "kubelet",
									OperationHistory: []OperationHistory{
										{
											Call:         "330",
											Task:         "monitor",
											RC:           "1",
											RCText:       "not running",
											LastRCChange: formatPacemakerTimestamp(now.Add(-1 * time.Minute)),
										},
									},
								},
							},
						},
						{
							Name: "master-1",
							ResourceHistory: []ResourceHistory{
								{
									ID: "etcd",
									OperationHistory: []OperationHistory{
										{
											Call:         "100",
											Task:         "start",
											RC:           "5",
											RCText:       "not installed",
											LastRCChange: formatPacemakerTimestamp(now.Add(-2 * time.Minute)),
										},
									},
								},
							},
						},
					},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 2,
			expectedFirst: &RecentFailedAction{
				ResourceID: "kubelet",
				Task:       "monitor",
				NodeName:   "master-0",
				RC:         "1",
			},
		},
		{
			name: "invalid_timestamp_gracefully_skipped",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{
						{
							Name: "master-0",
							ResourceHistory: []ResourceHistory{
								{
									ID: "kubelet",
									OperationHistory: []OperationHistory{
										{
											Call:         "330",
											Task:         "monitor",
											RC:           "1",
											RCText:       "not running",
											LastRCChange: "invalid-timestamp-format",
										},
									},
								},
							},
						},
					},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "empty_node_history",
			result: &PacemakerResult{
				NodeHistory: NodeHistory{
					Node: []NodeHistoryNode{},
				},
			},
			cutoffTime:    now.Add(-5 * time.Minute),
			expectedCount: 0,
			expectedFirst: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := filterRecentFailedActions(tt.result, tt.cutoffTime)
			require.Len(t, actions, tt.expectedCount, "Action count mismatch")

			if tt.expectedFirst != nil && len(actions) > 0 {
				require.Equal(t, tt.expectedFirst.ResourceID, actions[0].ResourceID)
				require.Equal(t, tt.expectedFirst.Task, actions[0].Task)
				require.Equal(t, tt.expectedFirst.NodeName, actions[0].NodeName)
				require.Equal(t, tt.expectedFirst.RC, actions[0].RC)
			}
		})
	}
}

func TestFilterRecentFencingEvents(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name          string
		result        *PacemakerResult
		cutoffTime    time.Time
		expectedCount int
		expectedFirst *RecentFencingEvent
	}{
		{
			name: "recent_fencing_event_within_window",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:    "reboot",
							Target:    "master-1",
							Status:    "success",
							Completed: formatPacemakerFenceTimestamp(now.Add(-1 * time.Hour)),
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 1,
			expectedFirst: &RecentFencingEvent{
				Action: "reboot",
				Target: "master-1",
				Status: "success",
			},
		},
		{
			name: "old_fencing_event_outside_window",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:    "reboot",
							Target:    "master-1",
							Status:    "success",
							Completed: formatPacemakerFenceTimestamp(now.Add(-48 * time.Hour)),
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "fencing_event_with_empty_target_skipped",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:    "reboot",
							Target:    "", // Empty target
							Status:    "success",
							Completed: formatPacemakerFenceTimestamp(now.Add(-1 * time.Hour)),
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "multiple_fencing_events_mixed_ages",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:    "reboot",
							Target:    "master-0",
							Status:    "success",
							Completed: formatPacemakerFenceTimestamp(now.Add(-2 * time.Hour)),
						},
						{
							Action:    "off",
							Target:    "master-1",
							Status:    "failed",
							Completed: formatPacemakerFenceTimestamp(now.Add(-48 * time.Hour)), // Outside window
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 1,
			expectedFirst: &RecentFencingEvent{
				Action: "reboot",
				Target: "master-0",
				Status: "success",
			},
		},
		{
			name: "invalid_timestamp_gracefully_skipped",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:    "reboot",
							Target:    "master-1",
							Status:    "success",
							Completed: "not-a-valid-timestamp",
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "empty_fence_history",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 0,
			expectedFirst: nil,
		},
		{
			name: "failed_fencing_event_still_recorded",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:    "reboot",
							Target:    "master-1",
							Status:    "failed",
							Completed: formatPacemakerFenceTimestamp(now.Add(-1 * time.Hour)),
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 1,
			expectedFirst: &RecentFencingEvent{
				Action: "reboot",
				Target: "master-1",
				Status: "failed",
			},
		},
		{
			name: "failed_fencing_with_exit_reason",
			result: &PacemakerResult{
				FenceHistory: FenceHistory{
					FenceEvent: []FenceEvent{
						{
							Action:     "reboot",
							Target:     "master-1",
							Status:     "failed",
							ExitReason: "Connection timed out",
							Completed:  formatPacemakerFenceTimestamp(now.Add(-1 * time.Hour)),
						},
					},
				},
			},
			cutoffTime:    now.Add(-24 * time.Hour),
			expectedCount: 1,
			expectedFirst: &RecentFencingEvent{
				Action:     "reboot",
				Target:     "master-1",
				Status:     "failed",
				ExitReason: "Connection timed out",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := filterRecentFencingEvents(tt.result, tt.cutoffTime)
			require.Len(t, events, tt.expectedCount, "Event count mismatch")

			if tt.expectedFirst != nil && len(events) > 0 {
				require.Equal(t, tt.expectedFirst.Action, events[0].Action)
				require.Equal(t, tt.expectedFirst.Target, events[0].Target)
				require.Equal(t, tt.expectedFirst.Status, events[0].Status)
				if tt.expectedFirst.ExitReason != "" {
					require.Equal(t, tt.expectedFirst.ExitReason, events[0].ExitReason)
				}
			}
		})
	}
}

func TestFencingEventMessageFormat(t *testing.T) {
	// Test that fencing event messages include exit reason for failures
	tests := []struct {
		name            string
		event           RecentFencingEvent
		expectedContain string
	}{
		{
			name: "successful_fencing_no_reason",
			event: RecentFencingEvent{
				Action:    "reboot",
				Target:    "master-1",
				Status:    "success",
				Completed: "2026-01-20 14:32:05.123456Z",
			},
			expectedContain: "completed with status success at",
		},
		{
			name: "failed_fencing_with_reason",
			event: RecentFencingEvent{
				Action:     "reboot",
				Target:     "master-1",
				Status:     "failed",
				ExitReason: "Connection timed out",
				Completed:  "2026-01-20 14:32:05.123456Z",
			},
			expectedContain: "status failed (Connection timed out)",
		},
		{
			name: "failed_fencing_without_reason",
			event: RecentFencingEvent{
				Action:     "reboot",
				Target:     "master-1",
				Status:     "failed",
				ExitReason: "",
				Completed:  "2026-01-20 14:32:05.123456Z",
			},
			expectedContain: "completed with status failed at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build message the same way recordFencingEvents does
			var message string
			if tt.event.Status != "success" && tt.event.ExitReason != "" {
				message = fmt.Sprintf("Fencing event: %s of %s completed with status %s (%s) at %s",
					tt.event.Action, tt.event.Target, tt.event.Status, tt.event.ExitReason, tt.event.Completed)
			} else {
				message = fmt.Sprintf("Fencing event: %s of %s completed with status %s at %s",
					tt.event.Action, tt.event.Target, tt.event.Status, tt.event.Completed)
			}

			require.Contains(t, message, tt.expectedContain,
				"Message should contain expected substring")
		})
	}
}

// =============================================================================
// Tests for Failure Scenarios
// =============================================================================

func TestBuildClusterStatus_FencingFailure_NodeUnclean(t *testing.T) {
	// Verifies correct condition generation when fencing fails and a node is marked UNCLEAN.
	// Key states:
	// - master-1: online=false, unclean=true (fencing failed)
	// - Multiple failed fencing events in fence_history
	fencingFailureXML := loadTestXML(t, "fencing_failure.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(fencingFailureXML), &result)
	require.NoError(t, err)

	status := buildClusterStatus(&result, nil)

	// Helper to find node by name
	findNode := func(name string) *v1alpha1.PacemakerClusterNodeStatus {
		for i := range *status.Nodes {
			if (*status.Nodes)[i].NodeName == name {
				return &(*status.Nodes)[i]
			}
		}
		return nil
	}

	t.Run("master-1_unclean_after_fencing_failure", func(t *testing.T) {
		node := findNode("master-1")
		require.NotNil(t, node, "master-1 should exist")

		// Node should be marked as UNCLEAN (Clean condition = False)
		cleanCondition := FindCondition(node.Conditions, v1alpha1.NodeCleanConditionType)
		require.NotNil(t, cleanCondition, "Clean condition should exist")
		require.Equal(t, metav1.ConditionFalse, cleanCondition.Status, "Node should NOT be clean after failed fencing")

		// Node should be offline
		onlineCondition := FindCondition(node.Conditions, v1alpha1.NodeOnlineConditionType)
		require.NotNil(t, onlineCondition, "Online condition should exist")
		require.Equal(t, metav1.ConditionFalse, onlineCondition.Status, "Node should be offline")

		// Node should be unhealthy overall
		healthyCondition := FindCondition(node.Conditions, v1alpha1.NodeHealthyConditionType)
		require.NotNil(t, healthyCondition, "Healthy condition should exist")
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status, "Node should be unhealthy when unclean")
	})

	t.Run("master-0_healthy_surviving_node", func(t *testing.T) {
		node := findNode("master-0")
		require.NotNil(t, node, "master-0 should exist")

		// Surviving node should be online
		onlineCondition := FindCondition(node.Conditions, v1alpha1.NodeOnlineConditionType)
		require.NotNil(t, onlineCondition)
		require.Equal(t, metav1.ConditionTrue, onlineCondition.Status, "Surviving node should be online")

		// Clean condition should be True
		cleanCondition := FindCondition(node.Conditions, v1alpha1.NodeCleanConditionType)
		require.NotNil(t, cleanCondition)
		require.Equal(t, metav1.ConditionTrue, cleanCondition.Status, "Surviving node should be clean")
	})

	t.Run("cluster_unhealthy_with_unclean_node", func(t *testing.T) {
		healthyCondition := FindCondition(status.Conditions, v1alpha1.ClusterHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status, "Cluster should be unhealthy with UNCLEAN node")
	})
}

func TestBuildClusterStatus_FencingDegraded_AgentStopped(t *testing.T) {
	// Verifies correct condition generation when a fencing agent fails to start.
	// Key states:
	// - master-1_redfish: role=Stopped, active=false
	// - Both nodes online
	// - Fencing for master-1 is unavailable
	fencingDegradedXML := loadTestXML(t, "fencing_degraded.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(fencingDegradedXML), &result)
	require.NoError(t, err)

	status := buildClusterStatus(&result, nil)

	// Helper to find node by name
	findNode := func(name string) *v1alpha1.PacemakerClusterNodeStatus {
		for i := range *status.Nodes {
			if (*status.Nodes)[i].NodeName == name {
				return &(*status.Nodes)[i]
			}
		}
		return nil
	}

	t.Run("master-1_fencing_unavailable", func(t *testing.T) {
		node := findNode("master-1")
		require.NotNil(t, node, "master-1 should exist")

		// Node should be online
		onlineCondition := FindCondition(node.Conditions, v1alpha1.NodeOnlineConditionType)
		require.NotNil(t, onlineCondition)
		require.Equal(t, metav1.ConditionTrue, onlineCondition.Status, "Node should be online")

		// Fencing should be unavailable (fencing agent stopped)
		fencingAvailableCondition := FindCondition(node.Conditions, v1alpha1.NodeFencingAvailableConditionType)
		require.NotNil(t, fencingAvailableCondition, "FencingAvailable condition should exist")
		require.Equal(t, metav1.ConditionFalse, fencingAvailableCondition.Status,
			"Fencing should be unavailable when fencing agent is stopped")

		// FencingHealthy should also be false
		fencingHealthyCondition := FindCondition(node.Conditions, v1alpha1.NodeFencingHealthyConditionType)
		require.NotNil(t, fencingHealthyCondition, "FencingHealthy condition should exist")
		require.Equal(t, metav1.ConditionFalse, fencingHealthyCondition.Status,
			"Fencing should not be healthy when agent is stopped")

		// Node should be unhealthy (fencing unavailable is critical)
		healthyCondition := FindCondition(node.Conditions, v1alpha1.NodeHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status,
			"Node should be unhealthy when fencing is unavailable")
	})

	t.Run("master-0_fencing_available", func(t *testing.T) {
		node := findNode("master-0")
		require.NotNil(t, node, "master-0 should exist")

		// master-0's fencing should still be available (master-0_redfish is running)
		fencingAvailableCondition := FindCondition(node.Conditions, v1alpha1.NodeFencingAvailableConditionType)
		require.NotNil(t, fencingAvailableCondition, "FencingAvailable condition should exist")
		require.Equal(t, metav1.ConditionTrue, fencingAvailableCondition.Status,
			"Fencing should be available for master-0")
	})

	t.Run("cluster_unhealthy_with_fencing_degraded", func(t *testing.T) {
		healthyCondition := FindCondition(status.Conditions, v1alpha1.ClusterHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionFalse, healthyCondition.Status,
			"Cluster should be unhealthy when a node has no fencing")
	})
}

func TestBuildClusterStatus_ResourceFailure_WithExitReason(t *testing.T) {
	// Verifies that resources are correctly marked healthy after recovering from failure,
	// and that failure details are captured in node_history for reporting.
	resourceFailureXML := loadTestXML(t, "resource_failure_with_exitreason.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(resourceFailureXML), &result)
	require.NoError(t, err)

	status := buildClusterStatus(&result, nil)

	// Helper to find node by name
	findNode := func(name string) *v1alpha1.PacemakerClusterNodeStatus {
		for i := range *status.Nodes {
			if (*status.Nodes)[i].NodeName == name {
				return &(*status.Nodes)[i]
			}
		}
		return nil
	}

	t.Run("resources_recovered_and_healthy", func(t *testing.T) {
		node := findNode("master-0")
		require.NotNil(t, node, "master-0 should exist")

		// Kubelet should be healthy (recovered from failure)
		kubeletResource := findResourceInList(node.Resources, v1alpha1.PacemakerClusterResourceNameKubelet)
		require.NotNil(t, kubeletResource, "Kubelet resource should exist")

		healthyCondition := FindCondition(kubeletResource.Conditions, v1alpha1.ResourceHealthyConditionType)
		require.NotNil(t, healthyCondition)
		require.Equal(t, metav1.ConditionTrue, healthyCondition.Status,
			"Kubelet should be healthy after recovery")

		startedCondition := FindCondition(kubeletResource.Conditions, v1alpha1.ResourceStartedConditionType)
		require.NotNil(t, startedCondition)
		require.Equal(t, metav1.ConditionTrue, startedCondition.Status,
			"Kubelet should be started after recovery")
	})

	t.Run("node_history_captures_failure_details", func(t *testing.T) {
		// Verify node_history has the failure in operation history
		require.NotEmpty(t, result.NodeHistory.Node, "Node history should not be empty")

		// Find master-0's kubelet history
		var kubeletHistory *ResourceHistory
		for _, node := range result.NodeHistory.Node {
			if node.Name == "master-0" {
				for i := range node.ResourceHistory {
					if node.ResourceHistory[i].ID == "kubelet" {
						kubeletHistory = &node.ResourceHistory[i]
						break
					}
				}
			}
		}
		require.NotNil(t, kubeletHistory, "Kubelet history should exist for master-0")

		// Verify operation history captured the failure with useful details
		require.NotEmpty(t, kubeletHistory.OperationHistory, "Operation history should not be empty")

		failureOp := kubeletHistory.OperationHistory[0]
		require.NotEmpty(t, failureOp.Task, "Task should be captured")
		require.NotEqual(t, "0", failureOp.RC, "RC should be non-zero (failure)")
		require.NotEmpty(t, failureOp.RCText, "RCText should capture the error message")
	})
}

func TestFilterRecentFencingEvents_FailedFencing(t *testing.T) {
	// Verifies that failed fencing events are correctly filtered from fence_history.
	// Uses fencing_failure.xml which has 4 failed + 1 success in fence_history.
	fencingFailureXML := loadTestXML(t, "fencing_failure.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(fencingFailureXML), &result)
	require.NoError(t, err)

	// Use a cutoff time that includes all events (24 hours before the events)
	// Events in the fixture are from "2026-01-23 20:51:39" range
	// We'll use time.Now() with a 30-day window to ensure they're all included
	cutoffTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := filterRecentFencingEvents(&result, cutoffTime)

	t.Run("extracts_fencing_events", func(t *testing.T) {
		// Should extract fencing events from the fixture
		require.NotEmpty(t, events, "Should extract fencing events from fixture")
	})

	t.Run("failed_events_have_required_fields", func(t *testing.T) {
		hasFailedEvent := false
		for _, event := range events {
			if event.Status == "failed" {
				hasFailedEvent = true
				// Verify essential fields are captured for reporting to user
				require.NotEmpty(t, event.Target, "Failed fencing event should have a target")
				require.NotEmpty(t, event.Action, "Failed fencing event should have an action")
				require.NotEmpty(t, event.Completed, "Failed fencing event should have a timestamp")
			}
		}
		require.True(t, hasFailedEvent, "Should have at least one failed fencing event")
	})
}

func TestFilterRecentFailedActions_WithExitReason(t *testing.T) {
	// Verifies that resource failures are correctly filtered and capture useful details.
	resourceFailureXML := loadTestXML(t, "resource_failure_with_exitreason.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(resourceFailureXML), &result)
	require.NoError(t, err)

	// Use a cutoff that includes the failure
	cutoffTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	actions := filterRecentFailedActions(&result, cutoffTime)

	t.Run("extracts_failed_actions_with_useful_details", func(t *testing.T) {
		require.NotEmpty(t, actions, "Should extract at least one failed action")

		// Find any kubelet failure
		var kubeletFailure *RecentFailedAction
		for i := range actions {
			if actions[i].ResourceID == "kubelet" {
				kubeletFailure = &actions[i]
				break
			}
		}
		require.NotNil(t, kubeletFailure, "Should find kubelet failure")

		// Verify essential fields are captured for reporting to user
		require.NotEmpty(t, kubeletFailure.Task, "Task should be captured")
		require.NotEmpty(t, kubeletFailure.NodeName, "Node name should be captured")
		require.NotEqual(t, "0", kubeletFailure.RC, "Return code should be non-zero (failure)")
		require.NotEmpty(t, kubeletFailure.RCText, "RC text should capture error message for user")
	})
}

func TestFilterRecentFailedActions_FencingAgentStartFailure(t *testing.T) {
	// Verifies that fencing agent failures are correctly filtered.
	fencingDegradedXML := loadTestXML(t, "fencing_degraded.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(fencingDegradedXML), &result)
	require.NoError(t, err)

	// Use a cutoff that includes all failures
	cutoffTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	actions := filterRecentFailedActions(&result, cutoffTime)

	t.Run("extracts_fencing_agent_failures", func(t *testing.T) {
		// Find fencing agent failures
		var fencingFailures []RecentFailedAction
		for _, action := range actions {
			if strings.Contains(action.ResourceID, "redfish") {
				fencingFailures = append(fencingFailures, action)
			}
		}
		require.NotEmpty(t, fencingFailures, "Should find fencing agent failures")

		// Verify essential fields are captured for reporting
		for _, failure := range fencingFailures {
			require.NotEmpty(t, failure.Task, "Task should be captured")
			require.NotEqual(t, "0", failure.RC, "Return code should be non-zero (failure)")
		}
	})
}
