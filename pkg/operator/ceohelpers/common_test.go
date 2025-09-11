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
