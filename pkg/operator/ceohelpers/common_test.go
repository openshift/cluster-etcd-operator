package ceohelpers

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	withWellKnownReplicasCountSet = `
{
 "controlPlane": {"replicas": 3}
}
`

	withReplicasCountSetInUnsupportedConfig = `
{
 "controlPlane": {"replicas": 7}
}
`
)

func TestReadDesiredControlPlaneReplicaCount(t *testing.T) {
	scenarios := []struct {
		name                             string
		operatorSpec                     operatorv1.StaticPodOperatorSpec
		expectedControlPlaneReplicaCount int
		expectedError                    error
	}{
		{
			name: "with replicas set in the observed config only",
			operatorSpec: operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					ObservedConfig: runtime.RawExtension{Raw: []byte(withWellKnownReplicasCountSet)},
				},
			},
			expectedControlPlaneReplicaCount: 3,
		},

		{
			name: "with replicas set in the UnsupportedConfigOverrides",
			operatorSpec: operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					ObservedConfig:             runtime.RawExtension{Raw: []byte(withWellKnownReplicasCountSet)},
					UnsupportedConfigOverrides: runtime.RawExtension{Raw: []byte(withReplicasCountSetInUnsupportedConfig)},
				},
			},
			expectedControlPlaneReplicaCount: 7,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(&scenario.operatorSpec, u.StaticPodOperatorStatus(), nil, nil)

			// act
			actualReplicaCount, err := ReadDesiredControlPlaneReplicasCount(fakeOperatorClient)

			// validate
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from readDesiredControlPlaneReplicasCount function")
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatal(err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if actualReplicaCount != scenario.expectedControlPlaneReplicaCount {
				t.Fatalf("unexpected control plance replicat count: %d, expected: %d", actualReplicaCount, scenario.expectedControlPlaneReplicaCount)
			}
		})
	}
}

func TestRevisionRolloutInProgress(t *testing.T) {
	scenarios := []struct {
		name     string
		status   operatorv1.StaticPodOperatorStatus
		expected bool
	}{
		{
			name: "revs equal single node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 1},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 1, TargetRevision: 1},
				},
			},
			expected: false,
		},
		{
			name: "revs not equal single node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 1, TargetRevision: 3},
				},
			},
			expected: true,
		},
		{
			name: "revs not equal multi node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 3, TargetRevision: 3},
					{NodeName: "node-2", CurrentRevision: 1, TargetRevision: 3},
				},
			},
			expected: true,
		},
		{
			name: "revs equal multi node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 3, TargetRevision: 3},
					{NodeName: "node-2", CurrentRevision: 3, TargetRevision: 3},
				},
			},
			expected: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			require.Equal(t, scenario.expected, RevisionRolloutInProgress(scenario.status))
		})
	}
}

func TestCurrentRevision(t *testing.T) {
	scenarios := []struct {
		name        string
		status      operatorv1.StaticPodOperatorStatus
		expected    int32
		expectedErr error
	}{
		{
			name: "revs equal single node",
			status: operatorv1.StaticPodOperatorStatus{
				NodeStatuses: []operatorv1.NodeStatus{},
			},
			expectedErr: errors.New("no node status"),
		},
		{
			name: "revs equal single node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 22},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 22, TargetRevision: 22},
				},
			},
			expected: 22,
		},
		{
			name: "revs not equal single node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 1, TargetRevision: 3},
				},
			},
			expected:    0,
			expectedErr: RevisionRolloutInProgressErr,
		},
		{
			name: "target revs not equal multi node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 3, TargetRevision: 3},
					{NodeName: "node-2", CurrentRevision: 1, TargetRevision: 3},
				},
			},
			expected:    0,
			expectedErr: RevisionRolloutInProgressErr,
		},
		{
			name: "revs equal multi node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 3, TargetRevision: 0},
					{NodeName: "node-2", CurrentRevision: 3, TargetRevision: 0},
				},
			},
			expected: 3,
		},
		{
			name: "revs differ multi node",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 3},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 3, TargetRevision: 3},
					{NodeName: "node-2", CurrentRevision: 2, TargetRevision: 2},
				},
			},
			expected:    0,
			expectedErr: errors.New("revision rollout in progress, can't establish current revision"),
		},
		{
			name: "latest rev far ahead",
			status: operatorv1.StaticPodOperatorStatus{
				OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 25},
				NodeStatuses: []operatorv1.NodeStatus{
					{NodeName: "node-1", CurrentRevision: 3, TargetRevision: 3},
				},
			},
			expected:    0,
			expectedErr: errors.New("revision rollout in progress, can't establish current revision"),
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			revision, err := CurrentRevision(scenario.status)
			require.Equal(t, scenario.expectedErr, err)
			require.Equal(t, scenario.expected, revision)
		})
	}
}

func TestCanonicalizeIP(t *testing.T) {
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
			name:     "invalid IP returns original",
			input:    "not-an-ip",
			expected: "not-an-ip",
		},
		{
			name:     "empty string returns original",
			input:    "",
			expected: "",
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
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			actual := dnshelpers.CanonicalizeIP(scenario.input)
			require.Equal(t, scenario.expected, actual)
		})
	}
}

func TestIndexMachinesByNodeInternalIP(t *testing.T) {
	scenarios := []struct {
		name                      string
		machines                  []*machinev1beta1.Machine
		expectedIndexKeys         []string
		expectedMachineNameForKey map[string]string
	}{
		{
			name: "single machine with IPv4",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"192.168.1.10"},
			expectedMachineNameForKey: map[string]string{
				"192.168.1.10": "machine1",
			},
		},
		{
			name: "single machine with IPv6 expanded form - should use canonical key only",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "2001:0db8:0000:0000:0000:0000:0000:0001"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"2001:db8::1"},
			expectedMachineNameForKey: map[string]string{
				"2001:db8::1": "machine1",
			},
		},
		{
			name: "single machine with dual-stack IPs",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
							{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"192.168.1.10", "2001:db8::1"},
			expectedMachineNameForKey: map[string]string{
				"192.168.1.10": "machine1",
				"2001:db8::1":  "machine1",
			},
		},
		{
			name: "multiple machines with different IPs",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine2"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.20"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"192.168.1.10", "192.168.1.20"},
			expectedMachineNameForKey: map[string]string{
				"192.168.1.10": "machine1",
				"192.168.1.20": "machine2",
			},
		},
		{
			name: "machine with multiple network interfaces (same machine, multiple IPs)",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "10.0.1.10"},
							{Type: corev1.NodeInternalIP, Address: "10.0.2.10"},
							{Type: corev1.NodeHostName, Address: "node1.example.com"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"10.0.1.10", "10.0.2.10"},
			expectedMachineNameForKey: map[string]string{
				"10.0.1.10": "machine1",
				"10.0.2.10": "machine1",
			},
		},
		{
			name: "IPv4-mapped IPv6 should convert to IPv4",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "::ffff:192.0.2.1"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"192.0.2.1"},
			expectedMachineNameForKey: map[string]string{
				"192.0.2.1": "machine1",
			},
		},
		{
			name: "filters out non-internal IP addresses",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
							{Type: corev1.NodeExternalIP, Address: "1.2.3.4"},
							{Type: corev1.NodeHostName, Address: "node1.example.com"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"192.168.1.10"},
			expectedMachineNameForKey: map[string]string{
				"192.168.1.10": "machine1",
			},
		},
		{
			name: "empty address should be filtered out",
			machines: []*machinev1beta1.Machine{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "machine1"},
					Status: machinev1beta1.MachineStatus{
						Addresses: []corev1.NodeAddress{
							{Type: corev1.NodeInternalIP, Address: ""},
							{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
						},
					},
				},
			},
			expectedIndexKeys: []string{"192.168.1.10"},
			expectedMachineNameForKey: map[string]string{
				"192.168.1.10": "machine1",
			},
		},
		{
			name:                      "empty machine list",
			machines:                  []*machinev1beta1.Machine{},
			expectedIndexKeys:         []string{},
			expectedMachineNameForKey: map[string]string{},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			index := IndexMachinesByNodeInternalIP(scenario.machines)

			// Verify the number of keys matches expected
			require.Equal(t, len(scenario.expectedIndexKeys), len(index),
				"index should have exactly %d entries, but has %d",
				len(scenario.expectedIndexKeys), len(index))

			// Verify each expected key exists and points to the correct machine
			for ip, expectedMachineName := range scenario.expectedMachineNameForKey {
				machine, exists := index[ip]
				require.True(t, exists, "expected IP %s to exist in index", ip)
				require.NotNil(t, machine, "machine should not be nil for IP %s", ip)
				require.Equal(t, expectedMachineName, machine.Name,
					"machine name mismatch for IP %s", ip)
			}

			// Verify no unexpected keys exist
			for ip := range index {
				_, expected := scenario.expectedMachineNameForKey[ip]
				require.True(t, expected, "unexpected IP %s found in index", ip)
			}
		})
	}
}
