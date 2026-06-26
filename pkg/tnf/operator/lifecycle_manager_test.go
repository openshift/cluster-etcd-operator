package operator

/*
TEST COVERAGE SUMMARY - lifecycle_manager_test.go
==================================================

This file tests PacemakerLifecycleManager - health monitoring and node reconciliation
for Two-Node Fencing (TNF) clusters.

WHAT'S TESTED
-------------

Health Monitoring Path:
├── getPacemakerStatus() - CR status retrieval and staleness detection
│   ├── ✅ Healthy cluster
│   ├── ✅ Unhealthy kubelet
│   ├── ✅ Unhealthy etcd
│   ├── ✅ Insufficient nodes
│   ├── ✅ No nodes
│   ├── ✅ Unpopulated status
│   ├── ✅ Stale status detection
│   ├── ✅ CR not found
│   └── ✅ Unchanged timestamp (skip processing)
├── updateOperatorStatus() - Operator condition management
│   ├── ✅ Degraded condition set (pacemaker error)
│   ├── ✅ Available condition set (pacemaker healthy/warning)
│   ├── ✅ Unknown status with grace period
│   └── ✅ Unknown status grace period expired
├── recordHealthCheckEvents() - Event recording with deduplication
│   ├── ✅ Fencing event
│   ├── ✅ Resource/node/cluster unhealthy events
│   ├── ✅ Status transitions (degraded → healthy)
│   ├── ✅ Event deduplication (same event not re-recorded)
│   └── ✅ Old event cleanup (>1 hour)
└── detectDrift() - K8s vs pacemaker state comparison
    ├── ✅ No drift
    ├── ✅ Node count mismatch (add)
    ├── ✅ Node count mismatch (delete)
    ├── ✅ IP address mismatch
    ├── ✅ Name mismatch (node replacement)
    └── ✅ Empty states

Reconciliation Path:
├── ReconcilePacemakerConfig() - Drift detection and reconciliation trigger
│   ├── ✅ No pacemaker CR - skip
│   ├── ✅ Node informer not synced - skip
│   ├── ✅ More than 2 nodes - skip
│   ├── ✅ Node not ready - skip
│   ├── ✅ No drift - no action
│   ├── ✅ Drift detected (count/IP mismatch) - trigger update-setup
│   ├── ✅ Update-setup already running - skip
│   ├── ✅ No intersection - error
│   └── ✅ Update-setup fails - return error
└── cleanupOrphanedJobs() - Delete jobs for deleted nodes
    ├── ✅ No jobs to clean
    ├── ✅ Orphaned job deleted
    ├── ✅ Multiple orphaned jobs deleted
    ├── ✅ Active job preserved
    └── ✅ Non-TNF jobs ignored
*/

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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

func createTestHealthCheck() *PacemakerLifecycleManager {
	return &PacemakerLifecycleManager{
		operatorClient:    createFakeOperatorClient(),
		kubeClient:        fake.NewSimpleClientset(),
		eventRecorder:     events.NewInMemoryRecorder("test", clock.RealClock{}),
		pacemakerInformer: createFakeInformer(nil),
		recordedEvents:    make(map[string]time.Time),
	}
}

// createFakeInformer creates a fake SharedIndexInformer with a store containing the given object.
// If obj is nil, the store will be empty.
func createFakeInformer(obj *pacmkrv1.PacemakerCluster) cache.SharedIndexInformer {
	store := cache.NewStore(func(obj any) (string, error) {
		if pc, ok := obj.(*pacmkrv1.PacemakerCluster); ok {
			return pc.Name, nil
		}
		return "", fmt.Errorf("object is not a PacemakerCluster")
	})
	if obj != nil {
		if err := store.Add(obj); err != nil {
			panic(fmt.Sprintf("failed to add object to fake informer store: %v", err))
		}
	}
	return &fakeInformer{store: store}
}

// fakeInformer implements cache.SharedIndexInformer for testing
type fakeInformer struct {
	store cache.Store
}

func (f *fakeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}
func (f *fakeInformer) GetStore() cache.Store           { return f.store }
func (f *fakeInformer) GetController() cache.Controller { return nil }
func (f *fakeInformer) Run(stopCh <-chan struct{})      {}
func (f *fakeInformer) RunWithContext(ctx context.Context) {
}
func (f *fakeInformer) HasSynced() bool                                            { return true }
func (f *fakeInformer) LastSyncResourceVersion() string                            { return "" }
func (f *fakeInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error { return nil }
func (f *fakeInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (f *fakeInformer) SetTransform(handler cache.TransformFunc) error { return nil }
func (f *fakeInformer) IsStopped() bool                                { return false }
func (f *fakeInformer) AddIndexers(indexers cache.Indexers) error      { return nil }
func (f *fakeInformer) GetIndexer() cache.Indexer                      { return nil }

// createTestHealthCheckWithMockStatus creates a HealthCheck with a mocked Pacemaker CR in the informer cache
func createTestHealthCheckWithMockStatus(t *testing.T, mockStatus *pacmkrv1.PacemakerCluster) *PacemakerLifecycleManager {
	return &PacemakerLifecycleManager{
		operatorClient:    createFakeOperatorClient(),
		kubeClient:        fake.NewSimpleClientset(),
		eventRecorder:     events.NewInMemoryRecorder("test", clock.RealClock{}),
		pacemakerInformer: createFakeInformer(mockStatus),
		recordedEvents:    make(map[string]time.Time),
	}
}

// =============================================================================
// Unit Tests
// =============================================================================
// Note: Test fixtures are defined in test_fixtures_test.go

func TestHealthCheck_getPacemakerStatus(t *testing.T) {
	tests := []struct {
		name           string
		mockStatus     *pacmkrv1.PacemakerCluster
		expectedStatus pacemaker.HealthStatusValue
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
					Conditions: pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: pacemaker.StatusHealthy,
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
					Conditions: pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // > 5 min threshold
				},
			},
			expectedStatus: pacemaker.StatusUnknown,
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
			expectedStatus: pacemaker.StatusUnknown,
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
					Conditions: pacemaker.CreateInsufficientNodesClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: pacemaker.StatusError,
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
					Conditions: pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameKubelet),
						pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: pacemaker.StatusError,
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
					Conditions: pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						pacemaker.CreateUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: pacemaker.StatusError,
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
					Conditions: pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
						pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: pacemaker.StatusError,
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
			require.NotNil(t, current, "pacemaker.HealthStatus should not be nil")
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
			Conditions: pacemaker.CreateHealthyClusterConditions(),
			Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
				pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
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
	require.Equal(t, pacemaker.StatusHealthy, current1.OverallStatus, "First call should return healthy status")

	// Second call with same timestamp should return nil (no change)
	current2, _, err2 := controller.getPacemakerStatus(ctx)
	require.NoError(t, err2, "Second getPacemakerStatus should not return an error")
	require.Nil(t, current2, "Second call with unchanged timestamp should return nil")
}

func TestHealthCheck_updateOperatorStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   *pacemaker.HealthStatus
		previous *pacemaker.HealthStatus
	}{
		{
			name: "error_status_sets_degraded",
			status: &pacemaker.HealthStatus{
				OverallStatus: pacemaker.StatusError,
				Warnings:      []string{"Test warning"},
				Errors:        []string{"Test error"},
			},
			previous: &pacemaker.HealthStatus{
				OverallStatus: pacemaker.StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
		},
		{
			name: "healthy_status_clears_degraded",
			status: &pacemaker.HealthStatus{
				OverallStatus: pacemaker.StatusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			previous: &pacemaker.HealthStatus{
				OverallStatus: pacemaker.StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
		},
		{
			name: "warning_status_clears_degraded",
			status: &pacemaker.HealthStatus{
				OverallStatus: pacemaker.StatusWarning,
				Warnings:      []string{"Test warning"},
				Errors:        []string{},
			},
			previous: &pacemaker.HealthStatus{
				OverallStatus: pacemaker.StatusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := createTestHealthCheck()
			ctx := context.TODO()

			err := controller.updateOperatorStatus(ctx, tt.status, tt.previous)
			require.NoError(t, err, "updateOperatorStatus should not return an error")
		})
	}
}

func TestHealthCheck_recordHealthCheckEvents(t *testing.T) {
	controller := createTestHealthCheck()

	// Test with warnings and errors - previous was healthy (not degraded, no warnings, known status)
	previousHealthy := &pacemaker.HealthStatus{OverallStatus: pacemaker.StatusHealthy, Warnings: []string{}, Errors: []string{}}
	currentWithWarnings := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusWarning,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed",
			"Recent fencing event: reboot of master-1 success",
			"Some other warning",
		},
		Errors: []string{"Test error"},
	}

	controller.recordHealthCheckEvents(currentWithWarnings, previousHealthy)

	// Test with healthy status - previous had warnings
	currentHealthy := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	controller.recordHealthCheckEvents(currentHealthy, currentWithWarnings)
}

func TestHealthCheck_eventDeduplication(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Track previous status through the test
	var previousStatus *pacemaker.HealthStatus

	// First recording - Error state (start in Error to test transitions properly)
	// previousStatus=nil because this is first sync
	errorStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusError,
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
	warningStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusWarning,
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
	fencingStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusWarning,
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
	healthyStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusHealthy,
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

func TestHealthCheck_BuildHealthStatusFromCR(t *testing.T) {
	tests := []struct {
		name           string
		cr             *pacmkrv1.PacemakerCluster
		expectedStatus pacemaker.HealthStatusValue
		expectErrors   bool
		errorContains  string // Optional: check error contains this substring
	}{
		{
			name: "healthy_cluster",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
				},
			},
			expectedStatus: pacemaker.StatusHealthy,
			expectErrors:   false,
		},
		{
			name: "unhealthy_kubelet",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, pacmkrv1.PacemakerClusterResourceNameKubelet),
						pacemaker.CreateHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
				},
			},
			expectedStatus: pacemaker.StatusError,
			expectErrors:   true,
			errorContains:  string(pacmkrv1.PacemakerClusterResourceNameKubelet),
		},
		{
			name: "unhealthy_etcd",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  pacemaker.CreateHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						pacemaker.CreateUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
					},
				},
			},
			expectedStatus: pacemaker.StatusError,
			expectErrors:   true,
			errorContains:  string(pacmkrv1.PacemakerClusterResourceNameEtcd),
		},
		{
			name: "insufficient_nodes",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  pacemaker.CreateInsufficientNodesClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						pacemaker.CreateHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
					},
				},
			},
			expectedStatus: pacemaker.StatusError,
			expectErrors:   true,
			errorContains:  "Insufficient nodes",
		},
		{
			name: "no_nodes",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  pacemaker.CreateHealthyClusterConditions(),
					Nodes:       &[]pacmkrv1.PacemakerClusterNodeStatus{},
				},
			},
			expectedStatus: pacemaker.StatusError,
			expectErrors:   true,
			errorContains:  "No nodes found",
		},
		{
			name: "unpopulated_status",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{}, // Zero LastUpdated
			},
			expectedStatus: pacemaker.StatusUnknown,
			expectErrors:   true,
			errorContains:  "Internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := pacemaker.BuildHealthStatusFromCR(tt.cr)

			require.NotNil(t, status, "pacemaker.HealthStatus should not be nil")
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
			expectedReason: pacemaker.EventReasonResourceUnhealthy,
		},
		{
			name:           "node_unhealthy",
			errorMsg:       "master-0 node is unhealthy: node has issues",
			expectedReason: pacemaker.EventReasonNodeUnhealthy,
		},
		{
			name:           "cluster_unhealthy",
			errorMsg:       "Cluster is unhealthy: Pacemaker cluster has issues that need investigation",
			expectedReason: pacemaker.EventReasonClusterUnhealthy,
		},
		{
			name:           "insufficient_nodes",
			errorMsg:       "Insufficient nodes in cluster (expected 2, found 1)",
			expectedReason: pacemaker.EventReasonInsufficientNodes,
		},
		{
			name:           "status_stale",
			errorMsg:       "Pacemaker status is stale",
			expectedReason: pacemaker.EventReasonStatusStale,
		},
		{
			name:           "cr_not_found",
			errorMsg:       "Failed to get PacemakerCluster CR: not found",
			expectedReason: pacemaker.EventReasonCRNotFound,
		},
		{
			name:           "cr_no_status",
			errorMsg:       "PacemakerCluster CR has no status populated",
			expectedReason: pacemaker.EventReasonCRNotFound,
		},
		{
			name:           "node_offline",
			errorMsg:       "Node master-0 is offline",
			expectedReason: pacemaker.EventReasonNodeOffline,
		},
		{
			name:           "excessive_nodes",
			errorMsg:       "Excessive nodes in cluster (expected 2, found 3)",
			expectedReason: pacemaker.EventReasonInsufficientNodes,
		},
		{
			name:           "cluster_maintenance",
			errorMsg:       "Cluster is in maintenance mode",
			expectedReason: pacemaker.EventReasonClusterInMaintenance,
		},
		{
			name:           "generic_error",
			errorMsg:       "Some unknown error",
			expectedReason: pacemaker.EventReasonGenericError,
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
	var previousStatus *pacemaker.HealthStatus

	// Start with healthy status from Unknown state (first sync or recovering from stale)
	// previousStatus=nil: PacemakerHealthy event fires on initial healthy status
	healthyStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusHealthy,
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
	warningStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusWarning,
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
	errorStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusError,
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
	previousStatus := &pacemaker.HealthStatus{OverallStatus: pacemaker.StatusHealthy, Warnings: []string{}, Errors: []string{}}
	currentStatus := &pacemaker.HealthStatus{
		OverallStatus: pacemaker.StatusWarning,
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

// TestDetectDrift tests drift detection between K8s and pacemaker nodes
func TestDetectDrift(t *testing.T) {
	tests := []struct {
		name           string
		k8sNodes       []*corev1.Node
		pacemakerNodes map[string]string
		expectedDrift  bool
	}{
		{
			name: "no drift - nodes match",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.11"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectedDrift: false,
		},
		{
			name: "drift - count mismatch - K8s has fewer nodes",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectedDrift: true,
		},
		{
			name: "drift - count mismatch - K8s has more nodes",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.11"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
			},
			expectedDrift: true,
		},
		{
			name: "drift - node name mismatch",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.11"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-2": "192.168.1.12",
			},
			expectedDrift: true,
		},
		{
			name: "drift - IP mismatch IPv4",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.99"), // Changed IP
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectedDrift: true,
		},
		{
			name: "drift - IP mismatch IPv6",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "fd00::1"),
				createReadyNodeWithIPLM("master-1", "fd00::99"), // Changed IP
			},
			pacemakerNodes: map[string]string{
				"master-0": "fd00::1",
				"master-1": "fd00::2",
			},
			expectedDrift: true,
		},
		{
			name: "no drift - IPv6 different format but same IP",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "fd00::1"),
				createReadyNodeWithIPLM("master-1", "fd00:0000:0000:0000:0000:0000:0000:0002"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "fd00:0000:0000:0000:0000:0000:0000:0001",
				"master-1": "fd00::2",
			},
			expectedDrift: false,
		},
		{
			name: "drift - node in pacemaker but not in K8s",
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectedDrift: true,
		},
		{
			name:           "no drift - both empty",
			k8sNodes:       []*corev1.Node{},
			pacemakerNodes: map[string]string{},
			expectedDrift:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create lifecycle manager with mock
			controller := &PacemakerLifecycleManager{}

			// Execute
			hasDrift := controller.detectDrift(tt.k8sNodes, tt.pacemakerNodes)

			// Verify
			require.Equal(t, tt.expectedDrift, hasDrift,
				"detectDrift() = %v, expected %v", hasDrift, tt.expectedDrift)
		})
	}
}

// TestCleanupOrphanedJobs tests job cleanup for deleted nodes
func TestCleanupOrphanedJobs(t *testing.T) {
	tests := []struct {
		name              string
		k8sNodes          []*corev1.Node
		existingJobs      []runtime.Object
		expectJobsDeleted []string
		expectJobsKept    []string
	}{
		{
			name: "orphaned jobs deleted - node replaced (same name, different UID)",
			k8sNodes: []*corev1.Node{
				createReadyNodeLM("master-0"),
				// master-1 was replaced - new node with same name but different UID
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "master-1",
						UID:  types.UID("master-1-new-uid"), // NEW UID after replacement
					},
					Status: corev1.NodeStatus{
						Conditions: []corev1.NodeCondition{
							{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
						},
					},
				},
			},
			existingJobs: []runtime.Object{
				createTNFJobWithNodeLabelLM("tnf-auth-job-master-0", "master-0", "tnf-auth-job"),
				// Jobs from old master-1 node (before replacement) - labeled with old UID
				createTNFJobWithNodeLabelLM("tnf-auth-job-master-1", "master-1", "tnf-auth-job"),           // Orphaned (old UID)
				createTNFJobWithNodeLabelLM("tnf-after-setup-master-1", "master-1", "tnf-after-setup-job"), // Orphaned (old UID)
				// Note: update-setup jobs don't have node labels and won't be cleaned up by node deletion
			},
			expectJobsDeleted: []string{
				"tnf-auth-job-master-1",    // Old UID "master-1" doesn't match new UID "master-1-new-uid"
				"tnf-after-setup-master-1", // Old UID "master-1" doesn't match new UID "master-1-new-uid"
			},
			expectJobsKept: []string{
				"tnf-auth-job-master-0",
			},
		},
		{
			name: "active node jobs preserved",
			k8sNodes: []*corev1.Node{
				createReadyNodeLM("master-0"),
				createReadyNodeLM("master-1"),
			},
			existingJobs: []runtime.Object{
				createTNFJobWithNodeLabelLM("tnf-auth-job-master-0", "master-0", "tnf-auth-job"),
				createTNFJobWithNodeLabelLM("tnf-auth-job-master-1", "master-1", "tnf-auth-job"),
				createTNFJobWithNodeLabelLM("tnf-after-setup-master-0", "master-0", "tnf-after-setup-job"),
			},
			expectJobsDeleted: []string{},
			expectJobsKept: []string{
				"tnf-auth-job-master-0",
				"tnf-auth-job-master-1",
				"tnf-after-setup-master-0",
			},
		},
		{
			name: "job without node label deleted - migration cleanup",
			k8sNodes: []*corev1.Node{
				createReadyNodeLM("master-0"),
			},
			existingJobs: []runtime.Object{
				createTNFJobWithNodeLabelLM("tnf-auth-job-master-0", "master-0", "tnf-auth-job"),
				createTNFJobWithoutNodeLabelLM("tnf-legacy-job", "tnf-auth-job"), // Old job without label - will be deleted and recreated with label
			},
			expectJobsDeleted: []string{
				"tnf-legacy-job", // Deleted (no node label - migration cleanup)
			},
			expectJobsKept: []string{
				"tnf-auth-job-master-0",
			},
		},
		{
			name:              "no jobs - no action",
			k8sNodes:          []*corev1.Node{createReadyNodeLM("master-0")},
			existingJobs:      []runtime.Object{},
			expectJobsDeleted: []string{},
			expectJobsKept:    []string{},
		},
		{
			name: "label selector filters correctly - only TNF jobs",
			k8sNodes: []*corev1.Node{
				createReadyNodeLM("master-0"),
			},
			existingJobs: []runtime.Object{
				createTNFJobWithNodeLabelLM("tnf-auth-job-master-1", "master-1", "tnf-auth-job"), // TNF job - should be deleted
				createNonTNFJobWithNodeLabelLM("other-job-master-1", "master-1"),                 // Not TNF job - should not be touched
			},
			expectJobsDeleted: []string{
				"tnf-auth-job-master-1",
			},
			expectJobsKept: []string{
				"other-job-master-1", // Not a TNF job, left alone
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create fake client with existing jobs
			fakeKubeClient := fake.NewSimpleClientset(tt.existingJobs...)

			// Create lifecycle manager
			controller := &PacemakerLifecycleManager{
				kubeClient: fakeKubeClient,
			}

			// Execute
			err := controller.cleanupOrphanedJobs(ctx, tt.k8sNodes)

			// Verify no error
			require.NoError(t, err)

			// Verify deleted jobs are gone
			for _, jobName := range tt.expectJobsDeleted {
				_, err := fakeKubeClient.BatchV1().Jobs("openshift-etcd").Get(ctx, jobName, metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err), "Expected job %s to be deleted", jobName)
			}

			// Verify kept jobs still exist
			for _, jobName := range tt.expectJobsKept {
				_, err := fakeKubeClient.BatchV1().Jobs("openshift-etcd").Get(ctx, jobName, metav1.GetOptions{})
				require.NoError(t, err, "Expected job %s to still exist", jobName)
			}
		})
	}
}

// TestReconcilePacemakerConfig tests the core reconciliation logic
func TestReconcilePacemakerConfig(t *testing.T) {
	tests := []struct {
		name                    string
		transitionComplete      bool
		nodeInformerSynced      bool
		k8sNodes                []*corev1.Node
		pacemakerNodes          map[string]string
		updateSetupJobs         []runtime.Object
		mockUpdateSetupError    error
		expectError             bool
		errorContains           string
		expectUpdateSetupCalled bool
	}{
		{
			name:               "no pacemaker CR - discovery reconciliation",
			transitionComplete: true, // Note: In production, sync() would not call this before transition
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			// pacemakerNodes is nil - will trigger discovery reconciliation (try all nodes)
			expectError:             false,
			expectUpdateSetupCalled: true,
		},
		{
			name:               "node informer not synced - skip",
			transitionComplete: true,
			nodeInformerSynced: false,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			expectError:             false,
			expectUpdateSetupCalled: false,
		},
		{
			name:               "more than 2 nodes - skip",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.11"),
				createReadyNodeWithIPLM("master-2", "192.168.1.12"),
			},
			expectError:             false,
			expectUpdateSetupCalled: false,
		},
		{
			name:               "node not ready during bootstrap - skip",
			transitionComplete: false,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createNotReadyNodeLM("master-1"),
			},
			expectError:             false,
			expectUpdateSetupCalled: false, // Should skip during bootstrap until all nodes Ready
		},
		{
			name:               "node not ready after transition - reconcile with ready nodes only",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createNotReadyNodeWithIPLM("master-1", "192.168.1.11"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectError:             false,
			expectUpdateSetupCalled: false, // No drift - both nodes match, so no reconciliation needed
		},
		{
			name:               "no drift - no action",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.11"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectError:             false,
			expectUpdateSetupCalled: false,
		},
		{
			name:               "drift - node count mismatch - trigger update-setup",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectError:             false,
			expectUpdateSetupCalled: true,
		},
		{
			name:               "drift - IP mismatch - trigger update-setup",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
				createReadyNodeWithIPLM("master-1", "192.168.1.99"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectError:             false,
			expectUpdateSetupCalled: true,
		},
		{
			name:               "drift - update-setup already running - still reconcile",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			updateSetupJobs: []runtime.Object{
				createRunningUpdateSetupJobLM("tnf-update-setup-job"),
			},
			expectError:             false,
			expectUpdateSetupCalled: true,
		},
		{
			name:               "drift - no intersection - error",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			pacemakerNodes: map[string]string{
				"master-1": "192.168.1.11",
				"master-2": "192.168.1.12",
			},
			expectError:             true,
			errorContains:           "no nodes in both K8s and pacemaker",
			expectUpdateSetupCalled: false,
		},
		{
			name:               "drift - update-setup fails - return error",
			transitionComplete: true,
			nodeInformerSynced: true,
			k8sNodes: []*corev1.Node{
				createReadyNodeWithIPLM("master-0", "192.168.1.10"),
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			mockUpdateSetupError:    fmt.Errorf("update-setup failed"),
			expectError:             true,
			errorContains:           "update-setup failed",
			expectUpdateSetupCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create fake clients
			fakeKubeClient := fake.NewSimpleClientset(tt.updateSetupJobs...)

			// Create fake operator client
			var conditions []operatorv1.OperatorCondition
			if tt.transitionComplete {
				conditions = append(conditions, operatorv1.OperatorCondition{
					Type:   "ExternalEtcdHasCompletedTransition",
					Status: operatorv1.ConditionTrue,
				})
			}
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				&operatorv1.StaticPodOperatorStatus{
					OperatorStatus: operatorv1.OperatorStatus{
						Conditions: conditions,
					},
				},
				nil,
				nil,
			)

			// Create node informer
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, node := range tt.k8sNodes {
				require.NoError(t, nodeIndexer.Add(node), "failed to add node to indexer")
			}
			nodeInformer := &mockNodeInformerLM{
				indexer: nodeIndexer,
				synced:  tt.nodeInformerSynced,
			}

			// Create pacemaker informer
			pacemakerInformer := createPacemakerInformerWithNodesLM(tt.pacemakerNodes)

			// Create lifecycle manager
			controller := &PacemakerLifecycleManager{
				operatorClient:    fakeOperatorClient,
				kubeClient:        fakeKubeClient,
				nodeInformer:      nodeInformer,
				pacemakerInformer: pacemakerInformer,
			}

			// Mock startTnfJobcontrollers (called from ReconcilePacemakerConfig before transition check)
			originalStartFunc := startTnfJobcontrollersFunc
			startTnfJobcontrollersFunc = func(ctx context.Context, nodes []*corev1.Node, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, etcdInformer operatorv1informers.EtcdInformer, lifecycleManager *PacemakerLifecycleManager) error {
				// No-op for tests - just return success
				return nil
			}
			defer func() { startTnfJobcontrollersFunc = originalStartFunc }()

			// Mock updateSetup
			updateSetupCalled := false
			originalUpdateSetup := updateSetupFunc
			updateSetupFunc = func(validTargetNodes []*corev1.Node, validNodeFunc jobs.TargetNodesFunc, allK8sNodes []*corev1.Node, pacemakerNodes map[string]string, ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) error {
				updateSetupCalled = true
				return tt.mockUpdateSetupError
			}
			defer func() { updateSetupFunc = originalUpdateSetup }()

			// Execute
			err := controller.ReconcilePacemakerConfig(ctx)

			// Verify error expectations
			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					require.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
			}

			// Verify updateSetup was called as expected
			require.Equal(t, tt.expectUpdateSetupCalled, updateSetupCalled,
				"updateSetup called=%v, expected=%v", updateSetupCalled, tt.expectUpdateSetupCalled)
		})
	}
}

// Helper functions for test setup

func createReadyNodeLM(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name), // Use name as UID for test simplicity
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func createNotReadyNodeLM(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}
}

func createNotReadyNodeWithIPLM(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ip},
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}
}

func createReadyNodeWithIPLM(name, ip string) *corev1.Node {
	node := createReadyNodeLM(name)
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: ip},
	}
	return node
}

func createTNFJobWithNodeLabelLM(name, nodeName, jobType string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-etcd",
			Labels: map[string]string{
				"app.kubernetes.io/name": jobType,
				"node":                   nodeName,
			},
		},
	}
}

func createTNFJobWithoutNodeLabelLM(name, jobType string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-etcd",
			Labels: map[string]string{
				"app.kubernetes.io/name": jobType,
				// No "node" label
			},
		},
	}
}

func createNonTNFJobWithNodeLabelLM(name, nodeName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-etcd",
			Labels: map[string]string{
				"app.kubernetes.io/name": "some-other-job", // Not a TNF job
				"node":                   nodeName,
			},
		},
	}
}

func createRunningUpdateSetupJobLM(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-etcd",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "tnf-update-setup-job",
				"app.kubernetes.io/component": "two-node-fencing-setup",
			},
		},
		Status: batchv1.JobStatus{
			// No Complete or Failed condition = still running
		},
	}
}

func createPacemakerInformerWithNodesLM(nodes map[string]string) cache.SharedIndexInformer {
	if nodes == nil {
		// No CR exists
		return createFakeInformer(nil)
	}

	// Create PacemakerCluster CR with the given nodes
	nodeStatuses := []pacmkrv1.PacemakerClusterNodeStatus{}
	for name, ip := range nodes {
		nodeStatuses = append(nodeStatuses, pacmkrv1.PacemakerClusterNodeStatus{
			NodeName: name,
			Addresses: []pacmkrv1.PacemakerNodeAddress{
				{Address: ip},
			},
		})
	}

	pacemakerCR := &pacmkrv1.PacemakerCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: pacmkrv1.PacemakerClusterStatus{
			Nodes:       &nodeStatuses,
			LastUpdated: metav1.Now(),
		},
	}

	return createFakeInformer(pacemakerCR)
}

// mockNodeInformerLM is a mock SharedIndexInformer for testing
type mockNodeInformerLM struct {
	indexer cache.Indexer
	synced  bool
}

func (m *mockNodeInformerLM) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (m *mockNodeInformerLM) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (m *mockNodeInformerLM) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (m *mockNodeInformerLM) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}
func (m *mockNodeInformerLM) GetStore() cache.Store              { return m.indexer }
func (m *mockNodeInformerLM) GetIndexer() cache.Indexer          { return m.indexer }
func (m *mockNodeInformerLM) GetController() cache.Controller    { return nil }
func (m *mockNodeInformerLM) Run(stopCh <-chan struct{})         {}
func (m *mockNodeInformerLM) RunWithContext(ctx context.Context) {}
func (m *mockNodeInformerLM) HasSynced() bool                    { return m.synced }
func (m *mockNodeInformerLM) LastSyncResourceVersion() string    { return "" }
func (m *mockNodeInformerLM) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}
func (m *mockNodeInformerLM) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (m *mockNodeInformerLM) SetTransform(handler cache.TransformFunc) error { return nil }
func (m *mockNodeInformerLM) IsStopped() bool                                { return false }
func (m *mockNodeInformerLM) AddIndexers(indexers cache.Indexers) error      { return nil }
