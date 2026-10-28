package dnshelpers

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTryCanonicalizeIP(t *testing.T) {
	scenarios := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid IPv4",
			input:    "192.168.1.10",
			expected: "192.168.1.10",
		},
		{
			name:     "valid IPv6 compressed",
			input:    "2001:db8::1",
			expected: "2001:db8::1",
		},
		{
			name:     "IPv6 with leading zeros",
			input:    "2001:0db8:0000:0000:0000:0000:0000:0001",
			expected: "2001:db8::1",
		},
		{
			name:     "IPv6 mixed case",
			input:    "2001:DB8::1",
			expected: "2001:db8::1",
		},
		{
			name:     "IPv4-mapped IPv6 converts to IPv4",
			input:    "::ffff:192.0.2.1",
			expected: "192.0.2.1",
		},
		{
			name:     "IPv4-mapped IPv6 with zeros converts to IPv4",
			input:    "::ffff:c000:0201",
			expected: "192.0.2.1",
		},
		{
			name:     "IPv6 loopback",
			input:    "::1",
			expected: "::1",
		},
		{
			name:     "IPv4 loopback",
			input:    "127.0.0.1",
			expected: "127.0.0.1",
		},
		{
			name:     "IPv6 with redundant zeros",
			input:    "fe80:0000:0000:0000:0202:b3ff:fe1e:8329",
			expected: "fe80::202:b3ff:fe1e:8329",
		},
		{
			name:     "IPv6 full form",
			input:    "2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			expected: "2001:db8:85a3::8a2e:370:7334",
		},
		{
			name:     "IPv4 with leading zeros",
			input:    "192.000.002.001",
			expected: "192.0.2.1",
		},
		{
			name:     "IPv4 with mixed leading zeros",
			input:    "010.000.002.100",
			expected: "10.0.2.100",
		},
		{
			name:     "IPv4 all zeros",
			input:    "000.000.000.000",
			expected: "0.0.0.0",
		},
		{
			name:     "IPv4 with all leading zeros in last octet",
			input:    "192.168.1.001",
			expected: "192.168.1.1",
		},
		// Anything that is not a valid IP address is returned unchanged.
		{
			name:     "invalid IP",
			input:    "not-an-ip",
			expected: "not-an-ip",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "IPv4 with too few octets",
			input:    "192.168.1",
			expected: "192.168.1",
		},
		{
			name:     "IPv4 with too many octets",
			input:    "192.168.1.1.1",
			expected: "192.168.1.1.1",
		},
		{
			name:     "IPv4 with invalid characters",
			input:    "192.168.1.a",
			expected: "192.168.1.a",
		},
		{
			name:     "IPv4 with empty octet",
			input:    "192.168..1",
			expected: "192.168..1",
		},
		{
			name:     "IPv4 with out-of-range octet",
			input:    "192.168.1.999",
			expected: "192.168.1.999",
		},
		{
			name:     "IPv4 with out-of-range octet padded with zeros",
			input:    "192.168.1.0999",
			expected: "192.168.1.0999",
		},
		{
			name:     "IPv4 with signed octet",
			input:    "192.168.1.+1",
			expected: "192.168.1.+1",
		},
		{
			name:     "IPv4 with CIDR suffix",
			input:    "192.168.1.1/24",
			expected: "192.168.1.1/24",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			actual := TryCanonicalizeIP(scenario.input)
			require.Equal(t, scenario.expected, actual)

			// Canonicalization has to be idempotent, both for an address it
			// rewrites and for one it passes through untouched.
			require.Equal(t, scenario.expected, TryCanonicalizeIP(actual))
		})
	}
}

func TestGetInternalIPAddresses(t *testing.T) {
	scenarios := []struct {
		name        string
		addresses   []corev1.NodeAddress
		expectedRaw []string
		expectedCan []string
	}{
		{
			name: "single IPv4",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
				{Type: corev1.NodeHostName, Address: "node1"},
			},
			expectedRaw: []string{"192.168.1.1"},
			expectedCan: []string{"192.168.1.1"},
		},
		{
			name: "dual stack",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
				{Type: corev1.NodeInternalIP, Address: "0:0:0:0:0:0:0:1"},
				{Type: corev1.NodeHostName, Address: "node1"},
			},
			expectedRaw: []string{"192.168.1.1", "0:0:0:0:0:0:0:1"},
			expectedCan: []string{"192.168.1.1", "::1"},
		},
		{
			name: "IPv6 with expansion canonicalization",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "2001:0db8:0000:0000:0000:0000:0000:0001"},
			},
			expectedRaw: []string{"2001:0db8:0000:0000:0000:0000:0000:0001"},
			expectedCan: []string{"2001:db8::1"},
		},
		{
			name: "filters out non-internal addresses",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
				{Type: corev1.NodeHostName, Address: "node1"},
			},
			expectedRaw: []string{"192.168.1.1"},
			expectedCan: []string{"192.168.1.1"},
		},
		{
			name: "skips empty addresses",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ""},
				{Type: corev1.NodeInternalIP, Address: "192.168.1.1"},
			},
			expectedRaw: []string{"192.168.1.1"},
			expectedCan: []string{"192.168.1.1"},
		},
		{
			name:        "no addresses",
			addresses:   nil,
			expectedRaw: []string{},
			expectedCan: []string{},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// The raw form has to be preserved, serving certificates are issued
			// for the address exactly as the node reports it.
			require.Equal(t, scenario.expectedRaw, GetInternalIPAddresses(scenario.addresses))
			require.Equal(t, scenario.expectedCan, GetCanonicalInternalIPAddresses(scenario.addresses))
		})
	}
}

func TestGetInternalIPAddressesForNodeName(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "2001:0db8:0000:0000:0000:0000:0000:0001"},
				{Type: corev1.NodeHostName, Address: "master-0"},
			},
		},
	}
	addresses, err := GetInternalIPAddressesForNodeName(node)
	require.NoError(t, err)
	require.Equal(t, []string{"2001:0db8:0000:0000:0000:0000:0000:0001"}, addresses)

	_, err = GetInternalIPAddressesForNodeName(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-1"}})
	require.Error(t, err)
}
