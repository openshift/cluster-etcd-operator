package ceohelpers

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime"

	operatorv1 "github.com/openshift/api/operator/v1"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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

func TestIPAddressesEqual(t *testing.T) {
	tests := []struct {
		name     string
		ip1      string
		ip2      string
		expected bool
	}{
		{
			name:     "IPv4 addresses equal",
			ip1:      "192.168.1.10",
			ip2:      "192.168.1.10",
			expected: true,
		},
		{
			name:     "IPv4 addresses not equal",
			ip1:      "192.168.1.10",
			ip2:      "192.168.1.11",
			expected: false,
		},
		{
			name:     "IPv6 addresses equal - same format",
			ip1:      "fd00::1",
			ip2:      "fd00::1",
			expected: true,
		},
		{
			name:     "IPv6 addresses equal - different format",
			ip1:      "fd00::1",
			ip2:      "fd00:0000:0000:0000:0000:0000:0000:0001",
			expected: true,
		},
		{
			name:     "IPv6 addresses not equal",
			ip1:      "fd00::1",
			ip2:      "fd00::2",
			expected: false,
		},
		{
			name:     "IPv6 addresses equal - expanded vs compressed",
			ip1:      "fd00:0000:0000:0000:0000:0000:0000:0002",
			ip2:      "fd00::2",
			expected: true,
		},
		{
			name:     "IPv4 vs IPv6 not equal",
			ip1:      "192.168.1.10",
			ip2:      "fd00::1",
			expected: false,
		},
		{
			name:     "empty strings equal",
			ip1:      "",
			ip2:      "",
			expected: true,
		},
		{
			name:     "empty vs non-empty not equal",
			ip1:      "",
			ip2:      "192.168.1.10",
			expected: false,
		},
		{
			name:     "invalid IP addresses - string comparison fallback (equal)",
			ip1:      "not-an-ip",
			ip2:      "not-an-ip",
			expected: true,
		},
		{
			name:     "invalid IP addresses - string comparison fallback (not equal)",
			ip1:      "not-an-ip",
			ip2:      "different-not-an-ip",
			expected: false,
		},
		{
			name:     "IPv4-mapped IPv6 addresses",
			ip1:      "::ffff:192.168.1.10",
			ip2:      "192.168.1.10",
			expected: true, // IPv4-mapped IPv6 represents the same address
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IPAddressesEqual(tt.ip1, tt.ip2)
			require.Equal(t, tt.expected, result, "IPAddressesEqual(%q, %q) = %v, expected %v", tt.ip1, tt.ip2, result, tt.expected)
		})
	}
}
