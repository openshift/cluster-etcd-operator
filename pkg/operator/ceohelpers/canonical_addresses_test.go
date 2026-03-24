package ceohelpers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHasCanonicalInternalIP(t *testing.T) {
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
			name: "expanded IPv6 matches canonical",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "2001:0db8:0000:0000:0000:0000:0000:0001"},
			},
			checkIP:  "2001:db8::1",
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
		{
			name: "ignores external IPs",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			checkIP:  "203.0.113.1",
			expected: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := HasCanonicalInternalIP(scenario.addresses, scenario.checkIP)
			assert.Equal(t, scenario.expected, result)
		})
	}
}

func TestCanonicalIPsEqual(t *testing.T) {
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
			result := CanonicalIPsEqual(scenario.ip1, scenario.ip2)
			assert.Equal(t, scenario.expected, result)
		})
	}
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
	assert.True(t, HasCanonicalInternalIP(node.Status.Addresses, etcdMemberIP),
		"Canonical comparison matches")

	assert.True(t, CanonicalIPsEqual(etcdMemberIP, node.Status.Addresses[0].Address),
		"CanonicalIPsEqual helper works")
}
