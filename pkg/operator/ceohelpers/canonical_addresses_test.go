package ceohelpers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewCanonicalAddress(t *testing.T) {
	scenarios := []struct {
		name              string
		address           corev1.NodeAddress
		expectedCanonical string
	}{
		{
			name: "IPv4 standard",
			address: corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: "192.168.1.1",
			},
			expectedCanonical: "192.168.1.1",
		},
		{
			name: "IPv6 compressed",
			address: corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: "2001:db8::1",
			},
			expectedCanonical: "2001:db8::1",
		},
		{
			name: "IPv6 expanded",
			address: corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: "2001:0db8:0000:0000:0000:0000:0000:0001",
			},
			expectedCanonical: "2001:db8::1",
		},
		{
			name: "IPv6 localhost expanded",
			address: corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: "0:0:0:0:0:0:0:1",
			},
			expectedCanonical: "::1",
		},
		{
			name: "IPv4-mapped IPv6",
			address: corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: "::ffff:192.0.2.1",
			},
			expectedCanonical: "192.0.2.1",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			canonical := NewCanonicalAddress(scenario.address)
			assert.Equal(t, scenario.expectedCanonical, canonical.CanonicalAddress)
			assert.Equal(t, scenario.address.Address, canonical.Address)
			assert.Equal(t, scenario.address.Type, canonical.Type)
		})
	}
}

func TestGetCanonicalInternalIPs(t *testing.T) {
	scenarios := []struct {
		name        string
		node        *corev1.Node
		expectedIPs []string
	}{
		{
			name: "single IPv4",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
						{Type: corev1.NodeHostName, Address: "node1"},
					},
				},
			},
			expectedIPs: []string{"192.168.1.1"},
		},
		{
			name: "dual stack",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
						{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
						{Type: corev1.NodeHostName, Address: "node1"},
					},
				},
			},
			expectedIPs: []string{"192.168.1.1", "2001:db8::1"},
		},
		{
			name: "IPv6 with expansion canonicalization",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "2001:0db8:0000:0000:0000:0000:0000:0001"},
						{Type: corev1.NodeHostName, Address: "node1"},
					},
				},
			},
			expectedIPs: []string{"2001:db8::1"},
		},
		{
			name: "filters out non-internal addresses",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
						{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
						{Type: corev1.NodeHostName, Address: "node1"},
					},
				},
			},
			expectedIPs: []string{"192.168.1.1"},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			ips := GetCanonicalInternalIPs(scenario.node)
			assert.Equal(t, scenario.expectedIPs, ips)
		})
	}
}

func TestGetCanonicalInternalIPsFromMachine(t *testing.T) {
	scenarios := []struct {
		name        string
		addresses   []corev1.NodeAddress
		expectedIPs []string
	}{
		{
			name: "single IPv4",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
				{Type: corev1.NodeHostName, Address: "machine1"},
			},
			expectedIPs: []string{"192.168.1.1"},
		},
		{
			name: "dual stack with canonicalization",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
				{Type: corev1.NodeInternalIP, Address: "0:0:0:0:0:0:0:1"},
			},
			expectedIPs: []string{"192.168.1.1", "::1"},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			ips := GetCanonicalInternalIPsFromMachine(scenario.addresses)
			assert.Equal(t, scenario.expectedIPs, ips)
		})
	}
}

func TestHasCanonicalInternalIP(t *testing.T) {
	scenarios := []struct {
		name     string
		node     *corev1.Node
		checkIP  string
		expected bool
	}{
		{
			name: "exact match IPv4",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
					},
				},
			},
			checkIP:  "192.168.1.1",
			expected: true,
		},
		{
			name: "IPv6 canonical matches expanded",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "::1"},
					},
				},
			},
			checkIP:  "0:0:0:0:0:0:0:1",
			expected: true,
		},
		{
			name: "IPv6 expanded matches canonical",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "2001:0db8:0000:0000:0000:0000:0000:0001"},
					},
				},
			},
			checkIP:  "2001:db8::1",
			expected: true,
		},
		{
			name: "no match",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
					},
				},
			},
			checkIP:  "192.168.1.2",
			expected: false,
		},
		{
			name: "ignores external IPs",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{
						{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
					},
				},
			},
			checkIP:  "203.0.113.1",
			expected: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := HasCanonicalInternalIP(scenario.node, scenario.checkIP)
			assert.Equal(t, scenario.expected, result)
		})
	}
}

func TestHasCanonicalInternalIPInMachine(t *testing.T) {
	scenarios := []struct {
		name      string
		addresses []corev1.NodeAddress
		checkIP   string
		expected  bool
	}{
		{
			name: "exact match",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
			},
			checkIP:  "192.168.1.1",
			expected: true,
		},
		{
			name: "canonical match",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "0:0:0:0:0:0:0:1"},
			},
			checkIP:  "::1",
			expected: true,
		},
		{
			name: "no match",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
			},
			checkIP:  "192.168.1.2",
			expected: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := HasCanonicalInternalIPInMachine(scenario.addresses, scenario.checkIP)
			assert.Equal(t, scenario.expected, result)
		})
	}
}

func TestIPsEqual(t *testing.T) {
	scenarios := []struct {
		name     string
		ip1      string
		ip2      string
		expected bool
	}{
		{
			name:     "IPv4 exact match",
			ip1:      "192.168.1.1",
			ip2:      "192.168.1.1",
			expected: true,
		},
		{
			name:     "IPv6 canonical and expanded",
			ip1:      "::1",
			ip2:      "0:0:0:0:0:0:0:1",
			expected: true,
		},
		{
			name:     "IPv6 different compressions",
			ip1:      "2001:db8::1",
			ip2:      "2001:0db8:0000:0000:0000:0000:0000:0001",
			expected: true,
		},
		{
			name:     "IPv4-mapped IPv6 to IPv4",
			ip1:      "::ffff:192.0.2.1",
			ip2:      "192.0.2.1",
			expected: true,
		},
		{
			name:     "different IPs",
			ip1:      "192.168.1.1",
			ip2:      "192.168.1.2",
			expected: false,
		},
		{
			name:     "different IPv6",
			ip1:      "2001:db8::1",
			ip2:      "2001:db8::2",
			expected: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := IPsEqual(scenario.ip1, scenario.ip2)
			assert.Equal(t, scenario.expected, result)
		})
	}
}

func TestGetCanonicalAddresses(t *testing.T) {
	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
		{Type: corev1.NodeInternalIP, Address: "0:0:0:0:0:0:0:1"},
		{Type: corev1.NodeHostName, Address: "node1"},
	}

	canonical := GetCanonicalAddresses(addresses)

	require.Len(t, canonical, 3)
	assert.Equal(t, "192.168.1.1", canonical[0].Address)
	assert.Equal(t, "192.168.1.1", canonical[0].CanonicalAddress)
	assert.Equal(t, "0:0:0:0:0:0:0:1", canonical[1].Address)
	assert.Equal(t, "::1", canonical[1].CanonicalAddress)
	assert.Equal(t, "node1", canonical[2].Address)
	assert.Equal(t, "node1", canonical[2].CanonicalAddress)
}

// TestCanonicalAddressPreservesOriginal verifies that the original address
// is preserved alongside the canonical form, which is important for backwards
// compatibility and debugging.
func TestCanonicalAddressPreservesOriginal(t *testing.T) {
	original := corev1.NodeAddress{
		Type:    corev1.NodeInternalIP,
		Address: "2001:0db8:0000:0000:0000:0000:0000:0001",
	}

	canonical := NewCanonicalAddress(original)

	assert.Equal(t, "2001:0db8:0000:0000:0000:0000:0000:0001", canonical.Address,
		"Original address should be preserved")
	assert.Equal(t, "2001:db8::1", canonical.CanonicalAddress,
		"Canonical form should be compressed")
	assert.NotEqual(t, canonical.Address, canonical.CanonicalAddress,
		"This test verifies they differ to ensure both are useful")
}

// TestRealWorldScenario simulates the etcd member removal scenario
// where an existing etcd cluster has a member with a non-canonical IP
func TestRealWorldScenario(t *testing.T) {
	// Simulate an etcd member that was registered with expanded IPv6
	etcdMemberIP := "0:0:0:0:0:0:0:1"

	// Simulate a node that reports canonical IPv6
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "::1"},
			},
		},
	}

	// Without canonicalization, these wouldn't match
	assert.NotEqual(t, etcdMemberIP, node.Status.Addresses[0].Address,
		"Raw strings don't match")

	// With canonicalization, they match
	assert.True(t, HasCanonicalInternalIP(node, etcdMemberIP),
		"Canonical comparison matches")

	assert.True(t, IPsEqual(etcdMemberIP, node.Status.Addresses[0].Address),
		"IPsEqual helper works")
}
