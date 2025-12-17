package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsNodeReady(t *testing.T) {
	tests := []struct {
		name       string
		node       *corev1.Node
		wantResult bool
	}{
		{
			name: "Node with Ready condition True",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: true,
		},
		{
			name: "Node with Ready condition False",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Node with Ready condition Unknown",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionUnknown,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Node with no conditions",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{},
				},
			},
			wantResult: false,
		},
		{
			name: "Node with other conditions but no Ready condition",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeMemoryPressure,
							Status: corev1.ConditionFalse,
						},
						{
							Type:   corev1.NodeDiskPressure,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Node with Ready condition True and other conditions",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeMemoryPressure,
							Status: corev1.ConditionFalse,
						},
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.NodeDiskPressure,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsNodeReady(tt.node)
			require.Equal(t, tt.wantResult, result,
				"Expected IsNodeReady() = %v, got %v", tt.wantResult, result)
		})
	}
}

func TestGetNodeIPForPacemaker(t *testing.T) {
	tests := []struct {
		name          string
		node          corev1.Node
		expectedIP    string
		expectError   bool
		errorContains string
	}{
		{
			name: "Node with internal IPv4 address",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.1.10",
						},
					},
				},
			},
			expectedIP:  "192.168.1.10",
			expectError: false,
		},
		{
			name: "Node with internal IPv6 address",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "2001:db8::1",
						},
					},
				},
			},
			expectedIP:  "2001:db8::1",
			expectError: false,
		},
		{
			name: "Node with multiple addresses - internal IP first",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.1.10",
						},
						{
							Type:    corev1.NodeExternalIP,
							Address: "203.0.113.10",
						},
						{
							Type:    corev1.NodeHostName,
							Address: "master-0.example.com",
						},
					},
				},
			},
			expectedIP:  "192.168.1.10",
			expectError: false,
		},
		{
			name: "Node with multiple addresses - internal IP not first",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeExternalIP,
							Address: "203.0.113.10",
						},
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.1.10",
						},
						{
							Type:    corev1.NodeHostName,
							Address: "master-0.example.com",
						},
					},
				},
			},
			expectedIP:  "192.168.1.10",
			expectError: false,
		},
		{
			name: "Node with multiple internal IPs - returns first valid one",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.1.10",
						},
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.1.11",
						},
					},
				},
			},
			expectedIP:  "192.168.1.10",
			expectError: false,
		},
		{
			name: "Node with no internal IP - uses first address as fallback",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeExternalIP,
							Address: "203.0.113.10",
						},
						{
							Type:    corev1.NodeHostName,
							Address: "master-0.example.com",
						},
					},
				},
			},
			expectedIP:  "203.0.113.10",
			expectError: false,
		},
		{
			name: "Node with no addresses - returns error",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{},
				},
			},
			expectedIP:    "",
			expectError:   true,
			errorContains: "has no configured address",
		},
		{
			name: "Node with invalid internal IP then valid internal IP - returns first valid",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "invalid-ip",
						},
						{
							Type:    corev1.NodeInternalIP,
							Address: "192.168.1.10",
						},
						{
							Type:    corev1.NodeExternalIP,
							Address: "203.0.113.10",
						},
					},
				},
			},
			expectedIP:  "192.168.1.10",
			expectError: false,
		},
		{
			name: "Node with internal IP and IPv4-mapped IPv6 address",
			node: corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{
							Type:    corev1.NodeInternalIP,
							Address: "::ffff:192.168.1.10",
						},
					},
				},
			},
			expectedIP:  "192.168.1.10", // net.ParseIP normalizes IPv4-mapped IPv6 to IPv4
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, err := GetNodeIPForPacemaker(tt.node)

			if tt.expectError {
				require.Error(t, err, "Expected error but got none")
				if tt.errorContains != "" {
					require.Contains(t, err.Error(), tt.errorContains,
						"Expected error to contain %q but got: %v", tt.errorContains, err)
				}
			} else {
				require.NoError(t, err, "Expected no error but got: %v", err)
				require.Equal(t, tt.expectedIP, ip,
					"Expected IP %q but got %q", tt.expectedIP, ip)
			}
		})
	}
}
