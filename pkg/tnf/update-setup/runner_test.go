package updatesetup

import (
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPacemakerReconciliation tests the pacemaker reconciliation logic
// that determines whether to add/remove nodes based on k8s vs pacemaker membership (name+IP)
func TestPacemakerReconciliation(t *testing.T) {
	tests := []struct {
		name           string
		k8sNodes       map[string]string // name -> IP
		pacemakerNodes map[string]string // name -> IP
		expectRemove   []string          // Nodes expected to be removed, empty if none
		expectAdd      []string          // Nodes expected to be added, empty if none
		expectSkip     bool              // True if no action should be taken
	}{
		{
			name: "Upgrade - both nodes in k8s and pacemaker with same IPs - no changes",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectRemove: nil,
			expectAdd:    nil,
			expectSkip:   true,
		},
		{
			name: "Node deleted - not in k8s - remove from pacemaker",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectRemove: []string{"master-1"},
			expectAdd:    nil,
			expectSkip:   false,
		},
		{
			name: "Node added - in k8s but not pacemaker - add to pacemaker",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
			},
			expectRemove: nil,
			expectAdd:    []string{"master-1"},
			expectSkip:   false,
		},
		{
			name: "No changes - k8s and pacemaker match (name+IP)",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectRemove: nil,
			expectAdd:    nil,
			expectSkip:   true,
		},
		{
			name: "Node replacement - different names - remove old and add new",
			k8sNodes: map[string]string{
				"master-0":     "192.168.1.10",
				"new-master-1": "192.168.1.20",
			},
			pacemakerNodes: map[string]string{
				"master-0":     "192.168.1.10",
				"old-master-1": "192.168.1.11",
			},
			expectRemove: []string{"old-master-1"},
			expectAdd:    []string{"new-master-1"},
			expectSkip:   false,
		},
		{
			name: "Node replacement - same name but IP changed - remove and re-add",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.20", // IP changed from .11 to .20
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11", // Old IP
			},
			expectRemove: []string{"master-1"},
			expectAdd:    []string{"master-1"},
			expectSkip:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Call the actual reconciliation logic from runner.go
			nodesToRemove, nodesToAdd := reconcileNodes(tt.k8sNodes, tt.pacemakerNodes)

			// Verify expectations
			if tt.expectSkip {
				require.Empty(t, nodesToRemove, "Expected no node removal")
				require.Empty(t, nodesToAdd, "Expected no node addition")
			} else {
				require.Equal(t, tt.expectRemove, nodesToRemove,
					"Expected to remove %v but got %v", tt.expectRemove, nodesToRemove)
				require.Equal(t, tt.expectAdd, nodesToAdd,
					"Expected to add %v but got %v", tt.expectAdd, nodesToAdd)
			}
		})
	}
}

// TestIPExtractionFromPeerURLs tests that we correctly extract IPs from etcd peer URLs
// for both IPv4 and IPv6 addresses
func TestIPExtractionFromPeerURLs(t *testing.T) {
	tests := []struct {
		name       string
		peerURLs   []string
		expectedIP string
		shouldFail bool
	}{
		{
			name:       "IPv4 with port",
			peerURLs:   []string{"https://192.168.1.10:2380"},
			expectedIP: "192.168.1.10",
			shouldFail: false,
		},
		{
			name:       "IPv6 with port - SplitHostPort removes brackets",
			peerURLs:   []string{"https://[2001:db8::1]:2380"},
			expectedIP: "2001:db8::1",
			shouldFail: false,
		},
		{
			name:       "IPv6 without port - manual bracket removal",
			peerURLs:   []string{"https://[2001:db8::1]"},
			expectedIP: "2001:db8::1",
			shouldFail: false,
		},
		{
			name:       "Invalid URL",
			peerURLs:   []string{"not-a-url"},
			expectedIP: "",
			shouldFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Extract IP using the same logic as removeUnstartedEtcdMembers
			var memberIP string
			for _, peerURLStr := range tt.peerURLs {
				parsedURL, err := url.Parse(peerURLStr)
				if err != nil {
					continue
				}

				host, _, err := net.SplitHostPort(parsedURL.Host)
				if err != nil {
					// No port in URL - use host directly and strip IPv6 brackets if present
					host = parsedURL.Host
					if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
						host = host[1 : len(host)-1]
					}
				}
				// SplitHostPort already removes brackets for IPv6 when successful

				// Validate that we got a valid IP
				if net.ParseIP(host) == nil {
					continue
				}

				memberIP = host
				break
			}

			if tt.shouldFail {
				require.Empty(t, memberIP, "Expected IP extraction to fail but got: %s", memberIP)
			} else {
				require.Equal(t, tt.expectedIP, memberIP, "IP extraction mismatch")

				// Verify the extracted IP is valid
				ip := net.ParseIP(memberIP)
				require.NotNil(t, ip, "Extracted IP %q should be valid", memberIP)
			}
		})
	}
}

func TestUpdateSetupConfigMapSelectsNode(t *testing.T) {
	require.True(t, updateSetupConfigMapSelectsNode(&corev1.ConfigMap{
		Data: map[string]string{"targetNode": "master-0"},
	}, "master-0"))
	require.False(t, updateSetupConfigMapSelectsNode(&corev1.ConfigMap{
		Data: map[string]string{"targetNode": "master-1"},
	}, "master-0"))
	require.True(t, updateSetupConfigMapSelectsNode(&corev1.ConfigMap{
		Data: map[string]string{"targetNodes": "master-0,master-1"},
	}, "master-1"))
	require.False(t, updateSetupConfigMapSelectsNode(&corev1.ConfigMap{
		Data: map[string]string{"targetNodes": "master-1,master-2"},
	}, "master-0"))
}

func TestPcsClusterNodeRemoveOutputContains401(t *testing.T) {
	require.True(t, pcsClusterNodeRemoveOutputContains401("HTTP 401 Unauthorized", ""))
	require.True(t, pcsClusterNodeRemoveOutputContains401("", "Error: status code 401 for"))
	require.False(t, pcsClusterNodeRemoveOutputContains401("", "Unable to authenticate against node"))
	require.False(t, pcsClusterNodeRemoveOutputContains401("", "connection refused"))
	require.False(t, pcsClusterNodeRemoveOutputContains401("", "node is not in the cluster"))
}

func TestPickUpdateSetupConfigMapForNode(t *testing.T) {
	items := []corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "old"},
			Data: map[string]string{
				"generation": "3", "targetNode": "master-0", "eventType": "add",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "new"},
			Data: map[string]string{
				"generation": "7", "targetNode": "master-0", "eventType": "delete",
			},
		},
	}
	cm, err := pickUpdateSetupConfigMapForNode(items, "master-0")
	require.NoError(t, err)
	require.Equal(t, "new", cm.Name)
	require.Equal(t, "7", cm.Data["generation"])
}
