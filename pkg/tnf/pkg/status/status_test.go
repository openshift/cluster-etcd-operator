package status

import (
	"context"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
)

func TestNewClusterStatus(t *testing.T) {
	ctx := context.Background()
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		&operatorv1.StaticPodOperatorStatus{},
		nil,
		nil,
	)

	testCases := []struct {
		name                  string
		isDualReplicaTopology bool
		bootstrapCompleted    bool
		readyForEtcdRemoval   bool
	}{
		{
			name:                  "dual replica enabled, bootstrap not completed",
			isDualReplicaTopology: true,
			bootstrapCompleted:    false,
			readyForEtcdRemoval:   false,
		},
		{
			name:                  "dual replica enabled, bootstrap completed",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			readyForEtcdRemoval:   false,
		},
		{
			name:                  "dual replica disabled, bootstrap not completed",
			isDualReplicaTopology: false,
			bootstrapCompleted:    false,
			readyForEtcdRemoval:   false,
		},
		{
			name:                  "dual replica disabled, bootstrap completed",
			isDualReplicaTopology: false,
			bootstrapCompleted:    true,
			readyForEtcdRemoval:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewClusterStatus(ctx, operatorClient, tc.isDualReplicaTopology, tc.bootstrapCompleted, tc.readyForEtcdRemoval)
			if cs == nil {
				t.Fatal("NewClusterStatus returned nil")
			}

			// Test that the cluster status implements the interface correctly
			if cs.IsDualReplicaTopology() != tc.isDualReplicaTopology {
				t.Errorf("expected IsDualReplicaTopology()=%v, got %v", tc.isDualReplicaTopology, cs.IsDualReplicaTopology())
			}

			// For disabled dual replica, bootstrap completion should be irrelevant
			if !tc.isDualReplicaTopology {
				if cs.IsBootstrapCompleted() {
					t.Errorf("expected IsBootstrapCompleted()=false for disabled dual replica topology, got true")
				}
				if cs.IsReadyForEtcdRemoval() {
					t.Errorf("expected IsReadyForEtcdRemoval()=false for disabled dual replica topology, got true")
				}
			}
		})
	}
}

func TestClusterStatusGetStatus(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name                  string
		isDualReplicaTopology bool
		bootstrapCompleted    bool
		operatorConditions    []operatorv1.OperatorCondition
		expectedStatus        DualReplicaClusterStatus
	}{
		{
			name:                  "dual replica disabled",
			isDualReplicaTopology: false,
			bootstrapCompleted:    false,
			operatorConditions:    nil,
			expectedStatus:        DualReplicaClusterStatusDisabled,
		},
		{
			name:                  "dual replica enabled, bootstrap not completed",
			isDualReplicaTopology: true,
			bootstrapCompleted:    false,
			operatorConditions:    nil,
			expectedStatus:        DualReplicaClusterStatusEnabled,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, operator condition not set",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			operatorConditions:    nil,
			expectedStatus:        DualReplicaClusterStatusBootstrapCompleted,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, operator condition false",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReady,
					Status: operatorv1.ConditionFalse,
				},
			},
			expectedStatus: DualReplicaClusterStatusBootstrapCompleted,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, operator condition true",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReady,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedStatus: DualReplicaClusterStatusSetupReadyForEtcdRemoval,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				&operatorv1.StaticPodOperatorStatus{
					OperatorStatus: operatorv1.OperatorStatus{
						Conditions: tc.operatorConditions,
					},
				},
				nil,
				nil,
			)

			cs := NewClusterStatus(ctx, operatorClient, tc.isDualReplicaTopology, tc.bootstrapCompleted, false).(*clusterStatus)

			status := cs.GetStatus()
			if status != tc.expectedStatus {
				t.Errorf("expected status %v, got %v", tc.expectedStatus, status)
			}
		})
	}
}

func TestClusterStatusMethods(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name                        string
		isDualReplicaTopology       bool
		bootstrapCompleted          bool
		operatorConditions          []operatorv1.OperatorCondition
		expectedIsDualReplica       bool
		expectedIsBootstrapComplete bool
		expectedIsReadyForRemoval   bool
	}{
		{
			name:                        "dual replica disabled",
			isDualReplicaTopology:       false,
			bootstrapCompleted:          false,
			operatorConditions:          nil,
			expectedIsDualReplica:       false,
			expectedIsBootstrapComplete: false,
			expectedIsReadyForRemoval:   false,
		},
		{
			name:                        "dual replica enabled, bootstrap not completed",
			isDualReplicaTopology:       true,
			bootstrapCompleted:          false,
			operatorConditions:          nil,
			expectedIsDualReplica:       true,
			expectedIsBootstrapComplete: false,
			expectedIsReadyForRemoval:   false,
		},
		{
			name:                        "dual replica enabled, bootstrap completed, not ready",
			isDualReplicaTopology:       true,
			bootstrapCompleted:          true,
			operatorConditions:          nil,
			expectedIsDualReplica:       true,
			expectedIsBootstrapComplete: true,
			expectedIsReadyForRemoval:   false,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, ready for removal",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReady,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedIsDualReplica:       true,
			expectedIsBootstrapComplete: true,
			expectedIsReadyForRemoval:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				&operatorv1.StaticPodOperatorStatus{
					OperatorStatus: operatorv1.OperatorStatus{
						Conditions: tc.operatorConditions,
					},
				},
				nil,
				nil,
			)

			cs := NewClusterStatus(ctx, operatorClient, tc.isDualReplicaTopology, tc.bootstrapCompleted, false)

			if cs.IsDualReplicaTopology() != tc.expectedIsDualReplica {
				t.Errorf("expected IsDualReplicaTopology()=%v, got %v", tc.expectedIsDualReplica, cs.IsDualReplicaTopology())
			}

			if cs.IsBootstrapCompleted() != tc.expectedIsBootstrapComplete {
				t.Errorf("expected IsBootstrapCompleted()=%v, got %v", tc.expectedIsBootstrapComplete, cs.IsBootstrapCompleted())
			}

			if cs.IsReadyForEtcdRemoval() != tc.expectedIsReadyForRemoval {
				t.Errorf("expected IsReadyForEtcdRemoval()=%v, got %v", tc.expectedIsReadyForRemoval, cs.IsReadyForEtcdRemoval())
			}
		})
	}
}

func TestSetBootstrapCompleted(t *testing.T) {
	ctx := context.Background()
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		&operatorv1.StaticPodOperatorStatus{},
		nil,
		nil,
	)

	// Test with dual replica enabled initially, bootstrap not completed
	cs := NewClusterStatus(ctx, operatorClient, true, false, false)

	if cs.IsBootstrapCompleted() {
		t.Fatal("expected IsBootstrapCompleted()=false initially")
	}

	cs.SetBootstrapCompleted()

	if !cs.IsBootstrapCompleted() {
		t.Fatal("expected IsBootstrapCompleted()=true after SetBootstrapCompleted()")
	}
}

func TestClusterStatusReadyTransition(t *testing.T) {
	ctx := context.Background()

	operatorStatus := &operatorv1.StaticPodOperatorStatus{
		OperatorStatus: operatorv1.OperatorStatus{
			Conditions: nil,
		},
	}

	// Create operator client without the ready condition initially
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		operatorStatus,
		nil,
		nil,
	)

	cs := NewClusterStatus(ctx, operatorClient, true, true, false).(*clusterStatus)

	// First call should return bootstrap completed status
	status := cs.GetStatus()
	if status != DualReplicaClusterStatusBootstrapCompleted {
		t.Errorf("expected status %v, got %v", DualReplicaClusterStatusBootstrapCompleted, status)
	}

	// Verify readyForEtcdRemoval is false
	if cs.readyForEtcdRemoval {
		t.Error("expected readyForEtcdRemoval to be false before successful ready check")
	}

	// Now update the operator client to have the ready condition
	operatorStatus.OperatorStatus.Conditions = []operatorv1.OperatorCondition{
		{
			Type:   etcd.OperatorConditionExternalEtcdReady,
			Status: operatorv1.ConditionTrue,
		},
	}

	// Should now be ready
	status = cs.GetStatus()
	if status != DualReplicaClusterStatusSetupReadyForEtcdRemoval {
		t.Errorf("expected status %v, got %v", DualReplicaClusterStatusSetupReadyForEtcdRemoval, status)
	}

	// Verify readyForEtcdRemoval is now true
	if !cs.readyForEtcdRemoval {
		t.Error("expected readyForEtcdRemoval to be true after successful ready check")
	}
}
