package pacemaker

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// Helper function to load test XML files
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

func TestBuildStatusComponents_HealthyCluster(t *testing.T) {
	// Test with healthy cluster XML
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(healthyXML), &result)
	require.NoError(t, err)

	summary, nodes, resources, nodeHistory, fencingHistory := buildStatusComponents(&result, createTestClusterConfig())

	// Verify summary
	require.Equal(t, v1alpha1.PacemakerDaemonStateRunning, summary.PacemakerDaemonState)
	require.Equal(t, v1alpha1.QuorumStatusQuorate, summary.QuorumStatus)
	require.NotNil(t, summary.NodesOnline)
	require.Equal(t, int32(2), *summary.NodesOnline)
	require.NotNil(t, summary.NodesTotal)
	require.Equal(t, int32(2), *summary.NodesTotal)
	require.NotNil(t, summary.ResourcesStarted)
	require.Greater(t, *summary.ResourcesStarted, int32(0))

	// Verify nodes
	require.Len(t, nodes, 2)
	for _, node := range nodes {
		require.Equal(t, v1alpha1.NodeOnlineStatusOnline, node.OnlineStatus, "All nodes should be online")
		require.Equal(t, v1alpha1.NodeModeActive, node.Mode, "No nodes should be in standby")
	}

	// Verify resources
	require.NotEmpty(t, resources)
	foundKubelet := false
	foundEtcd := false
	for _, resource := range resources {
		if strings.Contains(resource.ResourceAgent, "kubelet") {
			foundKubelet = true
		}
		if strings.Contains(resource.ResourceAgent, "etcd") {
			foundEtcd = true
		}
	}
	require.True(t, foundKubelet, "Should find kubelet resources")
	require.True(t, foundEtcd, "Should find etcd resources")

	// No recent failures or fencing in healthy cluster
	require.Empty(t, nodeHistory)
	require.Empty(t, fencingHistory)
}

func TestBuildStatusComponents_OfflineNode(t *testing.T) {
	// Test with offline node XML
	offlineXML := loadTestXML(t, "offline_node.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(offlineXML), &result)
	require.NoError(t, err)

	summary, nodes, _, _, _ := buildStatusComponents(&result, createTestClusterConfig())

	// Verify summary shows reduced online count
	require.Equal(t, v1alpha1.PacemakerDaemonStateRunning, summary.PacemakerDaemonState)
	require.NotNil(t, summary.NodesOnline)
	require.Equal(t, int32(1), *summary.NodesOnline, "Only one node should be online")
	require.NotNil(t, summary.NodesTotal)
	require.Equal(t, int32(2), *summary.NodesTotal)

	// Verify nodes - one should be offline
	require.Len(t, nodes, 2)
	offlineCount := 0
	for _, node := range nodes {
		if node.OnlineStatus != "Online" {
			offlineCount++
		}
	}
	require.Equal(t, 1, offlineCount, "Should have exactly one offline node")
}

func TestBuildStatusComponents_StandbyNode(t *testing.T) {
	// Test with standby node XML
	standbyXML := loadTestXML(t, "standby_node.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(standbyXML), &result)
	require.NoError(t, err)

	_, nodes, _, _, _ := buildStatusComponents(&result, createTestClusterConfig())

	// Verify nodes - one should be in standby
	require.Len(t, nodes, 2)
	standbyCount := 0
	for _, node := range nodes {
		if node.Mode == v1alpha1.NodeModeStandby {
			standbyCount++
		}
	}
	require.Equal(t, 1, standbyCount, "Should have exactly one standby node")
}

func TestBuildStatusComponents_RecentFailures(t *testing.T) {
	// Test with recent failures XML
	recentFailuresXML := getRecentFailuresXML(t)
	var result PacemakerResult
	err := xml.Unmarshal([]byte(recentFailuresXML), &result)
	require.NoError(t, err)

	_, _, _, nodeHistory, fencingHistory := buildStatusComponents(&result, createTestClusterConfig())

	// Verify node history contains recent failures
	require.NotEmpty(t, nodeHistory, "Should have node history entries")
	foundFailure := false
	for _, entry := range nodeHistory {
		if entry.RC != nil && *entry.RC != 0 {
			foundFailure = true
			require.NotEmpty(t, entry.Node, "Entry should have node name")
			require.NotEmpty(t, entry.Resource, "Entry should have resource name")
			require.NotEmpty(t, entry.Operation, "Entry should have operation")
			require.NotEmpty(t, entry.RCText, "Entry should have RC text")
			require.False(t, entry.LastRCChange.IsZero(), "Entry should have timestamp")
		}
	}
	require.True(t, foundFailure, "Should have at least one failed operation")

	// Verify fencing history contains recent fencing events
	require.NotEmpty(t, fencingHistory, "Should have fencing history entries")
	for _, event := range fencingHistory {
		require.NotEmpty(t, event.Target, "Event should have target")
		require.NotEmpty(t, event.Action, "Event should have action")
		require.NotEmpty(t, event.Status, "Event should have status")
		require.False(t, event.Completed.IsZero(), "Event should have timestamp")
	}
}

func TestBuildStatusComponents_TimeWindowFiltering(t *testing.T) {
	// Create a test result with operations at different times
	now := time.Now()
	recentTime := now.Add(-2 * time.Minute)    // Within 5-minute window
	oldTime := now.Add(-10 * time.Minute)      // Outside 5-minute window
	recentFenceTime := now.Add(-1 * time.Hour) // Within 24-hour window
	oldFenceTime := now.Add(-48 * time.Hour)   // Outside 24-hour window

	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true"},
			},
		},
		NodeHistory: NodeHistory{
			Node: []NodeHistoryNode{
				{
					Name: "master-0",
					ResourceHistory: []ResourceHistory{
						{
							ID: "etcd-clone-0",
							OperationHistory: []OperationHistory{
								{
									Task:         "monitor",
									RC:           "1",
									RCText:       "error",
									LastRCChange: recentTime.UTC().Format("Mon Jan 2 15:04:05 2006"),
								},
								{
									Task:         "monitor",
									RC:           "1",
									RCText:       "error",
									LastRCChange: oldTime.UTC().Format("Mon Jan 2 15:04:05 2006"),
								},
							},
						},
					},
				},
			},
		},
		FenceHistory: FenceHistory{
			FenceEvent: []FenceEvent{
				{
					Target:    "master-1",
					Action:    "reboot",
					Status:    "success",
					Completed: recentFenceTime.UTC().Format("2006-01-02 15:04:05.000000Z"),
				},
				{
					Target:    "master-1",
					Action:    "reboot",
					Status:    "success",
					Completed: oldFenceTime.UTC().Format("2006-01-02 15:04:05.000000Z"),
				},
			},
		},
	}

	_, _, _, nodeHistory, fencingHistory := buildStatusComponents(result, createTestClusterConfig())

	// Only recent operations should be included
	require.Len(t, nodeHistory, 1, "Should only include recent operation")

	// Only recent fencing events should be included
	require.Len(t, fencingHistory, 1, "Should only include recent fencing event")
}

func TestBuildStatusComponents_ResourceCounting(t *testing.T) {
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true"},
				{Name: "master-1", Online: "true"},
			},
		},
		Resources: Resources{
			Clone: []Clone{
				{
					Resource: []Resource{
						{
							ID:            "kubelet-0",
							ResourceAgent: resourceAgentKubelet,
							Role:          "Started",
							Active:        "true",
							Node:          NodeRef{Name: "master-0"},
						},
						{
							ID:            "kubelet-1",
							ResourceAgent: resourceAgentKubelet,
							Role:          "Started",
							Active:        "true",
							Node:          NodeRef{Name: "master-1"},
						},
					},
				},
			},
			Resource: []Resource{
				{
					ID:            "etcd-0",
					ResourceAgent: resourceAgentEtcd,
					Role:          "Started",
					Active:        "true",
					Node:          NodeRef{Name: "master-0"},
				},
				{
					ID:            "etcd-1",
					ResourceAgent: resourceAgentEtcd,
					Role:          "Stopped",
					Active:        "false",
					Node:          NodeRef{Name: ""},
				},
			},
		},
	}

	summary, _, resources, _, _ := buildStatusComponents(result, createTestClusterConfig())

	// Verify resource counting
	require.NotNil(t, summary.ResourcesTotal)
	require.Equal(t, int32(4), *summary.ResourcesTotal, "Should count all resources")
	require.NotNil(t, summary.ResourcesStarted)
	require.Equal(t, int32(3), *summary.ResourcesStarted, "Should count only started resources")

	// Verify resource details
	require.Len(t, resources, 4)
	startedCount := 0
	for _, resource := range resources {
		if resource.Role == "Started" && resource.ActiveStatus == v1alpha1.ResourceActiveStatusActive {
			startedCount++
		}
	}
	require.Equal(t, 3, startedCount, "Should have 3 started resources")
}

func TestBuildStatusComponents_EmptyXML(t *testing.T) {
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "false"},
		},
	}

	// Test without cluster config (fallback mode) - should have no nodes
	summary, nodes, resources, nodeHistory, fencingHistory := buildStatusComponents(result, nil)

	// Verify minimal valid result
	require.Equal(t, v1alpha1.PacemakerDaemonStateRunning, summary.PacemakerDaemonState)
	require.Equal(t, v1alpha1.QuorumStatusNoQuorum, summary.QuorumStatus)
	require.NotNil(t, summary.NodesOnline)
	require.Equal(t, int32(0), *summary.NodesOnline)
	require.NotNil(t, summary.NodesTotal)
	require.Equal(t, int32(0), *summary.NodesTotal)
	require.Empty(t, nodes, "Without cluster config and empty XML, should have no nodes")
	require.Empty(t, resources)
	require.Empty(t, nodeHistory)
	require.Empty(t, fencingHistory)
}

func TestBuildStatusComponents_QuorumHandling(t *testing.T) {
	tests := []struct {
		name                 string
		withQuorum           string
		expectedQuorumStatus v1alpha1.QuorumStatusType
	}{
		{"quorum_true", "true", v1alpha1.QuorumStatusQuorate},
		{"quorum_false", "false", v1alpha1.QuorumStatusNoQuorum},
		{"quorum_empty", "", v1alpha1.QuorumStatusNoQuorum},
		{"quorum_invalid", "invalid", v1alpha1.QuorumStatusNoQuorum},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &PacemakerResult{
				Summary: Summary{
					Stack:     Stack{PacemakerdState: "running"},
					CurrentDC: CurrentDC{WithQuorum: tt.withQuorum},
				},
			}

			summary, _, _, _, _ := buildStatusComponents(result, createTestClusterConfig())
			require.Equal(t, tt.expectedQuorumStatus, summary.QuorumStatus)
		})
	}
}

func TestBuildStatusComponents_NodeStatusVariations(t *testing.T) {
	tests := []struct {
		name                 string
		online               string
		standby              string
		expectedOnlineStatus string
		expectedMode         string
	}{
		{"online_normal", "true", "false", "Online", "Active"},
		{"offline", "false", "false", "Offline", "Active"},
		{"online_standby", "true", "true", "Online", "Standby"},
		{"offline_standby", "false", "true", "Offline", "Standby"},
		{"empty_values", "", "", "Offline", "Active"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &PacemakerResult{
				Summary: Summary{
					Stack:     Stack{PacemakerdState: "running"},
					CurrentDC: CurrentDC{WithQuorum: "true"},
				},
				Nodes: Nodes{
					Node: []Node{
						{
							Name:    "test-node",
							Online:  tt.online,
							Standby: tt.standby,
						},
					},
				},
			}

			// Test without cluster config (XML-only mode) - nodes should appear with empty IP
			_, nodes, _, _, _ := buildStatusComponents(result, nil)
			// Without cluster config, nodes should still appear but with empty IPAddress
			require.Len(t, nodes, 1, "Nodes should be included even without IP address")
			require.Equal(t, "test-node", nodes[0].Name)
			require.Empty(t, nodes[0].IPAddress, "IPAddress should be empty when not available")
			require.Equal(t, tt.expectedOnlineStatus, string(nodes[0].OnlineStatus))
			require.Equal(t, tt.expectedMode, string(nodes[0].Mode))
		})
	}
}

func TestCollectPacemakerStatus_XMLSizeValidation(t *testing.T) {
	// This test verifies that large XML is rejected
	// In a real test, you would mock the exec.Execute function
	// For now, we just verify the maxXMLSize constant is reasonable
	require.Equal(t, 10*1024*1024, maxXMLSize, "Max XML size should be 10MB")
}

func TestCollectPacemakerStatus_InvalidXMLHandling(t *testing.T) {
	invalidXML := "<invalid><unclosed>"
	var result PacemakerResult
	err := xml.Unmarshal([]byte(invalidXML), &result)

	// Should fail to unmarshal but not panic
	require.Error(t, err, "Should return error for invalid XML")
}

func TestBuildStatusComponents_NodeIPExtraction(t *testing.T) {
	// Test with custom cluster config to verify IPs come from cluster config, not XML attributes
	customConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "192.168.1.10", Link: "0", Type: "IPv4"},
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
				{Name: "master-0", Online: "true"},
			},
		},
		NodeAttributes: NodeAttributes{
			Node: []NodeAttributeSet{
				{
					Name: "master-0",
					Attribute: []NodeAttribute{
						{Name: "node_ip", Value: "10.0.0.1"}, // This should be ignored
						{Name: "other_attr", Value: "value"},
					},
				},
			},
		},
	}

	// Verify IP comes from cluster config, not node_ip attribute
	_, nodes, _, _, _ := buildStatusComponents(result, customConfig)
	require.Len(t, nodes, 1)
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "192.168.1.10", nodes[0].IPAddress, "IP address should come from cluster config, not XML attribute")
}

func TestBuildStatusComponents_NodeIPFiltering(t *testing.T) {
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true"},  // Has IP
				{Name: "master-1", Online: "true"},  // No IP - should be filtered
				{Name: "master-2", Online: "false"}, // Has IP
			},
		},
		NodeAttributes: NodeAttributes{
			Node: []NodeAttributeSet{
				{
					Name: "master-0",
					Attribute: []NodeAttribute{
						{Name: "node_ip", Value: "192.168.1.10"},
					},
				},
				{
					Name: "master-2",
					Attribute: []NodeAttribute{
						{Name: "node_ip", Value: "192.168.1.12"},
					},
				},
				// master-1 has no node_ip attribute
			},
		},
	}

	// Test with cluster config: all nodes from cluster config are included with their config IPs
	_, nodes, _, _, _ := buildStatusComponents(result, createTestClusterConfig())
	require.Len(t, nodes, 2, "All nodes from cluster config should be included")
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "192.168.111.20", nodes[0].IPAddress, "IP should come from cluster config")
	require.Equal(t, "master-1", nodes[1].Name)
	require.Equal(t, "192.168.111.21", nodes[1].IPAddress, "IP should come from cluster config")

	// Test without cluster config (fallback): nodes from XML are included with empty IPs
	_, nodesFallback, _, _, _ := buildStatusComponents(result, nil)
	require.Len(t, nodesFallback, 3, "All XML nodes should be included in fallback mode")

	// Verify IPs in fallback mode (empty since no cluster config)
	for _, node := range nodesFallback {
		require.Empty(t, node.IPAddress, "Without cluster config, IPs should be empty")
	}
}

func TestBuildStatusComponents_ResourceAgentTypes(t *testing.T) {
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Resources: Resources{
			Resource: []Resource{
				{
					ID:            "kubelet-0",
					ResourceAgent: "systemd:kubelet",
					Role:          "Started",
					Active:        "true",
				},
				{
					ID:            "etcd-0",
					ResourceAgent: resourceAgentEtcd,
					Role:          "Started",
					Active:        "true",
				},
				{
					ID:            "ip-0",
					ResourceAgent: resourceAgentIPAddr,
					Role:          "Started",
					Active:        "true",
				},
			},
		},
	}

	_, _, resources, _, _ := buildStatusComponents(result, createTestClusterConfig())

	require.Len(t, resources, 3)

	// Verify all resource agents are preserved
	agents := make(map[string]bool)
	for _, resource := range resources {
		agents[resource.ResourceAgent] = true
	}

	require.True(t, agents[resourceAgentKubelet])
	require.True(t, agents[resourceAgentEtcd])
	require.True(t, agents[resourceAgentIPAddr])
}

// Cluster Config Integration Tests

func TestBuildStatusComponents_ClusterConfigPriority(t *testing.T) {
	// Test that cluster config IPs take priority over XML node_ip attributes
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.10", Link: "0", Type: "IPv4"},
			},
		},
		{
			Name:   "master-1",
			NodeID: "2",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.11", Link: "0", Type: "IPv4"},
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
				{Name: "master-0", Online: "true"},
				{Name: "master-1", Online: "false"},
			},
		},
		NodeAttributes: NodeAttributes{
			Node: []NodeAttributeSet{
				{
					Name: "master-0",
					Attribute: []NodeAttribute{
						{Name: "node_ip", Value: "192.168.1.10"}, // This should be IGNORED
					},
				},
				{
					Name: "master-1",
					Attribute: []NodeAttribute{
						{Name: "node_ip", Value: "192.168.1.11"}, // This should be IGNORED
					},
				},
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 2, "Should have 2 nodes from cluster config")

	// Verify IPs come from cluster config, NOT XML
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "10.0.0.10", nodes[0].IPAddress, "IP should come from cluster config")
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[0].OnlineStatus, "Online status enriched from XML")

	require.Equal(t, "master-1", nodes[1].Name)
	require.Equal(t, "10.0.0.11", nodes[1].IPAddress, "IP should come from cluster config")
	require.Equal(t, v1alpha1.NodeOnlineStatusOffline, nodes[1].OnlineStatus, "Offline status enriched from XML")
}

func TestBuildStatusComponents_ClusterConfigExtraNodes(t *testing.T) {
	// Test when cluster config has more nodes than XML (e.g., node is down)
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.10", Link: "0", Type: "IPv4"},
			},
		},
		{
			Name:   "master-1",
			NodeID: "2",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.11", Link: "0", Type: "IPv4"},
			},
		},
		{
			Name:   "master-2",
			NodeID: "3",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.12", Link: "0", Type: "IPv4"},
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
				{Name: "master-0", Online: "true"},
				{Name: "master-1", Online: "true"},
				// master-2 is missing from XML (node is down/unreachable)
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 3, "Should have all 3 nodes from cluster config")

	// Verify master-2 is included with default (offline) status
	require.Equal(t, "master-2", nodes[2].Name)
	require.Equal(t, "10.0.0.12", nodes[2].IPAddress)
	require.Equal(t, v1alpha1.NodeOnlineStatusOffline, nodes[2].OnlineStatus, "Node not in XML should default to offline")
	require.Equal(t, v1alpha1.NodeModeActive, nodes[2].Mode)
}

func TestBuildStatusComponents_XMLExtraNodes(t *testing.T) {
	// Test when XML has nodes NOT in cluster config (should be ignored)
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.10", Link: "0", Type: "IPv4"},
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
				{Name: "master-0", Online: "true"},
				{Name: "rogue-node", Online: "true"}, // NOT in cluster config, should be ignored
				{Name: "old-node", Online: "false"},  // NOT in cluster config, should be ignored
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 1, "Should only have nodes from cluster config")
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "10.0.0.10", nodes[0].IPAddress)
}

func TestBuildStatusComponents_ClusterConfigInvalidIP(t *testing.T) {
	// Test handling of invalid IP addresses in cluster config
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.10", Link: "0", Type: "IPv4"}, // Valid
			},
		},
		{
			Name:   "master-1",
			NodeID: "2",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "not-an-ip", Link: "0", Type: "IPv4"}, // Invalid
			},
		},
		{
			Name:   "master-2",
			NodeID: "3",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "127.0.0.1", Link: "0", Type: "IPv4"}, // Loopback (invalid per validation)
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
				{Name: "master-0", Online: "true"},
				{Name: "master-1", Online: "true"},
				{Name: "master-2", Online: "true"},
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 3, "All nodes should be included even with invalid IPs")

	// master-0: valid IP
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "10.0.0.10", nodes[0].IPAddress)

	// master-1: invalid IP format, should have empty IP
	require.Equal(t, "master-1", nodes[1].Name)
	require.Empty(t, nodes[1].IPAddress, "Invalid IP should result in empty IPAddress")
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[1].OnlineStatus, "But online status should still be enriched")

	// master-2: loopback IP (fails IsGlobalUnicast), should have empty IP
	require.Equal(t, "master-2", nodes[2].Name)
	require.Empty(t, nodes[2].IPAddress, "Loopback IP should result in empty IPAddress")
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[2].OnlineStatus, "But online status should still be enriched")
}

func TestBuildStatusComponents_ClusterConfigNoAddresses(t *testing.T) {
	// Test node in cluster config with no addresses
	clusterConfig := createTestClusterConfigWithNodes([]ClusterConfigNode{
		{
			Name:   "master-0",
			NodeID: "1",
			Addrs: []ClusterConfigNodeAddr{
				{Addr: "10.0.0.10", Link: "0", Type: "IPv4"},
			},
		},
		{
			Name:   "master-1",
			NodeID: "2",
			Addrs:  []ClusterConfigNodeAddr{}, // No addresses configured
		},
	})

	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true"},
				{Name: "master-1", Online: "true"},
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 2, "Both nodes should be included")

	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "10.0.0.10", nodes[0].IPAddress)

	require.Equal(t, "master-1", nodes[1].Name)
	require.Empty(t, nodes[1].IPAddress, "Node with no addresses should have empty IP")
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[1].OnlineStatus, "But still be enriched with XML data")
}

func TestBuildStatusComponents_ClusterConfigIPv6(t *testing.T) {
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
				{Name: "master-0", Online: "true"},
				{Name: "master-1", Online: "true"},
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 2)

	// Valid IPv6
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "2001:db8::1", nodes[0].IPAddress, "Valid IPv6 should be canonicalized")

	// Link-local IPv6 (invalid per validation)
	require.Equal(t, "master-1", nodes[1].Name)
	require.Empty(t, nodes[1].IPAddress, "Link-local IPv6 should fail validation and result in empty IP")
}

func TestBuildStatusComponents_FallbackModeWithoutClusterConfig(t *testing.T) {
	// Test fallback behavior when cluster config is nil
	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true", Standby: "false"},
				{Name: "master-1", Online: "false", Standby: "true"},
			},
		},
		NodeAttributes: NodeAttributes{
			Node: []NodeAttributeSet{
				{
					Name: "master-0",
					Attribute: []NodeAttribute{
						{Name: "node_ip", Value: "192.168.1.10"}, // Should be ignored without cluster config
					},
				},
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, nil)

	require.Len(t, nodes, 2, "Fallback mode should include all XML nodes")

	// All nodes should have empty IPs in fallback mode
	require.Equal(t, "master-0", nodes[0].Name)
	require.Empty(t, nodes[0].IPAddress, "Fallback mode should not populate IPs")
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[0].OnlineStatus)
	require.Equal(t, v1alpha1.NodeModeActive, nodes[0].Mode)

	require.Equal(t, "master-1", nodes[1].Name)
	require.Empty(t, nodes[1].IPAddress, "Fallback mode should not populate IPs")
	require.Equal(t, v1alpha1.NodeOnlineStatusOffline, nodes[1].OnlineStatus)
	require.Equal(t, v1alpha1.NodeModeStandby, nodes[1].Mode)
}

func TestBuildStatusComponents_StandbyEnrichment(t *testing.T) {
	// Test that standby mode from XML properly enriches cluster config nodes
	clusterConfig := createTestClusterConfig() // master-0 and master-1

	result := &PacemakerResult{
		Summary: Summary{
			Stack:     Stack{PacemakerdState: "running"},
			CurrentDC: CurrentDC{WithQuorum: "true"},
		},
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: "true", Standby: "true"},  // In standby
				{Name: "master-1", Online: "true", Standby: "false"}, // Active
			},
		},
	}

	_, nodes, _, _, _ := buildStatusComponents(result, clusterConfig)

	require.Len(t, nodes, 2)

	// Verify standby mode is properly enriched from XML
	require.Equal(t, "master-0", nodes[0].Name)
	require.Equal(t, "192.168.111.20", nodes[0].IPAddress)
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[0].OnlineStatus)
	require.Equal(t, v1alpha1.NodeModeStandby, nodes[0].Mode, "Standby mode should be enriched from XML")

	require.Equal(t, "master-1", nodes[1].Name)
	require.Equal(t, "192.168.111.21", nodes[1].IPAddress)
	require.Equal(t, v1alpha1.NodeOnlineStatusOnline, nodes[1].OnlineStatus)
	require.Equal(t, v1alpha1.NodeModeActive, nodes[1].Mode, "Active mode should be enriched from XML")
}
