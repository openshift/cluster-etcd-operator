package ceohelpers

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"

	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
)

func TestIsExternalEtcdCluster(t *testing.T) {
	tests := []struct {
		name           string
		topology       configv1.TopologyMode
		expectedResult bool
		expectError    bool
	}{
		{
			name:           "HighlyAvailable topology - not external etcd",
			topology:       configv1.HighlyAvailableTopologyMode,
			expectedResult: false,
			expectError:    false,
		},
		{
			name:           "DualReplica topology - external etcd",
			topology:       configv1.DualReplicaTopologyMode,
			expectedResult: true,
			expectError:    false,
		},
		{
			name:           "SingleReplica topology - not external etcd",
			topology:       configv1.SingleReplicaTopologyMode,
			expectedResult: false,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake infrastructure lister
			infraLister := testutils.FakeInfrastructureLister(t, tt.topology)

			// Test the function
			result, err := IsExternalEtcdCluster(context.Background(), infraLister)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedResult, result)
			}
		})
	}
}

func TestIsReadyForEtcdTransition(t *testing.T) {
	tests := []struct {
		name               string
		operatorConditions []operatorv1.OperatorCondition
		expectedResult     bool
		expectError        bool
	}{
		{
			name:               "No conditions - not ready",
			operatorConditions: []operatorv1.OperatorCondition{},
			expectedResult:     false,
			expectError:        false,
		},
		{
			name: "ExternalEtcdReadyForTransition condition true - ready",
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name: "ExternalEtcdReadyForTransition condition false - not ready",
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
					Status: operatorv1.ConditionFalse,
				},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name: "Other conditions present but not ExternalEtcdReadyForTransition - not ready",
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedResult: false,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake operator client
			operatorClient := testutils.FakeStaticPodOperatorClient(t, tt.operatorConditions)

			// Test the function
			result, err := IsReadyForEtcdTransition(operatorClient)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedResult, result)
			}
		})
	}
}

func TestIsEtcdRunningInCluster(t *testing.T) {
	tests := []struct {
		name               string
		operatorConditions []operatorv1.OperatorCondition
		expectedResult     bool
		expectError        bool
	}{
		{
			name:               "No conditions - not completed",
			operatorConditions: []operatorv1.OperatorCondition{},
			expectedResult:     false,
			expectError:        false,
		},
		{
			name: "EtcdRunningInCluster condition true - completed",
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name: "EtcdRunningInCluster condition false - not completed",
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionFalse,
				},
			},
			expectedResult: false,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake operator client
			operatorClient := testutils.FakeStaticPodOperatorClient(t, tt.operatorConditions)

			// Test the function
			result, err := IsEtcdRunningInCluster(context.Background(), operatorClient)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedResult, result)
			}
		})
	}
}

func TestHasExternalEtcdCompletedTransition(t *testing.T) {
	tests := []struct {
		name           string
		operatorStatus func(*operatorv1.StaticPodOperatorStatus)
		expectedResult bool
		expectError    bool
	}{
		{
			name:           "No conditions - transition not completed",
			operatorStatus: nil,
			expectedResult: false,
			expectError:    false,
		},
		{
			name: "ExternalEtcdHasCompletedTransition condition true - transition completed",
			operatorStatus: testutils.WithConditions(operatorv1.OperatorCondition{
				Type:   etcd.OperatorConditionExternalEtcdHasCompletedTransition,
				Status: operatorv1.ConditionTrue,
			}),
			expectedResult: true,
			expectError:    false,
		},
		{
			name: "ExternalEtcdHasCompletedTransition condition false - transition not completed",
			operatorStatus: testutils.WithConditions(operatorv1.OperatorCondition{
				Type:   etcd.OperatorConditionExternalEtcdHasCompletedTransition,
				Status: operatorv1.ConditionFalse,
			}),
			expectedResult: false,
			expectError:    false,
		},
		{
			name: "Other conditions present but not ExternalEtcdHasCompletedTransition - transition not completed",
			operatorStatus: testutils.WithConditions(
				operatorv1.OperatorCondition{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				},
				operatorv1.OperatorCondition{
					Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
					Status: operatorv1.ConditionTrue,
				},
			),
			expectedResult: false,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake operator client
			var operatorClient v1helpers.StaticPodOperatorClient
			if tt.operatorStatus != nil {
				operatorClient = v1helpers.NewFakeStaticPodOperatorClient(
					&operatorv1.StaticPodOperatorSpec{},
					testutils.StaticPodOperatorStatus(tt.operatorStatus),
					nil,
					nil,
				)
			} else {
				operatorClient = testutils.FakeStaticPodOperatorClient(t, []operatorv1.OperatorCondition{})
			}

			// Test the function
			result, err := HasExternalEtcdCompletedTransition(context.Background(), operatorClient)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedResult, result)
			}
		})
	}
}

func TestGetExternalEtcdClusterStatus(t *testing.T) {
	tests := []struct {
		name                           string
		topology                       configv1.TopologyMode
		operatorConditions             []operatorv1.OperatorCondition
		expectedExternalEtcd           bool
		expectedEtcdRunningInCluster   bool
		expectedReadyForTransition     bool
		expectedHasCompletedTransition bool
		expectError                    bool
	}{
		{
			name:                           "HighlyAvailable topology - not external etcd",
			topology:                       configv1.HighlyAvailableTopologyMode,
			operatorConditions:             []operatorv1.OperatorCondition{},
			expectedExternalEtcd:           false,
			expectedEtcdRunningInCluster:   false,
			expectedReadyForTransition:     false,
			expectedHasCompletedTransition: false,
			expectError:                    false,
		},
		{
			name:                           "DualReplica topology with no conditions",
			topology:                       configv1.DualReplicaTopologyMode,
			operatorConditions:             []operatorv1.OperatorCondition{},
			expectedExternalEtcd:           true,
			expectedEtcdRunningInCluster:   false,
			expectedReadyForTransition:     false,
			expectedHasCompletedTransition: false,
			expectError:                    false,
		},
		{
			name:     "DualReplica topology with bootstrap completed",
			topology: configv1.DualReplicaTopologyMode,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedExternalEtcd:           true,
			expectedEtcdRunningInCluster:   true,
			expectedReadyForTransition:     false,
			expectedHasCompletedTransition: false,
			expectError:                    false,
		},
		{
			name:     "DualReplica topology with bootstrap completed and ready for transition",
			topology: configv1.DualReplicaTopologyMode,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				},
				{
					Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedExternalEtcd:           true,
			expectedEtcdRunningInCluster:   true,
			expectedReadyForTransition:     true,
			expectedHasCompletedTransition: false,
			expectError:                    false,
		},
		{
			name:     "DualReplica topology with transition completed",
			topology: configv1.DualReplicaTopologyMode,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				},
				{
					Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
					Status: operatorv1.ConditionTrue,
				},
				{
					Type:   etcd.OperatorConditionExternalEtcdHasCompletedTransition,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedExternalEtcd:           true,
			expectedEtcdRunningInCluster:   true,
			expectedReadyForTransition:     true,
			expectedHasCompletedTransition: true,
			expectError:                    false,
		},
		{
			name:     "DualReplica topology with only transition completed (full lifecycle)",
			topology: configv1.DualReplicaTopologyMode,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdHasCompletedTransition,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedExternalEtcd:           true,
			expectedEtcdRunningInCluster:   false,
			expectedReadyForTransition:     false,
			expectedHasCompletedTransition: true,
			expectError:                    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake infrastructure lister
			infraLister := testutils.FakeInfrastructureLister(t, tt.topology)

			// Create fake operator client
			operatorClient := testutils.FakeStaticPodOperatorClient(t, tt.operatorConditions)

			// Test the function
			externalEtcdStatus, err := GetExternalEtcdClusterStatus(
				context.Background(), operatorClient, infraLister)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedExternalEtcd, externalEtcdStatus.IsExternalEtcdCluster,
					"IsExternalEtcdCluster mismatch")
				require.Equal(t, tt.expectedEtcdRunningInCluster, externalEtcdStatus.IsEtcdRunningInCluster,
					"IsEtcdRunningInCluster mismatch")
				require.Equal(t, tt.expectedReadyForTransition, externalEtcdStatus.IsReadyForEtcdTransition,
					"IsReadyForEtcdTransition mismatch")
				require.Equal(t, tt.expectedHasCompletedTransition, externalEtcdStatus.HasExternalEtcdCompletedTransition,
					"HasExternalEtcdCompletedTransition mismatch")
			}
		})
	}
}

// TestExternalEtcdTransitionLifecycle validates the complete state progression
// for external etcd clusters from initial state through transition completion.
func TestExternalEtcdTransitionLifecycle(t *testing.T) {
	// State 1: Initial state - external etcd cluster detected, no bootstrap yet
	t.Run("State 1: External etcd cluster detected", func(t *testing.T) {
		infraLister := testutils.FakeInfrastructureLister(t, configv1.DualReplicaTopologyMode)
		operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
			&operatorv1.StaticPodOperatorSpec{},
			testutils.StaticPodOperatorStatus(),
			nil,
			nil,
		)

		status, err := GetExternalEtcdClusterStatus(context.Background(), operatorClient, infraLister)
		require.NoError(t, err)
		require.True(t, status.IsExternalEtcdCluster, "Should detect external etcd cluster")
		require.False(t, status.IsEtcdRunningInCluster, "Bootstrap should not be complete")
		require.False(t, status.IsReadyForEtcdTransition, "Should not be ready for transition")
		require.False(t, status.HasExternalEtcdCompletedTransition, "Transition should not be complete")
	})

	// State 2: Bootstrap completed - etcd running in cluster
	t.Run("State 2: Bootstrap completed", func(t *testing.T) {
		infraLister := testutils.FakeInfrastructureLister(t, configv1.DualReplicaTopologyMode)
		operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
			&operatorv1.StaticPodOperatorSpec{},
			testutils.StaticPodOperatorStatus(
				testutils.WithConditions(operatorv1.OperatorCondition{
					Type:   etcd.OperatorConditionEtcdRunningInCluster,
					Status: operatorv1.ConditionTrue,
				}),
			),
			nil,
			nil,
		)

		status, err := GetExternalEtcdClusterStatus(context.Background(), operatorClient, infraLister)
		require.NoError(t, err)
		require.True(t, status.IsExternalEtcdCluster, "Should detect external etcd cluster")
		require.True(t, status.IsEtcdRunningInCluster, "Bootstrap should be complete")
		require.False(t, status.IsReadyForEtcdTransition, "Should not yet be ready for transition")
		require.False(t, status.HasExternalEtcdCompletedTransition, "Transition should not be complete")
	})

	// State 3: Ready for transition - TNF setup complete
	t.Run("State 3: Ready for transition", func(t *testing.T) {
		infraLister := testutils.FakeInfrastructureLister(t, configv1.DualReplicaTopologyMode)
		operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
			&operatorv1.StaticPodOperatorSpec{},
			testutils.StaticPodOperatorStatus(
				testutils.WithConditions(
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionEtcdRunningInCluster,
						Status: operatorv1.ConditionTrue,
					},
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
						Status: operatorv1.ConditionTrue,
					},
				),
			),
			nil,
			nil,
		)

		status, err := GetExternalEtcdClusterStatus(context.Background(), operatorClient, infraLister)
		require.NoError(t, err)
		require.True(t, status.IsExternalEtcdCluster, "Should detect external etcd cluster")
		require.True(t, status.IsEtcdRunningInCluster, "Bootstrap should be complete")
		require.True(t, status.IsReadyForEtcdTransition, "Should be ready for transition")
		require.False(t, status.HasExternalEtcdCompletedTransition, "Transition should not yet be complete")
	})

	// State 4: Transition completed - etcd running externally under pacemaker
	t.Run("State 4: Transition completed", func(t *testing.T) {
		infraLister := testutils.FakeInfrastructureLister(t, configv1.DualReplicaTopologyMode)
		operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
			&operatorv1.StaticPodOperatorSpec{},
			testutils.StaticPodOperatorStatus(
				testutils.WithConditions(
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionEtcdRunningInCluster,
						Status: operatorv1.ConditionTrue,
					},
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
						Status: operatorv1.ConditionTrue,
					},
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionExternalEtcdHasCompletedTransition,
						Status: operatorv1.ConditionTrue,
					},
				),
			),
			nil,
			nil,
		)

		status, err := GetExternalEtcdClusterStatus(context.Background(), operatorClient, infraLister)
		require.NoError(t, err)
		require.True(t, status.IsExternalEtcdCluster, "Should detect external etcd cluster")
		require.True(t, status.IsEtcdRunningInCluster, "Bootstrap should remain complete")
		require.True(t, status.IsReadyForEtcdTransition, "Should remain ready for transition")
		require.True(t, status.HasExternalEtcdCompletedTransition, "Transition should be complete")
	})

	// Negative test: Transition not complete, no automatic quorum recovery
	t.Run("Negative: No auto recovery without transition completion", func(t *testing.T) {
		operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
			&operatorv1.StaticPodOperatorSpec{},
			testutils.StaticPodOperatorStatus(
				testutils.WithConditions(
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionEtcdRunningInCluster,
						Status: operatorv1.ConditionTrue,
					},
					operatorv1.OperatorCondition{
						Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
						Status: operatorv1.ConditionTrue,
					},
				),
			),
			nil,
			nil,
		)

		// Test that HasExternalEtcdCompletedTransition returns false when transition is not complete
		hasCompletedTransition, err := HasExternalEtcdCompletedTransition(context.Background(), operatorClient)
		require.NoError(t, err)
		require.False(t, hasCompletedTransition, "Should not report transition as completed before condition is set")
	})
}
