package operator

import (
	"testing"

	"github.com/openshift/api/etcd/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFindModifiedNode(t *testing.T) {
	tests := []struct {
		name             string
		pacemakerCluster *v1alpha1.PacemakerCluster
		nodeList         []*corev1.Node
		expectedNodeName string
		expectedNodeIP   string
		expectError      bool
	}{
		{
			name: "Node with IP address mismatch",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10", // Old IP
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
						{
							Name:         "master-1",
							IPAddress:    "192.168.1.11",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.20"}, // New IP
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.11"}, // Same IP
						},
					},
				},
			},
			expectedNodeName: "master-0",
			expectedNodeIP:   "192.168.1.20",
		},
		{
			name: "Node offline in Pacemaker - same IP",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10",
							OnlineStatus: v1alpha1.NodeOnlineStatusOffline, // Offline
						},
						{
							Name:         "master-1",
							IPAddress:    "192.168.1.11",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"}, // Same IP
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.11"}, // Same IP
						},
					},
				},
			},
			expectedNodeName: "master-0",
			expectedNodeIP:   "192.168.1.10",
		},
		{
			name: "Both nodes match and online - no modified nodes",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
						{
							Name:         "master-1",
							IPAddress:    "192.168.1.11",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.11"},
						},
					},
				},
			},
			expectedNodeName: "", // No modified node
			expectedNodeIP:   "",
		},
		{
			name: "Both nodes have mismatches - error",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
						{
							Name:         "master-1",
							IPAddress:    "192.168.1.11",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.20"}, // Different
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.21"}, // Different
						},
					},
				},
			},
			expectError: true,
		},
		{
			name: "Both nodes offline - error",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10",
							OnlineStatus: v1alpha1.NodeOnlineStatusOffline,
						},
						{
							Name:         "master-1",
							IPAddress:    "192.168.1.11",
							OnlineStatus: v1alpha1.NodeOnlineStatusOffline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.11"},
						},
					},
				},
			},
			expectError: true,
		},
		{
			name: "Nil PacemakerCluster status nodes - error",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: nil,
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
			},
			expectError: true,
		},
		{
			name: "K8s node not in Pacemaker - modified",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.11"},
						},
					},
				},
			},
			expectedNodeName: "master-1",
			expectedNodeIP:   "192.168.1.11",
		},
		{
			name: "Pacemaker has extra node - no modified k8s nodes",
			pacemakerCluster: &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{
							Name:         "master-0",
							IPAddress:    "192.168.1.10",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
						{
							Name:         "master-1",
							IPAddress:    "192.168.1.11",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
						{
							Name:         "master-old",
							IPAddress:    "192.168.1.99",
							OnlineStatus: v1alpha1.NodeOnlineStatusOnline,
						},
					},
				},
			},
			nodeList: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-1"},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.11"},
						},
					},
				},
			},
			expectedNodeName: "",
			expectedNodeIP:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName, nodeIP, err := findModifiedNode(tt.pacemakerCluster, tt.nodeList)

			if tt.expectError {
				require.Error(t, err, "Expected error but got none")
				return
			}

			require.NoError(t, err, "Expected no error but got: %v", err)
			require.Equal(t, tt.expectedNodeName, nodeName,
				"Expected node name '%s', got '%s'", tt.expectedNodeName, nodeName)
			require.Equal(t, tt.expectedNodeIP, nodeIP,
				"Expected node IP '%s', got '%s'", tt.expectedNodeIP, nodeIP)
		})
	}
}
