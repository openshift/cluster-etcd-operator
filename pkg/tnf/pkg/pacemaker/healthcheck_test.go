package pacemaker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/internal/testutil"
)

// Test helper functions

// createFakeOperatorClient creates a fake static pod operator client for testing
func createFakeOperatorClient() v1helpers.StaticPodOperatorClient {
	return v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		&operatorv1.StaticPodOperatorStatus{
			OperatorStatus: operatorv1.OperatorStatus{
				Conditions: []operatorv1.OperatorCondition{},
			},
		},
		nil,
		nil,
	)
}

func createTestHealthCheck() *HealthCheck {
	return &HealthCheck{
		operatorClient:    createFakeOperatorClient(),
		kubeClient:        fake.NewSimpleClientset(),
		eventRecorder:     events.NewInMemoryRecorder("test", clock.RealClock{}),
		pacemakerInformer: testutil.CreateFakeInformer(nil),
		recordedEvents:    make(map[string]time.Time),
	}
}

// createTestHealthCheckWithMockStatus creates a HealthCheck with a mocked Pacemaker CR in the informer cache
func createTestHealthCheckWithMockStatus(t *testing.T, mockStatus *pacmkrv1.PacemakerCluster) *HealthCheck {
	return &HealthCheck{
		operatorClient:    createFakeOperatorClient(),
		kubeClient:        fake.NewSimpleClientset(),
		eventRecorder:     events.NewInMemoryRecorder("test", clock.RealClock{}),
		pacemakerInformer: testutil.CreateFakeInformer(mockStatus),
		recordedEvents:    make(map[string]time.Time),
	}
}

// =============================================================================
// Unit Tests
// =============================================================================
// Note: Test fixtures are defined in test_fixtures_test.go

func TestNewHealthCheck(t *testing.T) {
	controller, informer, err := NewHealthCheck(
		createFakeOperatorClient(),
		fake.NewSimpleClientset(),
		events.NewInMemoryRecorder("test", clock.RealClock{}),
		&rest.Config{Host: "https://localhost:6443"},
		false,
	)

	require.NoError(t, err, "NewHealthCheck should not return an error")
	require.NotNil(t, controller, "Controller should not be nil")
	require.NotNil(t, informer, "Informer should not be nil")
}

func TestHealthCheck_getPacemakerStatus(t *testing.T) {
	tests := []struct {
		name           string
		mockStatus     *pacmkrv1.PacemakerCluster
		expectedStatus HealthStatusValue
		expectErrors   bool
		expectWarnings bool
	}{
		{
			name: "healthy_status",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: StatusHealthy,
			expectErrors:   false,
			expectWarnings: false,
		},
		{
			name: "stale_status",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // > 5 min threshold
				},
			},
			expectedStatus: StatusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "nil_status",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{}, // Status not populated yet (zero LastUpdated)
			},
			expectedStatus: StatusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "insufficient_nodes",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createInsufficientNodesClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "unhealthy_kubelet",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameKubelet),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "unhealthy_etcd",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "unhealthy_etcd_first_node",
			mockStatus: &pacmkrv1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: pacmkrv1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			expectWarnings: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := createTestHealthCheckWithMockStatus(t, tt.mockStatus)
			ctx := context.TODO()

			current, _, err := controller.getPacemakerStatus(ctx)

			require.NoError(t, err, "getPacemakerStatus should not return an error")
			require.NotNil(t, current, "HealthStatus should not be nil")
			require.Equal(t, tt.expectedStatus, current.OverallStatus, "Status should match expected")

			if tt.expectErrors {
				require.NotEmpty(t, current.Errors, "Should have errors")
			} else {
				require.Empty(t, current.Errors, "Should not have errors")
			}

			if tt.expectWarnings {
				require.NotEmpty(t, current.Warnings, "Should have warnings")
			} else {
				require.Empty(t, current.Warnings, "Should not have warnings")
			}
		})
	}
}

func TestHealthCheck_getPacemakerStatus_UnchangedTimestamp(t *testing.T) {
	// Create a fixed timestamp for the test
	fixedTime := metav1.Now()

	mockStatus := &pacmkrv1.PacemakerCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: pacmkrv1.SchemeGroupVersion.String(),
			Kind:       "PacemakerCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: pacmkrv1.PacemakerClusterStatus{
			Conditions: createHealthyClusterConditions(),
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			LastUpdated: fixedTime,
		},
	}

	controller := createTestHealthCheckWithMockStatus(t, mockStatus)
	ctx := context.TODO()

	// First call should return a valid status
	current1, _, err1 := controller.getPacemakerStatus(ctx)
	require.NoError(t, err1, "First getPacemakerStatus should not return an error")
	require.NotNil(t, current1, "First call should return a status")
	require.Equal(t, StatusHealthy, current1.OverallStatus, "First call should return healthy status")

	// Simulate what sync() does after successful updateOperatorStatus
	controller.previous = current1

	// Second call with same timestamp should return nil (no change)
	current2, _, err2 := controller.getPacemakerStatus(ctx)
	require.NoError(t, err2, "Second getPacemakerStatus should not return an error")
	require.Nil(t, current2, "Second call with unchanged timestamp should return nil")
}

// TestHealthCheck_sync_AppliesPendingBlockAtDeadlineWithUnchangedTimestamp covers the case where the
// UpgradeableBlockInertia deadline elapses between collector updates. The inertia is wall-clock, so a
// pending block must be applied on the controller's periodic resync even when the CR's lastUpdated has
// not advanced; otherwise the block is deferred until the next CR write or until the status goes stale.
func TestHealthCheck_sync_AppliesPendingBlockAtDeadlineWithUnchangedTimestamp(t *testing.T) {
	// Fresh, non-stale timestamp that never changes across the two syncs.
	fixedTime := metav1.Now()
	mockStatus := &pacmkrv1.PacemakerCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: pacmkrv1.SchemeGroupVersion.String(),
			Kind:       "PacemakerCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: pacmkrv1.PacemakerClusterStatus{
			Conditions: createHealthyClusterConditions(),
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceName("etcd")),
			},
			LastUpdated: fixedTime,
		},
	}

	controller := createTestHealthCheckWithMockStatus(t, mockStatus)
	controller.disruptionTracker = NewDisruptionTracker()
	ctx := context.TODO()

	// Sync #1: the Error is observed and the inertia clock starts. Upgradeable must remain unblocked
	// (damped) on the first sync, and a pending block must be recorded.
	require.NoError(t, controller.sync(ctx, nil))
	up1, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.True(t, up1 == nil || up1.Status != operatorv1.ConditionFalse,
		"within inertia the first sync must not block upgrades")
	require.False(t, controller.upgradeBlockedSince.IsZero(),
		"a pending upgrade block should have been recorded")

	// The inertia deadline passes WITHOUT the collector advancing lastUpdated.
	controller.previousMu.Lock()
	controller.upgradeBlockedSince = time.Now().Add(-UpgradeableBlockInertia - time.Minute)
	controller.previousMu.Unlock()

	// Sync #2 sees the same (non-stale) lastUpdated. The pending block has now crossed its deadline
	// and must be applied. Before the fix, sync skips on the unchanged timestamp and never reconciles
	// the deadline, leaving Upgradeable unblocked.
	require.NoError(t, controller.sync(ctx, nil))
	up2, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, up2, "upgradeable condition should be set after the deadline")
	require.Equal(t, operatorv1.ConditionFalse, up2.Status,
		"a pending block must be applied at its deadline even when lastUpdated is unchanged")
}

func TestHealthCheck_updateOperatorStatus(t *testing.T) {
	// An empty expected Status ("") means the condition is expected to be absent (untouched).
	tests := []struct {
		name                  string
		status                *HealthStatus
		previous              *HealthStatus
		setup                 func(*HealthCheck) // optional: preset baselines/conditions before the call
		expectedDegraded      operatorv1.ConditionStatus
		expectedUpgradeable   operatorv1.ConditionStatus
		expectedUpgradeReason string // checked only when expectedUpgradeable is False
	}{
		{
			// Degraded is set immediately, but Upgradeable is damped: a single Error sync must
			// not flap the Upgradeable union (library-go applies no inertia there).
			name: "error_first_sync_sets_degraded_but_damps_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusError,
				Warnings:      []string{"Test warning"},
				Errors:        []string{"Test error"},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
			expectedDegraded:    operatorv1.ConditionTrue,
			expectedUpgradeable: "",
		},
		{
			name: "error_persisted_past_inertia_blocks_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusError,
				Warnings:      []string{"Test warning"},
				Errors:        []string{"Test error"},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
			// The block has already persisted longer than the inertia window.
			setup: func(c *HealthCheck) {
				c.upgradeBlockedSince = time.Now().Add(-UpgradeableBlockInertia - time.Minute)
			},
			expectedDegraded:      operatorv1.ConditionTrue,
			expectedUpgradeable:   operatorv1.ConditionFalse,
			expectedUpgradeReason: reasonPacemakerUnhealthy,
		},
		{
			name: "healthy_status_clears_degraded_and_allows_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
			expectedDegraded:    operatorv1.ConditionFalse,
			expectedUpgradeable: operatorv1.ConditionTrue,
		},
		{
			name: "warning_status_clears_degraded_and_allows_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusWarning,
				Warnings:      []string{"Test warning"},
				Errors:        []string{},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
			expectedDegraded:    operatorv1.ConditionFalse,
			expectedUpgradeable: operatorv1.ConditionTrue,
		},
		{
			// OCPBUGS-85833 review clarification: fencing that is available but not fully
			// managed is a Warning and must NOT block upgrades; only fencing being unavailable
			// (an Error) blocks. Even with the inertia window long elapsed, a Warning never blocks.
			name: "warning_fencing_degraded_never_blocks_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusWarning,
				Warnings:      []string{"master-0: " + msgFencingDegraded},
				Errors:        []string{},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
			setup: func(c *HealthCheck) {
				c.upgradeBlockedSince = time.Now().Add(-UpgradeableBlockInertia - time.Minute)
			},
			expectedDegraded:    operatorv1.ConditionFalse,
			expectedUpgradeable: operatorv1.ConditionTrue,
		},
		{
			name: "unknown_within_grace_leaves_conditions_untouched",
			status: &HealthStatus{
				OverallStatus: StatusUnknown,
				Warnings:      []string{},
				Errors:        []string{"CR not found"},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				// Recent enough to stay within StatusUnknownDegradedThreshold.
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
			expectedDegraded:    "",
			expectedUpgradeable: "",
		},
		{
			// Prolonged Unknown is exempt from the Upgradeable inertia: the grace period already
			// provides the anti-flap damping, so it blocks on the first sync past the threshold.
			name: "unknown_past_threshold_sets_degraded_and_blocks_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusUnknown,
				Warnings:      []string{},
				Errors:        []string{"CR not found"},
			},
			previous: &HealthStatus{
				OverallStatus: StatusHealthy,
				// Older than StatusUnknownDegradedThreshold (5m) so the grace period has elapsed.
				CRLastUpdated: time.Now().Add(-10 * time.Minute),
			},
			expectedDegraded:      operatorv1.ConditionTrue,
			expectedUpgradeable:   operatorv1.ConditionFalse,
			expectedUpgradeReason: reasonPacemakerUnhealthy,
		},
		{
			// Blocking #1: after a controller restart `previous` is nil, but a stale CR still
			// carries its lastUpdated timestamp. The grace period must still expire so a persisted
			// Upgradeable=True does not survive indefinitely while health is indeterminate.
			name: "unknown_restart_stale_cr_past_threshold_blocks_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusUnknown,
				Warnings:      []string{},
				Errors:        []string{"Pacemaker status is stale"},
				CRLastUpdated: time.Now().Add(-10 * time.Minute),
			},
			previous:              nil,
			expectedDegraded:      operatorv1.ConditionTrue,
			expectedUpgradeable:   operatorv1.ConditionFalse,
			expectedUpgradeReason: reasonPacemakerUnhealthy,
		},
		{
			// Restart with a missing CR (no timestamp anywhere): the in-memory baseline is seeded
			// to "now", so the first sync stays within grace and leaves conditions untouched.
			name: "unknown_restart_missing_cr_within_grace_leaves_untouched",
			status: &HealthStatus{
				OverallStatus: StatusUnknown,
				Warnings:      []string{},
				Errors:        []string{"PacemakerCluster CR not found in cache"},
			},
			previous:            nil,
			expectedDegraded:    "",
			expectedUpgradeable: "",
		},
		{
			// Restart with a missing CR but the in-memory baseline already elapsed (controller has
			// been unable to determine health across restarts for longer than the threshold).
			name: "unknown_restart_missing_cr_expired_baseline_blocks_upgrade",
			status: &HealthStatus{
				OverallStatus: StatusUnknown,
				Warnings:      []string{},
				Errors:        []string{"PacemakerCluster CR not found in cache"},
			},
			previous: nil,
			setup: func(c *HealthCheck) {
				c.unknownSince = time.Now().Add(-10 * time.Minute)
			},
			expectedDegraded:      operatorv1.ConditionTrue,
			expectedUpgradeable:   operatorv1.ConditionFalse,
			expectedUpgradeReason: reasonPacemakerUnhealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := createTestHealthCheck()
			ctx := context.TODO()

			if tt.setup != nil {
				tt.setup(controller)
			}

			err := controller.updateOperatorStatus(ctx, tt.status, tt.previous)
			require.NoError(t, err, "updateOperatorStatus should not return an error")

			degraded, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerDegraded)
			require.NoError(t, err)
			if tt.expectedDegraded == "" {
				require.Nil(t, degraded, "degraded condition should be untouched")
			} else {
				require.NotNil(t, degraded, "degraded condition should be set")
				require.Equal(t, tt.expectedDegraded, degraded.Status, "degraded condition status")
			}

			upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
			require.NoError(t, err)
			if tt.expectedUpgradeable == "" {
				require.Nil(t, upgradeable, "upgradeable condition should be untouched")
			} else {
				require.NotNil(t, upgradeable, "upgradeable condition should be set")
				require.Equal(t, tt.expectedUpgradeable, upgradeable.Status, "upgradeable condition status")
				if tt.expectedUpgradeable == operatorv1.ConditionFalse {
					require.NotEmpty(t, upgradeable.Message, "blocking upgradeable condition should carry a message")
					require.Equal(t, tt.expectedUpgradeReason, upgradeable.Reason)
				} else {
					require.Equal(t, "AsExpected", upgradeable.Reason)
				}
			}
		})
	}
}

// TestHealthCheck_updateOperatorStatus_RestartWithPersistedUpgradeableTrue covers blocking #1:
// after a controller restart, `previous` is nil in memory, but a persisted Upgradeable=True must
// not survive indefinitely while pacemaker health cannot be determined. Repeated Unknown
// observations beyond the grace period (here via a stale CR that still carries its lastUpdated
// timestamp) must flip the persisted True to False.
func TestHealthCheck_updateOperatorStatus_RestartWithPersistedUpgradeableTrue(t *testing.T) {
	controller := createTestHealthCheck()
	ctx := context.TODO()

	// Before the restart the cluster was healthy, so Upgradeable=True was persisted.
	require.NoError(t, controller.setPacemakerUpgradeableCondition(ctx, true, "AsExpected", ""))
	persisted, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.Equal(t, operatorv1.ConditionTrue, persisted.Status)

	// Simulate a restart: no in-memory baselines survive.
	controller.previous = nil
	controller.unknownSince = time.Time{}
	controller.upgradeBlockedSince = time.Time{}

	// Repeated Unknown observations: the CR is stale and its last valid timestamp is beyond the
	// grace period. `previous` stays nil because Unknown is never saved as a valid observation.
	staleStatus := &HealthStatus{
		OverallStatus: StatusUnknown,
		Warnings:      []string{},
		Errors:        []string{"Pacemaker status is stale"},
		CRLastUpdated: time.Now().Add(-10 * time.Minute),
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, controller.updateOperatorStatus(ctx, staleStatus, nil))
	}

	upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable)
	require.Equal(t, operatorv1.ConditionFalse, upgradeable.Status, "prolonged Unknown after restart must block upgrades")
	require.Equal(t, reasonPacemakerUnhealthy, upgradeable.Reason)
	require.NotEmpty(t, upgradeable.Message)

	degraded, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerDegraded)
	require.NoError(t, err)
	require.NotNil(t, degraded)
	require.Equal(t, operatorv1.ConditionTrue, degraded.Status, "prolonged Unknown after restart must also degrade")
}

// TestHealthCheck_updateOperatorStatus_TransientErrorDoesNotFlapUpgrade covers blocking #3: a brief
// Error that recovers before the inertia window elapses must never write Upgradeable=False, and
// recovery clears the damping clock so a later error starts a fresh window.
func TestHealthCheck_updateOperatorStatus_TransientErrorDoesNotFlapUpgrade(t *testing.T) {
	controller := createTestHealthCheck()
	ctx := context.TODO()

	previousHealthy := &HealthStatus{OverallStatus: StatusHealthy, CRLastUpdated: time.Now().Add(-1 * time.Minute)}
	errorStatus := &HealthStatus{OverallStatus: StatusError, Errors: []string{"Node master-0 is offline"}}
	healthyStatus := &HealthStatus{OverallStatus: StatusHealthy, Errors: []string{}, Warnings: []string{}}

	// A single Error sync must not flip Upgradeable (inertia not yet satisfied).
	require.NoError(t, controller.updateOperatorStatus(ctx, errorStatus, previousHealthy))
	upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.Nil(t, upgradeable, "a transient Error must not immediately flip Upgradeable to False")
	require.False(t, controller.upgradeBlockedSince.IsZero(), "the damping clock should have started")

	// Recovery clears the block immediately and resets the clock.
	require.NoError(t, controller.updateOperatorStatus(ctx, healthyStatus, previousHealthy))
	upgradeable, err = controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable)
	require.Equal(t, operatorv1.ConditionTrue, upgradeable.Status, "recovery must allow upgrades immediately")
	require.True(t, controller.upgradeBlockedSince.IsZero(), "recovery must reset the damping clock")
}

// TestHealthCheck_NodeNotFenceableBlocksUpgrade pins the rule agreed in review: if either of the
// two TNF nodes is no longer fenceable, upgrades must be blocked. The collector marks such a node
// FencingAvailable=False, FencingHealthy=False and Healthy=False (fencing availability is folded
// into the node's Healthy condition). The health check must turn that into an Error and, once the
// inertia window has elapsed, Upgradeable=False. Only one of the two nodes loses fencing here, so
// this also proves a single unfenceable node is enough to block.
func TestHealthCheck_NodeNotFenceableBlocksUpgrade(t *testing.T) {
	// master-1 lost its fencing: no agent is running, so it cannot be fenced at all.
	node := createHealthyNodeStatus("master-1", []string{"192.168.1.11"})
	node.FencingAgents = nil
	for i := range node.Conditions {
		switch node.Conditions[i].Type {
		case pacmkrv1.NodeFencingAvailableConditionType:
			node.Conditions[i].Status = metav1.ConditionFalse
			node.Conditions[i].Reason = pacmkrv1.NodeFencingAvailableReasonUnavailable
		case pacmkrv1.NodeFencingHealthyConditionType:
			node.Conditions[i].Status = metav1.ConditionFalse
			node.Conditions[i].Reason = pacmkrv1.NodeFencingHealthyReasonUnhealthy
		case pacmkrv1.NodeHealthyConditionType:
			node.Conditions[i].Status = metav1.ConditionFalse
			node.Conditions[i].Reason = pacmkrv1.NodeHealthyReasonUnhealthy
		}
	}

	cr := &pacmkrv1.PacemakerCluster{
		Status: pacmkrv1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Conditions:  createHealthyClusterConditions(),
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				node,
			},
		},
	}

	status := BuildHealthStatusFromCR(cr, false)
	require.Equal(t, StatusError, status.OverallStatus, "a node that cannot be fenced must be an Error")
	errs := strings.Join(status.Errors, "; ")
	require.Contains(t, errs, "fencing unavailable", "the error must name fencing unavailability")
	require.Contains(t, errs, "master-1", "the error must name the node that lost fencing")

	// An Error blocks upgrades once the inertia window has elapsed.
	controller := createTestHealthCheck()
	controller.upgradeBlockedSince = time.Now().Add(-UpgradeableBlockInertia - time.Minute)
	previous := &HealthStatus{OverallStatus: StatusHealthy, CRLastUpdated: time.Now().Add(-time.Minute)}
	require.NoError(t, controller.updateOperatorStatus(context.TODO(), status, previous))

	upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable, "the upgradeable condition must be set")
	require.Equal(t, operatorv1.ConditionFalse, upgradeable.Status,
		"one node losing fenceability must block upgrades")
	require.Equal(t, reasonPacemakerUnhealthy, upgradeable.Reason)
}

// TestHealthCheck_UpgradeBlockSurvivesRestart covers the blocking review comment that the inertia
// timer lived only in memory. A restarted controller observing the same ongoing error must resume
// the original UpgradeableBlockInertia deadline instead of starting a fresh window, which would
// otherwise postpone the block by a full window on every restart. The episode start is persisted
// as the PacemakerHealthCheckDegraded LastTransitionTime.
func TestHealthCheck_UpgradeBlockSurvivesRestart(t *testing.T) {
	tests := []struct {
		name          string
		episodeAge    time.Duration
		expectBlocked bool
	}{
		{"within_original_window", 2 * time.Minute, false},
		{"past_original_deadline", 11 * time.Minute, true},
	}

	errorStatus := &HealthStatus{OverallStatus: StatusError, Errors: []string{"Node master-0 is offline"}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			episodeStart := time.Now().Add(-tt.episodeAge)

			// The previous process persisted Degraded=True when the episode began.
			controller := createTestHealthCheck()
			controller.operatorClient = newOperatorClientWithConditions(operatorv1.OperatorCondition{
				Type:               conditionTypePacemakerDegraded,
				Status:             operatorv1.ConditionTrue,
				Reason:             reasonPacemakerUnhealthy,
				Message:            "Node master-0 is offline",
				LastTransitionTime: metav1.NewTime(episodeStart),
			})
			// A restart carries no in-memory baseline and no previous observation.
			require.True(t, controller.upgradeBlockedSince.IsZero())

			require.NoError(t, controller.updateOperatorStatus(context.TODO(), errorStatus, nil))

			// The resumed baseline is the persisted episode start, not the restart time.
			controller.previousMu.Lock()
			resumed := controller.upgradeBlockedSince
			controller.previousMu.Unlock()
			require.WithinDuration(t, episodeStart, resumed, 2*time.Second,
				"the inertia baseline must resume the persisted episode start across a restart")

			upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
			require.NoError(t, err)
			if tt.expectBlocked {
				require.NotNil(t, upgradeable)
				require.Equal(t, operatorv1.ConditionFalse, upgradeable.Status,
					"an episode older than the inertia window must block immediately after a restart")
			} else {
				require.True(t, upgradeable == nil || upgradeable.Status != operatorv1.ConditionFalse,
					"the remaining part of the original window must still be honoured")
			}
		})
	}
}

// TestHealthCheck_sync_RestoresFastPathAfterBlockApplied covers the non-blocking review comment
// that a nonzero upgradeBlockedSince kept bypassing the unchanged-status optimization forever.
// The bypass must apply only while the block is still pending; once Upgradeable=False is written
// for the episode, an unchanged CR timestamp can be skipped again.
func TestHealthCheck_sync_RestoresFastPathAfterBlockApplied(t *testing.T) {
	fixedTime := metav1.Now()
	mockStatus := &pacmkrv1.PacemakerCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: pacmkrv1.SchemeGroupVersion.String(),
			Kind:       "PacemakerCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: pacmkrv1.PacemakerClusterStatus{
			Conditions: createHealthyClusterConditions(),
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceName("etcd")),
			},
			LastUpdated: fixedTime,
		},
	}

	controller := createTestHealthCheckWithMockStatus(t, mockStatus)
	controller.disruptionTracker = NewDisruptionTracker()
	ctx := context.TODO()

	// Sync #1 observes the Error and starts the damping clock.
	require.NoError(t, controller.sync(ctx, nil))

	// Force the inertia deadline to have elapsed.
	controller.previousMu.Lock()
	controller.upgradeBlockedSince = time.Now().Add(-UpgradeableBlockInertia - time.Minute)
	controller.previousMu.Unlock()

	// Sync #2 applies the block even though lastUpdated did not advance.
	require.NoError(t, controller.sync(ctx, nil))
	upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable)
	require.Equal(t, operatorv1.ConditionFalse, upgradeable.Status)
	require.True(t, controller.upgradeBlockApplied, "the applied block must be recorded")

	// Sync #3 sees the same lastUpdated. With the block already written, the unchanged-status fast
	// path must be restored instead of re-evaluating on every periodic resync.
	current, _, err := controller.getPacemakerStatus(ctx)
	require.NoError(t, err)
	require.Nil(t, current, "the unchanged-status fast path must be restored once the block is applied")
}

// TestHealthCheck_sync_RetriesPersistedEpisodeStartAfterReadFailure covers the remaining restart
// case: when the persisted Degraded condition cannot be read, the lookup must not cache a fallback
// "now" anchor, because that would lose the original deadline for the rest of the episode. The next
// sync must retry the read and honour the original deadline.
func TestHealthCheck_sync_RetriesPersistedEpisodeStartAfterReadFailure(t *testing.T) {
	fixedTime := metav1.Now()
	mockStatus := &pacmkrv1.PacemakerCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: pacmkrv1.SchemeGroupVersion.String(),
			Kind:       "PacemakerCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: pacmkrv1.PacemakerClusterStatus{
			Conditions: createHealthyClusterConditions(),
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceName("etcd")),
			},
			LastUpdated: fixedTime,
		},
	}

	// The episode began past the inertia deadline before the restart, and the previous process
	// persisted Degraded=True at that moment.
	episodeStart := time.Now().Add(-UpgradeableBlockInertia - time.Minute)
	persisted := newOperatorClientWithConditions(operatorv1.OperatorCondition{
		Type:               conditionTypePacemakerDegraded,
		Status:             operatorv1.ConditionTrue,
		Reason:             reasonPacemakerUnhealthy,
		Message:            "Node master-1 is unhealthy",
		LastTransitionTime: metav1.NewTime(episodeStart),
	})

	controller := createTestHealthCheckWithMockStatus(t, mockStatus)
	controller.disruptionTracker = NewDisruptionTracker()
	controller.operatorClient = &flakyStaticPodOperatorClient{StaticPodOperatorClient: persisted, failures: 1}
	ctx := context.TODO()

	// Sync #1: reading the operator status fails. The sync must surface the error and must not
	// cache a fallback anchor at the failure time.
	require.Error(t, controller.sync(ctx, nil))
	controller.previousMu.Lock()
	cached := controller.upgradeBlockedSince
	controller.previousMu.Unlock()
	require.True(t, cached.IsZero(), "a failed read must not cache a fallback episode start")

	// Sync #2: the read succeeds. The original deadline has already passed, so the block must be
	// applied immediately rather than starting a fresh 10-minute window from the retry.
	require.NoError(t, controller.sync(ctx, nil))
	upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable)
	require.Equal(t, operatorv1.ConditionFalse, upgradeable.Status,
		"the retry must honour the persisted deadline instead of restarting the window")
}

// flakyStaticPodOperatorClient wraps a StaticPodOperatorClient and fails the first N status reads,
// to exercise retries of the persisted-episode lookup.
type flakyStaticPodOperatorClient struct {
	v1helpers.StaticPodOperatorClient
	failures int
}

func (f *flakyStaticPodOperatorClient) GetStaticPodOperatorState() (*operatorv1.StaticPodOperatorSpec, *operatorv1.StaticPodOperatorStatus, string, error) {
	if f.failures > 0 {
		f.failures--
		return nil, nil, "", fmt.Errorf("injected operator status read failure")
	}
	return f.StaticPodOperatorClient.GetStaticPodOperatorState()
}

// newOperatorClientWithConditions builds a fake operator client whose persisted status already
// carries the given conditions, to simulate state surviving a controller restart.
func newOperatorClientWithConditions(conditions ...operatorv1.OperatorCondition) v1helpers.StaticPodOperatorClient {
	return v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed},
		},
		&operatorv1.StaticPodOperatorStatus{
			OperatorStatus: operatorv1.OperatorStatus{Conditions: conditions},
		},
		nil,
		nil,
	)
}

// newOperatorClientWithUpgradeable builds a fake operator client that already carries a persisted
// PacemakerHealthCheckUpgradeable condition with the given status and transition time, to simulate
// state surviving a controller restart.
func newOperatorClientWithUpgradeable(status operatorv1.ConditionStatus, reason string, transition time.Time) v1helpers.StaticPodOperatorClient {
	return newOperatorClientWithConditions(operatorv1.OperatorCondition{
		Type:               conditionTypePacemakerUpgradeable,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(transition),
	})
}

// TestHealthCheck_updateOperatorStatus_RestartMissingCRHonorsGrace is a regression test for the
// case where a long-healthy cluster restarts and the CR is momentarily missing. The persisted
// Upgradeable=True transitioned long ago; the grace period must still be honored (measured from
// the first Unknown observation), not skipped by seeding from that old transition time.
func TestHealthCheck_updateOperatorStatus_RestartMissingCRHonorsGrace(t *testing.T) {
	controller := createTestHealthCheck()
	// Upgradeable has been True for two days (healthy cluster), now restarted.
	controller.operatorClient = newOperatorClientWithUpgradeable(
		operatorv1.ConditionTrue, "AsExpected", time.Now().Add(-48*time.Hour))
	ctx := context.TODO()

	// CR missing after restart: no timestamp anywhere, no in-memory previous.
	missingCR := &HealthStatus{
		OverallStatus: StatusUnknown,
		Warnings:      []string{},
		Errors:        []string{"PacemakerCluster CR not found in cache"},
	}

	// First sync must leave both conditions unchanged - grace starts now, not 48h ago.
	require.NoError(t, controller.updateOperatorStatus(ctx, missingCR, nil))
	upgradeable, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable)
	require.Equal(t, operatorv1.ConditionTrue, upgradeable.Status, "first sync after restart must not skip the grace period")
	degraded, err := controller.getCurrentPacemakerCondition(conditionTypePacemakerDegraded)
	require.NoError(t, err)
	require.Nil(t, degraded, "first sync after restart must not degrade")

	// Sustained Unknown: once the grace period has elapsed, upgrades are blocked and etcd degrades.
	controller.previousMu.Lock()
	controller.unknownSince = time.Now().Add(-StatusUnknownDegradedThreshold - time.Minute)
	controller.previousMu.Unlock()

	require.NoError(t, controller.updateOperatorStatus(ctx, missingCR, nil))
	upgradeable, err = controller.getCurrentPacemakerCondition(conditionTypePacemakerUpgradeable)
	require.NoError(t, err)
	require.NotNil(t, upgradeable)
	require.Equal(t, operatorv1.ConditionFalse, upgradeable.Status, "sustained Unknown past grace must block upgrades")
	degraded, err = controller.getCurrentPacemakerCondition(conditionTypePacemakerDegraded)
	require.NoError(t, err)
	require.NotNil(t, degraded)
	require.Equal(t, operatorv1.ConditionTrue, degraded.Status, "sustained Unknown past grace must degrade")
}

func TestHealthCheck_recordHealthCheckEvents(t *testing.T) {
	controller := createTestHealthCheck()

	// Test with warnings and errors - previous was healthy (not degraded, no warnings, known status)
	previousHealthy := &HealthStatus{OverallStatus: StatusHealthy, Warnings: []string{}, Errors: []string{}}
	currentWithWarnings := &HealthStatus{
		OverallStatus: StatusWarning,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed",
			"Recent fencing event: reboot of master-1 success",
			"Some other warning",
		},
		Errors: []string{"Test error"},
	}

	controller.recordHealthCheckEvents(currentWithWarnings, previousHealthy)

	// Test with healthy status - previous had warnings
	currentHealthy := &HealthStatus{
		OverallStatus: StatusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	controller.recordHealthCheckEvents(currentHealthy, currentWithWarnings)
}

func TestHealthCheck_eventDeduplication(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Track previous status through the test
	var previousStatus *HealthStatus

	// First recording - Error state (start in Error to test transitions properly)
	// previousStatus=nil because this is first sync
	errorStatus := &HealthStatus{
		OverallStatus: StatusError,
		Warnings:      []string{},
		Errors:        []string{"Test error"},
	}
	controller.recordHealthCheckEvents(errorStatus, previousStatus)
	previousStatus = errorStatus

	// Count events after first recording
	initialEvents := len(recorder.Events())
	require.Equal(t, 1, initialEvents, "Should have recorded 1 error event")

	// Immediately record the same error again - should be deduplicated (5 min window)
	// previousStatus is now errorStatus (degraded)
	controller.recordHealthCheckEvents(errorStatus, previousStatus)
	afterDuplicateEvents := len(recorder.Events())
	require.Equal(t, initialEvents, afterDuplicateEvents, "Duplicate error should not be recorded")

	// Transition to Warning (operationally healthy) - should record PacemakerHealthy event
	// previousStatus is errorStatus (degraded), so healthy event should fire
	warningStatus := &HealthStatus{
		OverallStatus: StatusWarning,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed",
		},
		Errors: []string{},
	}
	controller.recordHealthCheckEvents(warningStatus, previousStatus)
	previousStatus = warningStatus
	afterWarningTransition := len(recorder.Events())
	require.Equal(t, 3, afterWarningTransition, "Should have recorded warning event + PacemakerHealthy transition event")

	// Stay in Warning - no transition event
	controller.recordHealthCheckEvents(warningStatus, previousStatus)
	afterWarningDuplicate := len(recorder.Events())
	require.Equal(t, afterWarningTransition, afterWarningDuplicate, "No transition event when staying in Warning")

	// Test fencing event deduplication (longer window)
	fencingStatus := &HealthStatus{
		OverallStatus: StatusWarning,
		Warnings: []string{
			"Recent fencing event: reboot of master-1 success",
		},
		Errors: []string{},
	}
	controller.recordHealthCheckEvents(fencingStatus, previousStatus)
	previousStatus = fencingStatus
	afterFencingEvents := len(recorder.Events())
	require.Equal(t, 4, afterFencingEvents, "Fencing event should be recorded")

	// Immediately record the same fencing event - should be deduplicated (24 hour window)
	controller.recordHealthCheckEvents(fencingStatus, previousStatus)
	afterFencingDuplicateEvents := len(recorder.Events())
	require.Equal(t, afterFencingEvents, afterFencingDuplicateEvents, "Duplicate fencing event should not be recorded")

	// Transition from Warning to Healthy - fires PacemakerWarningsCleared event
	// previousStatus has warnings, so WarningsCleared should fire
	healthyStatus := &HealthStatus{
		OverallStatus: StatusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(healthyStatus, previousStatus)
	previousStatus = healthyStatus
	afterHealthyTransition := len(recorder.Events())
	require.Equal(t, afterFencingDuplicateEvents+1, afterHealthyTransition, "PacemakerWarningsCleared event should fire on Warning to Healthy transition")

	// Stay healthy - no event
	controller.recordHealthCheckEvents(healthyStatus, previousStatus)
	afterHealthyDuplicate := len(recorder.Events())
	require.Equal(t, afterHealthyTransition, afterHealthyDuplicate, "No event when staying healthy")

	// Transition to Error
	controller.recordHealthCheckEvents(errorStatus, previousStatus)
	previousStatus = errorStatus
	beforeRecovery := len(recorder.Events())

	// Transition back to Healthy - should record PacemakerHealthy event
	// previousStatus is errorStatus (degraded), so healthy event should fire
	controller.recordHealthCheckEvents(healthyStatus, previousStatus)
	afterRecovery := len(recorder.Events())
	require.Greater(t, afterRecovery, beforeRecovery, "PacemakerHealthy event should be recorded on transition from Error to Healthy")
}

func TestBuildHealthStatusFromCR(t *testing.T) {
	tests := []struct {
		name           string
		cr             *pacmkrv1.PacemakerCluster
		expectedStatus HealthStatusValue
		expectErrors   bool
		errorContains  string // Optional: check error contains this substring
	}{
		{
			name: "healthy_cluster",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
				},
			},
			expectedStatus: StatusHealthy,
			expectErrors:   false,
		},
		{
			name: "unhealthy_kubelet",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameKubelet),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			errorContains:  string(pacmkrv1.PacemakerClusterResourceNameKubelet),
		},
		{
			name: "unhealthy_etcd",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
					},
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			errorContains:  string(pacmkrv1.PacemakerClusterResourceNameEtcd),
		},
		{
			name: "insufficient_nodes",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createInsufficientNodesClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
					},
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			errorContains:  "Insufficient nodes",
		},
		{
			name: "no_nodes",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes:       &[]pacmkrv1.PacemakerClusterNodeStatus{},
				},
			},
			expectedStatus: StatusError,
			expectErrors:   true,
			errorContains:  "No nodes found",
		},
		{
			name: "unpopulated_status",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{}, // Zero LastUpdated
			},
			expectedStatus: StatusUnknown,
			expectErrors:   true,
			errorContains:  "no status populated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := BuildHealthStatusFromCR(tt.cr, false)

			require.NotNil(t, status, "HealthStatus should not be nil")
			require.Equal(t, tt.expectedStatus, status.OverallStatus)

			if tt.expectErrors {
				require.NotEmpty(t, status.Errors, "Should have errors")
				if tt.errorContains != "" {
					found := false
					for _, err := range status.Errors {
						if strings.Contains(err, tt.errorContains) {
							found = true
							break
						}
					}
					require.True(t, found, "Error should contain %q", tt.errorContains)
				}
			} else {
				require.Empty(t, status.Errors, "Should have no errors")
				require.Empty(t, status.Warnings, "Should have no warnings")
			}
		})
	}
}

func TestCheckClusterConditions(t *testing.T) {
	tests := []struct {
		name          string
		conditions    []metav1.Condition
		nodes         *[]pacmkrv1.PacemakerClusterNodeStatus
		expectErrors  bool
		errorContains []string // Multiple strings to check
	}{
		{
			name:       "healthy_cluster",
			conditions: createHealthyClusterConditions(),
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors: false,
		},
		{
			name:       "insufficient_nodes",
			conditions: createInsufficientNodesClusterConditions(),
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true,
			errorContains: []string{"Insufficient nodes"},
		},
		{
			name:       "excessive_nodes",
			conditions: createExcessiveNodesClusterConditions(),
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
				createHealthyNodeStatus("master-2", []string{"192.168.1.12"}),
			},
			expectErrors:  true,
			errorContains: []string{"Excessive nodes", "expected 2, found 3"},
		},
		{
			name:       "maintenance_mode",
			conditions: createMaintenanceModeClusterConditions(),
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors:  true,
			errorContains: []string{"maintenance mode"},
		},
		{
			name:       "empty_conditions",
			conditions: []metav1.Condition{},
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true, // Empty conditions means we can't verify cluster health
			errorContains: []string{"No cluster conditions available"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: tt.conditions,
					Nodes:      tt.nodes,
				},
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			checkClusterConditions(cr, status)

			if tt.expectErrors {
				require.NotEmpty(t, status.Errors, "Should have errors")
				for _, expected := range tt.errorContains {
					require.Contains(t, status.Errors[0], expected, "Error should contain: %s", expected)
				}
			} else {
				require.Empty(t, status.Errors, "Should have no errors")
			}
		})
	}
}

func TestCheckNodeStatuses(t *testing.T) {
	tests := []struct {
		name          string
		nodes         *[]pacmkrv1.PacemakerClusterNodeStatus
		expectErrors  bool
		errorContains string
	}{
		{
			name: "healthy_nodes",
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors: false,
		},
		{
			name: "unhealthy_kubelet",
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameKubelet),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors:  true,
			errorContains: string(pacmkrv1.PacemakerClusterResourceNameKubelet),
		},
		{
			name: "unhealthy_etcd",
			nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true,
			errorContains: string(pacmkrv1.PacemakerClusterResourceNameEtcd),
		},
		{
			name:          "nil_nodes",
			nodes:         nil,
			expectErrors:  true, // Nil nodes means we can't verify cluster health
			errorContains: "No nodes found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes:      tt.nodes,
				},
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			checkNodeStatuses(cr, status, false)

			if tt.expectErrors {
				require.NotEmpty(t, status.Errors, "Should have errors")
				if tt.errorContains != "" {
					found := false
					for _, err := range status.Errors {
						if strings.Contains(err, tt.errorContains) {
							found = true
							break
						}
					}
					require.True(t, found, "Should have error containing: %s", tt.errorContains)
				}
			} else {
				require.Empty(t, status.Errors, "Should have no errors")
			}
		})
	}
}

func TestCheckNodeStatuses_MultipleUnhealthyResources(t *testing.T) {
	node := createHealthyNodeStatus("master-0", []string{"192.168.1.10"})
	for i := range node.Conditions {
		if node.Conditions[i].Type == pacmkrv1.NodeHealthyConditionType {
			node.Conditions[i].Status = metav1.ConditionFalse
			node.Conditions[i].Reason = pacmkrv1.NodeHealthyReasonUnhealthy
		}
	}
	for i := range node.Resources {
		if node.Resources[i].Name == pacmkrv1.PacemakerClusterResourceNameKubelet ||
			node.Resources[i].Name == pacmkrv1.PacemakerClusterResourceNameEtcd {
			node.Resources[i].Conditions = createUnhealthyResourceConditions()
		}
	}

	pacemakerStatus := &pacmkrv1.PacemakerCluster{
		Status: pacmkrv1.PacemakerClusterStatus{
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				node,
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	checkNodeStatuses(pacemakerStatus, status, false)
	require.Len(t, status.Errors, 1, "Should have 1 consolidated error for unhealthy node")

	// Verify the single error mentions both resources
	nodeError := status.Errors[0]
	require.Contains(t, nodeError, "master-0", "Error should mention the node name")
	require.Contains(t, nodeError, "node is unhealthy", "Error should indicate node is unhealthy")
	require.Contains(t, nodeError, string(pacmkrv1.PacemakerClusterResourceNameKubelet), "Error should mention Kubelet")
	require.Contains(t, nodeError, string(pacmkrv1.PacemakerClusterResourceNameEtcd), "Error should mention Etcd")
}

func TestGetResourceIssue(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		wantIssue  string
	}{
		{
			name:       "failed takes priority",
			conditions: []metav1.Condition{{Type: pacmkrv1.ResourceOperationalConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "has failed",
		},
		{
			name:       "stopped",
			conditions: []metav1.Condition{{Type: pacmkrv1.ResourceStartedConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is stopped",
		},
		{
			name:       "inactive",
			conditions: []metav1.Condition{{Type: pacmkrv1.ResourceActiveConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is not active",
		},
		{
			name:       "unmanaged",
			conditions: []metav1.Condition{{Type: pacmkrv1.ResourceManagedConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is unmanaged",
		},
		{
			name:       "fallback to generic unhealthy",
			conditions: []metav1.Condition{{Type: pacmkrv1.ResourceHealthyConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is unhealthy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getResourceIssue(tt.conditions)
			require.Equal(t, tt.wantIssue, got)
		})
	}
}

func TestHealthCheck_recordErrorEvent(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Test different error types get the correct event reason
	tests := []struct {
		name           string
		errorMsg       string
		expectedReason string
	}{
		{
			name:           "resource_unhealthy",
			errorMsg:       "Kubelet resource is unhealthy on node master-0: resource has failed",
			expectedReason: EventReasonResourceUnhealthy,
		},
		{
			name:           "node_unhealthy",
			errorMsg:       "master-0 node is unhealthy: node has issues",
			expectedReason: EventReasonNodeUnhealthy,
		},
		{
			name:           "cluster_unhealthy",
			errorMsg:       "Cluster is unhealthy: Pacemaker cluster has issues that need investigation",
			expectedReason: EventReasonClusterUnhealthy,
		},
		{
			name:           "insufficient_nodes",
			errorMsg:       "Insufficient nodes in cluster (expected 2, found 1)",
			expectedReason: EventReasonInsufficientNodes,
		},
		{
			name:           "status_stale",
			errorMsg:       "Pacemaker status is stale",
			expectedReason: EventReasonStatusStale,
		},
		{
			name:           "cr_not_found",
			errorMsg:       "Failed to get PacemakerCluster CR: not found",
			expectedReason: EventReasonCRNotFound,
		},
		{
			name:           "cr_no_status",
			errorMsg:       "PacemakerCluster CR has no status populated",
			expectedReason: EventReasonCRNotFound,
		},
		{
			name:           "node_offline",
			errorMsg:       "Node master-0 is offline",
			expectedReason: EventReasonNodeOffline,
		},
		{
			name:           "excessive_nodes",
			errorMsg:       "Excessive nodes in cluster (expected 2, found 3)",
			expectedReason: EventReasonInsufficientNodes,
		},
		{
			name:           "cluster_maintenance",
			errorMsg:       "Cluster is in maintenance mode",
			expectedReason: EventReasonClusterInMaintenance,
		},
		{
			name:           "generic_error",
			errorMsg:       "Some unknown error",
			expectedReason: EventReasonGenericError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initialCount := len(recorder.Events())
			controller.recordErrorEvent(tt.errorMsg)

			events := recorder.Events()
			require.Len(t, events, initialCount+1, "Should have recorded one more event")

			// Get the last event
			lastEvent := events[len(events)-1]
			require.Equal(t, tt.expectedReason, lastEvent.Reason, "Event reason should match expected")
			require.Contains(t, lastEvent.Message, tt.errorMsg, "Event message should contain error message")
		})
	}
}

func TestHealthCheck_eventDeduplication_StatusTransitions(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Track previous status through the test
	var previousStatus *HealthStatus

	// Start with healthy status from Unknown state (first sync or recovering from stale)
	// previousStatus=nil: PacemakerHealthy event fires on initial healthy status
	healthyStatus := &HealthStatus{
		OverallStatus: StatusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(healthyStatus, previousStatus) // previous=nil (first sync)
	previousStatus = healthyStatus
	afterFirstHealthy := len(recorder.Events())
	require.Equal(t, 1, afterFirstHealthy, "Healthy event should fire on transition from Unknown")

	// Stay healthy - should not record
	controller.recordHealthCheckEvents(healthyStatus, previousStatus)
	require.Equal(t, afterFirstHealthy, len(recorder.Events()), "No event when staying healthy")

	// Transition to Warning - should record warning but not healthy event (already in healthy state)
	warningStatus := &HealthStatus{
		OverallStatus: StatusWarning,
		Warnings:      []string{"Some warning"},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(warningStatus, previousStatus)
	previousStatus = warningStatus
	afterWarning := len(recorder.Events())
	require.Equal(t, afterFirstHealthy+1, afterWarning, "Warning event should be recorded")

	// Transition from Warning to Healthy - should record PacemakerWarningsCleared event
	// previousStatus has warnings, so WarningsCleared should fire
	controller.recordHealthCheckEvents(healthyStatus, previousStatus)
	previousStatus = healthyStatus
	afterSecondHealthy := len(recorder.Events())
	require.Equal(t, afterWarning+1, afterSecondHealthy, "PacemakerWarningsCleared event should fire on Warning to Healthy transition")

	// Transition to Error
	errorStatus := &HealthStatus{
		OverallStatus: StatusError,
		Warnings:      []string{},
		Errors:        []string{"Critical error"},
	}
	controller.recordHealthCheckEvents(errorStatus, previousStatus)
	previousStatus = errorStatus
	afterError := len(recorder.Events())
	require.Greater(t, afterError, afterSecondHealthy, "Error should be recorded")

	// Transition from Error to Healthy - should record PacemakerHealthy event
	// previousStatus is errorStatus (degraded), so healthy event should fire
	controller.recordHealthCheckEvents(healthyStatus, previousStatus)
	afterThirdHealthy := len(recorder.Events())
	require.Equal(t, afterError+1, afterThirdHealthy, "Healthy event should be recorded on transition from Error")
}

func TestHealthCheck_eventDeduplication_CleanupOldEntries(t *testing.T) {
	controller := createTestHealthCheck()

	// Add a very old entry to the recorded events map
	oldEventKey := "warning:Very old warning"
	controller.recordedEventsMu.Lock()
	controller.recordedEvents[oldEventKey] = time.Now().Add(-25 * time.Hour) // 25 hours ago (older than longest window of 24 hours)
	controller.recordedEventsMu.Unlock()

	// Trigger cleanup by recording a new event
	previousStatus := &HealthStatus{OverallStatus: StatusHealthy, Warnings: []string{}, Errors: []string{}}
	currentStatus := &HealthStatus{
		OverallStatus: StatusWarning,
		Warnings:      []string{"New warning"},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(currentStatus, previousStatus)

	// Verify old entry was cleaned up
	controller.recordedEventsMu.Lock()
	_, exists := controller.recordedEvents[oldEventKey]
	require.False(t, exists, "Old entry should have been cleaned up")
	controller.recordedEventsMu.Unlock()
}

func TestGetNodeConditionErrorsAndWarnings(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name             string
		conditions       []metav1.Condition
		expectedErrors   []string
		expectedWarnings []string
	}{
		{
			name: "in_maintenance_mode",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeInServiceConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeInServiceReasonInMaintenance, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in maintenance mode"},
			expectedWarnings: []string{},
		},
		{
			name: "in_standby_mode",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeActiveConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeActiveReasonStandby, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in standby mode"},
			expectedWarnings: []string{},
		},
		{
			name: "unclean_state",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeCleanConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeCleanReasonUnclean, LastTransitionTime: now},
			},
			expectedErrors:   []string{"unclean state (fencing/communication issue)"},
			expectedWarnings: []string{},
		},
		{
			name: "not_a_member",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeMemberConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeMemberReasonNotMember, LastTransitionTime: now},
			},
			expectedErrors:   []string{"not a cluster member"},
			expectedWarnings: []string{},
		},
		{
			name: "fencing_unavailable",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeFencingAvailableConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeFencingAvailableReasonUnavailable, LastTransitionTime: now},
			},
			expectedErrors:   []string{"fencing unavailable (no agents running)"},
			expectedWarnings: []string{},
		},
		{
			name: "fencing_unhealthy_but_available_is_warning",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue, Reason: pacmkrv1.NodeFencingAvailableReasonAvailable, LastTransitionTime: now},
				{Type: pacmkrv1.NodeFencingHealthyConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeFencingHealthyReasonUnhealthy, LastTransitionTime: now},
			},
			expectedErrors:   []string{},
			expectedWarnings: []string{"fencing at risk"},
		},
		{
			name: "pending_state_no_error_or_warning",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeReadyConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeReadyReasonPending, LastTransitionTime: now},
			},
			expectedErrors:   []string{},
			expectedWarnings: []string{},
		},
		{
			name: "multiple_errors",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeInServiceConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				{Type: pacmkrv1.NodeActiveConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				{Type: pacmkrv1.NodeCleanConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in maintenance mode", "in standby mode", "unclean state (fencing/communication issue)"},
			expectedWarnings: []string{},
		},
		{
			name: "error_and_warning_together",
			conditions: []metav1.Condition{
				{Type: pacmkrv1.NodeInServiceConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				{Type: pacmkrv1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue, Reason: pacmkrv1.NodeFencingAvailableReasonAvailable, LastTransitionTime: now},
				{Type: pacmkrv1.NodeFencingHealthyConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeFencingHealthyReasonUnhealthy, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in maintenance mode"},
			expectedWarnings: []string{"fencing at risk"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Check errors and warnings using the separate functions
			errors := getNodeConditionErrors(tt.conditions)
			warnings := getFencingWarnings(tt.conditions)

			// Check errors
			require.Equal(t, len(tt.expectedErrors), len(errors), "Number of errors should match")
			for _, expected := range tt.expectedErrors {
				found := false
				for _, err := range errors {
					if strings.Contains(err, expected) {
						found = true
						break
					}
				}
				require.True(t, found, "Expected error '%s' not found in %v", expected, errors)
			}

			// Check warnings
			require.Equal(t, len(tt.expectedWarnings), len(warnings), "Number of warnings should match")
			for _, expected := range tt.expectedWarnings {
				found := false
				for _, warn := range warnings {
					if strings.Contains(warn, expected) {
						found = true
						break
					}
				}
				require.True(t, found, "Expected warning '%s' not found in %v", expected, warnings)
			}
		})
	}
}

// TestCheckNodeConditions_FencingDegradedWarning tests fencing degradation warnings in
// various scenarios. Fencing degraded (FencingHealthy=False but FencingAvailable=True) should be
// reported as a warning, not an error, and should be captured regardless of overall node health.
func TestCheckNodeConditions_FencingDegradedWarning(t *testing.T) {
	tests := []struct {
		name                   string
		nodeHealthy            bool   // Overall node healthy condition
		inMaintenance          bool   // Node in maintenance mode (causes error)
		expectWarnings         bool   // Expect fencing warning
		expectErrors           bool   // Expect errors
		expectedWarningContain string // String warning should contain
		expectedErrorContain   string // String error should contain
	}{
		{
			name:                   "fencing_degraded_node_unhealthy",
			nodeHealthy:            false,
			inMaintenance:          false,
			expectWarnings:         true,
			expectErrors:           false, // Only fencing issue, which is a warning
			expectedWarningContain: "fencing at risk",
		},
		{
			name:                   "fencing_degraded_node_healthy",
			nodeHealthy:            true, // Node is healthy overall, but fencing is degraded
			inMaintenance:          false,
			expectWarnings:         true,
			expectErrors:           false,
			expectedWarningContain: "fencing at risk",
		},
		{
			name:                   "fencing_degraded_with_other_errors",
			nodeHealthy:            false,
			inMaintenance:          true, // Causes error
			expectWarnings:         true,
			expectErrors:           true,
			expectedWarningContain: "fencing at risk",
			expectedErrorContain:   "maintenance mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.Now()

			// Build conditions based on test case
			nodeHealthyStatus := metav1.ConditionFalse
			nodeHealthyReason := pacmkrv1.NodeHealthyReasonUnhealthy
			if tt.nodeHealthy {
				nodeHealthyStatus = metav1.ConditionTrue
				nodeHealthyReason = pacmkrv1.NodeHealthyReasonHealthy
			}

			inServiceStatus := metav1.ConditionTrue
			inServiceReason := pacmkrv1.NodeInServiceReasonInService
			if tt.inMaintenance {
				inServiceStatus = metav1.ConditionFalse
				inServiceReason = pacmkrv1.NodeInServiceReasonInMaintenance
			}

			conditions := []metav1.Condition{
				{Type: pacmkrv1.NodeHealthyConditionType, Status: nodeHealthyStatus, Reason: nodeHealthyReason, LastTransitionTime: now},
				{Type: pacmkrv1.NodeOnlineConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: pacmkrv1.NodeInServiceConditionType, Status: inServiceStatus, Reason: inServiceReason, LastTransitionTime: now},
				{Type: pacmkrv1.NodeActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: pacmkrv1.NodeReadyConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: pacmkrv1.NodeCleanConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: pacmkrv1.NodeMemberConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				// Fencing available but not healthy - warning
				{Type: pacmkrv1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue, Reason: pacmkrv1.NodeFencingAvailableReasonAvailable, LastTransitionTime: now},
				{Type: pacmkrv1.NodeFencingHealthyConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.NodeFencingHealthyReasonUnhealthy, LastTransitionTime: now},
			}

			healthyResources := []pacmkrv1.PacemakerClusterResourceStatus{
				{Name: pacmkrv1.PacemakerClusterResourceNameKubelet, Conditions: createHealthyResourceConditions()},
				{Name: pacmkrv1.PacemakerClusterResourceNameEtcd, Conditions: createHealthyResourceConditions()},
			}

			node := pacmkrv1.PacemakerClusterNodeStatus{
				NodeName:   "master-0",
				Conditions: conditions,
				Resources:  healthyResources,
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			checkNodeConditions(node, status, false)

			// Check warnings
			if tt.expectWarnings {
				require.NotEmpty(t, status.Warnings, "Should have warnings")
				require.Contains(t, status.Warnings[0], "master-0", "Warning should include node name")
				require.Contains(t, status.Warnings[0], tt.expectedWarningContain, "Warning should contain expected text")
			} else {
				require.Empty(t, status.Warnings, "Should not have warnings")
			}

			// Check errors
			if tt.expectErrors {
				require.NotEmpty(t, status.Errors, "Should have errors")
				require.Contains(t, status.Errors[0], tt.expectedErrorContain, "Error should contain expected text")
			} else {
				require.Empty(t, status.Errors, "Should not have errors")
			}
		})
	}
}

// TestIsFencingDegraded bounds the exact signal a fencing warning reports on: fencing
// available but not healthy. Unavailable fencing is an Error handled elsewhere and must not
// trip this predicate.
func TestIsFencingDegraded(t *testing.T) {
	now := metav1.Now()
	cond := func(t string, s metav1.ConditionStatus) metav1.Condition {
		return metav1.Condition{Type: t, Status: s, LastTransitionTime: now}
	}

	tests := []struct {
		name       string
		conditions []metav1.Condition
		expect     bool
	}{
		{
			name: "available_but_not_healthy_is_degraded",
			conditions: []metav1.Condition{
				cond(pacmkrv1.NodeFencingAvailableConditionType, metav1.ConditionTrue),
				cond(pacmkrv1.NodeFencingHealthyConditionType, metav1.ConditionFalse),
			},
			expect: true,
		},
		{
			name: "available_and_healthy_is_not_degraded",
			conditions: []metav1.Condition{
				cond(pacmkrv1.NodeFencingAvailableConditionType, metav1.ConditionTrue),
				cond(pacmkrv1.NodeFencingHealthyConditionType, metav1.ConditionTrue),
			},
			expect: false,
		},
		{
			name: "unavailable_is_an_error_not_degraded",
			conditions: []metav1.Condition{
				cond(pacmkrv1.NodeFencingAvailableConditionType, metav1.ConditionFalse),
				cond(pacmkrv1.NodeFencingHealthyConditionType, metav1.ConditionFalse),
			},
			expect: false,
		},
		{
			name:       "missing_fencing_conditions_is_not_degraded",
			conditions: []metav1.Condition{cond(pacmkrv1.NodeOnlineConditionType, metav1.ConditionTrue)},
			expect:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expect, isFencingDegraded(tt.conditions))
		})
	}
}

// TestHealthCheck_WarningsClearedEvent tests PacemakerWarningsCleared and PacemakerHealthy
// event firing across various state transitions.
func TestHealthCheck_WarningsClearedEvent(t *testing.T) {
	tests := []struct {
		name                  string
		previousStatus        *HealthStatus
		currentStatus         *HealthStatus
		expectHealthyEvent    bool
		expectWarningsCleared bool
	}{
		{
			name: "warning_to_healthy",
			previousStatus: &HealthStatus{
				OverallStatus: StatusWarning,
				Warnings:      []string{"master-0: " + msgFencingDegraded},
				Errors:        []string{},
			},
			currentStatus: &HealthStatus{
				OverallStatus: StatusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			expectHealthyEvent:    false, // Not degraded before
			expectWarningsCleared: true,  // Had warnings before
		},
		{
			name: "error_without_warnings_to_healthy",
			previousStatus: &HealthStatus{
				OverallStatus: StatusError,
				Warnings:      []string{},
				Errors:        []string{"Critical error"},
			},
			currentStatus: &HealthStatus{
				OverallStatus: StatusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			expectHealthyEvent:    true,  // Was degraded
			expectWarningsCleared: false, // No warnings before
		},
		{
			name: "error_with_warnings_to_error_without_warnings",
			previousStatus: &HealthStatus{
				OverallStatus: StatusError,
				Warnings:      []string{"master-0: " + msgFencingDegraded},
				Errors:        []string{"Critical error"},
			},
			currentStatus: &HealthStatus{
				OverallStatus: StatusError,
				Warnings:      []string{},
				Errors:        []string{"Critical error"},
			},
			expectHealthyEvent:    false, // Still degraded
			expectWarningsCleared: true,  // Warnings cleared while still degraded
		},
		{
			name: "error_with_warnings_to_healthy",
			previousStatus: &HealthStatus{
				OverallStatus: StatusError,
				Warnings:      []string{"master-0: " + msgFencingDegraded},
				Errors:        []string{"Critical error"},
			},
			currentStatus: &HealthStatus{
				OverallStatus: StatusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			expectHealthyEvent:    true, // Was degraded
			expectWarningsCleared: true, // Had warnings
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := createTestHealthCheck()
			recorder := controller.eventRecorder.(events.InMemoryRecorder)

			controller.recordHealthCheckEvents(tt.currentStatus, tt.previousStatus)

			var healthyEventFound, warningsClearedFound bool
			for _, e := range recorder.Events() {
				if e.Reason == EventReasonHealthy {
					healthyEventFound = true
				}
				if e.Reason == EventReasonWarningsCleared {
					warningsClearedFound = true
				}
			}

			require.Equal(t, tt.expectHealthyEvent, healthyEventFound,
				"PacemakerHealthy event expectation mismatch")
			require.Equal(t, tt.expectWarningsCleared, warningsClearedFound,
				"PacemakerWarningsCleared event expectation mismatch")
		})
	}
}

// unhealthyAlertAgentEntry builds an alert-agent entry with the three alert-agent conditions
// (Healthy False, Configured True, Available False) representing a missing script on this node.
func unhealthyAlertAgentEntry(name pacmkrv1.PacemakerClusterAlertAgentName) pacmkrv1.PacemakerClusterAlertAgentStatus {
	now := metav1.Now()
	return pacmkrv1.PacemakerClusterAlertAgentStatus{
		Name: name,
		Conditions: []metav1.Condition{
			{Type: pacmkrv1.AlertAgentHealthyConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.AlertAgentHealthyReasonUnhealthy, LastTransitionTime: now},
			{Type: pacmkrv1.AlertAgentConfiguredConditionType, Status: metav1.ConditionTrue, Reason: pacmkrv1.AlertAgentConfiguredReasonConfigured, LastTransitionTime: now},
			{Type: pacmkrv1.AlertAgentAvailableConditionType, Status: metav1.ConditionFalse, Reason: pacmkrv1.AlertAgentAvailableReasonUnavailable, LastTransitionTime: now},
		},
	}
}

// TestCheckNodeConditions_AlertAgentDegradesOperator verifies that a confirmed alert-agent failure
// produces an error (and therefore degrades the operator). Unlike the resources[]-reuse design this
// replaced, alert-agent health is expected to have already been folded by the status collector into
// the node's AlertAgentsHealthy and Healthy conditions (see refreshHealthAfterAlertAgents in
// alertstatus.go) before checkNodeConditions ever sees the CR, so these fixtures set node.Healthy=False
// themselves to reflect a CR the collector actually would have produced.
func TestCheckNodeConditions_AlertAgentDegradesOperator(t *testing.T) {
	t.Run("node unhealthy solely due to a bad alert agent degrades and names it", func(t *testing.T) {
		node := createHealthyNodeStatus("master-0", []string{"192.168.1.10"})
		for i := range node.Conditions {
			if node.Conditions[i].Type == pacmkrv1.NodeHealthyConditionType {
				node.Conditions[i].Status = metav1.ConditionFalse
				node.Conditions[i].Reason = pacmkrv1.NodeHealthyReasonUnhealthy
			}
		}
		node.Conditions = append(node.Conditions, metav1.Condition{Type: pacmkrv1.NodeAlertAgentsHealthyConditionType, Status: metav1.ConditionFalse})
		node.AlertAgents = append(node.AlertAgents, unhealthyAlertAgentEntry(pacmkrv1.PacemakerClusterAlertAgentNameTaint))

		status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
		checkNodeConditions(node, status, true)

		require.Empty(t, status.Warnings, "a bad alert agent is reported as an error, not a warning")
		require.Len(t, status.Errors, 1, "a bad alert agent must produce exactly one node-unhealthy error")
		require.Contains(t, status.Errors[0], "master-0")
		require.Contains(t, status.Errors[0], "alert agent")
		require.Contains(t, status.Errors[0], string(pacmkrv1.PacemakerClusterAlertAgentNameTaint))
	})

	t.Run("node unhealthy for both a core reason and a bad alert agent names both in one error", func(t *testing.T) {
		node := createHealthyNodeStatus("master-0", []string{"192.168.1.10"})
		for i := range node.Conditions {
			switch node.Conditions[i].Type {
			case pacmkrv1.NodeHealthyConditionType:
				node.Conditions[i].Status = metav1.ConditionFalse
				node.Conditions[i].Reason = pacmkrv1.NodeHealthyReasonUnhealthy
			case pacmkrv1.NodeInServiceConditionType:
				node.Conditions[i].Status = metav1.ConditionFalse
				node.Conditions[i].Reason = pacmkrv1.NodeInServiceReasonInMaintenance
			}
		}
		node.Conditions = append(node.Conditions, metav1.Condition{Type: pacmkrv1.NodeAlertAgentsHealthyConditionType, Status: metav1.ConditionFalse})
		node.AlertAgents = append(node.AlertAgents, unhealthyAlertAgentEntry(pacmkrv1.PacemakerClusterAlertAgentNameUntaint))

		status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
		checkNodeConditions(node, status, true)

		require.Empty(t, status.Warnings)
		require.Len(t, status.Errors, 1, "both problems are folded into a single node-unhealthy error")
		require.Contains(t, status.Errors[0], "alert agent")
		require.Contains(t, status.Errors[0], string(pacmkrv1.PacemakerClusterAlertAgentNameUntaint))
		require.Contains(t, status.Errors[0], "maintenance mode")
	})

	t.Run("Pending AlertAgentsHealthy never degrades an otherwise-healthy node", func(t *testing.T) {
		node := createHealthyNodeStatus("master-0", []string{"192.168.1.10"})
		node.Conditions = append(node.Conditions, metav1.Condition{Type: pacmkrv1.NodeAlertAgentsHealthyConditionType, Status: metav1.ConditionUnknown, Reason: "Pending"})

		status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
		checkNodeConditions(node, status, true)

		require.Empty(t, status.Warnings)
		require.Empty(t, status.Errors, "Unknown/Pending alert-agent health must never degrade the operator")
	})
}

func TestHealthCheck_AlertAgentFeatureGate(t *testing.T) {
	// Retained alert-agent entries must not change the base resource diagnostics
	// when the reader's gate is disabled, even before the next collector update.
	base := testCluster(withNodes(
		testNode("master-0", "192.168.111.20", unhealthyEtcd()),
		testNode("master-1", "192.168.111.21"),
	))
	withAgents := base.DeepCopy()
	(*withAgents.Status.Nodes)[0].AlertAgents = []pacmkrv1.PacemakerClusterAlertAgentStatus{
		unhealthyAlertAgentEntry(pacmkrv1.PacemakerClusterAlertAgentNameTaint),
	}
	original := withAgents.DeepCopy()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			controller := createTestHealthCheckWithMockStatus(t, withAgents)
			controller.alertAgentStatusEnabled = enabled
			status, _, err := controller.getPacemakerStatus(context.Background())
			require.NoError(t, err)
			require.Equal(t, StatusError, status.OverallStatus, "base resource failures must still be reported")
			if enabled {
				require.Contains(t, strings.Join(status.Errors, " "), "alert agent")
			} else {
				require.Equal(t, BuildHealthStatusFromCR(base, false), status)
			}
			require.Equal(t, original, withAgents, "reading health must not mutate the informer cache")
		})
	}
}
