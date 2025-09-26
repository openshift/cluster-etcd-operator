package ceohelpers

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
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
			result, err := IsReadyForEtcdTransition(context.Background(), operatorClient)

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

func TestGetExternalEtcdClusterStatus(t *testing.T) {
	tests := []struct {
		name                         string
		topology                     configv1.TopologyMode
		operatorConditions           []operatorv1.OperatorCondition
		expectedExternalEtcd         bool
		expectedEtcdRunningInCluster bool
		expectedReadyForTransition   bool
		expectError                  bool
	}{
		{
			name:                         "HighlyAvailable topology - not external etcd",
			topology:                     configv1.HighlyAvailableTopologyMode,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedExternalEtcd:         false,
			expectedEtcdRunningInCluster: false,
			expectedReadyForTransition:   false,
			expectError:                  false,
		},
		{
			name:                         "DualReplica topology with no conditions",
			topology:                     configv1.DualReplicaTopologyMode,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedExternalEtcd:         true,
			expectedEtcdRunningInCluster: false,
			expectedReadyForTransition:   false,
			expectError:                  false,
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
			expectedExternalEtcd:         true,
			expectedEtcdRunningInCluster: true,
			expectedReadyForTransition:   false,
			expectError:                  false,
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
			expectedExternalEtcd:         true,
			expectedEtcdRunningInCluster: true,
			expectedReadyForTransition:   true,
			expectError:                  false,
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
				require.Equal(t, tt.expectedExternalEtcd, externalEtcdStatus.IsExternalEtcdCluster)
				require.Equal(t, tt.expectedEtcdRunningInCluster, externalEtcdStatus.IsEtcdRunningInCluster)
				require.Equal(t, tt.expectedReadyForTransition, externalEtcdStatus.IsReadyForEtcdTransition)
			}
		})
	}
}
