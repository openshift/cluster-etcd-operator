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
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
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
		pacemakerInformer: createFakeInformer(nil),
		recordedEvents:    make(map[string]time.Time),
	}
}

// createFakeInformer creates a fake SharedIndexInformer with a store containing the given object.
// If obj is nil, the store will be empty.
func createFakeInformer(obj *v1alpha1.PacemakerCluster) cache.SharedIndexInformer {
	store := cache.NewStore(func(obj interface{}) (string, error) {
		if pc, ok := obj.(*v1alpha1.PacemakerCluster); ok {
			return pc.Name, nil
		}
		return "", fmt.Errorf("object is not a PacemakerCluster")
	})
	if obj != nil {
		_ = store.Add(obj)
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
func createTestHealthCheckWithMockStatus(t *testing.T, mockStatus *v1alpha1.PacemakerCluster) *HealthCheck {
	return &HealthCheck{
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

func TestNewHealthCheck(t *testing.T) {
	controller, informer, err := NewHealthCheck(
		createFakeOperatorClient(),
		fake.NewSimpleClientset(),
		events.NewInMemoryRecorder("test", clock.RealClock{}),
		&rest.Config{Host: "https://localhost:6443"},
	)

	require.NoError(t, err, "NewHealthCheck should not return an error")
	require.NotNil(t, controller, "Controller should not be nil")
	require.NotNil(t, informer, "Informer should not be nil")
}

func TestHealthCheck_getPacemakerStatus(t *testing.T) {
	tests := []struct {
		name           string
		mockStatus     *v1alpha1.PacemakerCluster
		expectedStatus HealthStatusValue
		expectErrors   bool
		expectWarnings bool
	}{
		{
			name: "healthy_status",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: statusHealthy,
			expectErrors:   false,
			expectWarnings: false,
		},
		{
			name: "stale_status",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // > 5 min threshold
				},
			},
			expectedStatus: statusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "nil_status",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{}, // Status not populated yet (zero LastUpdated)
			},
			expectedStatus: statusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "insufficient_nodes",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createInsufficientNodesClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "unhealthy_kubelet",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, v1alpha1.PacemakerClusterResourceNameKubelet),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "unhealthy_etcd",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, v1alpha1.PacemakerClusterResourceNameEtcd),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "unhealthy_etcd_first_node",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, v1alpha1.PacemakerClusterResourceNameEtcd),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
					LastUpdated: metav1.Now(),
				},
			},
			expectedStatus: statusError,
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

	mockStatus := &v1alpha1.PacemakerCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "PacemakerCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: v1alpha1.PacemakerClusterStatus{
			Conditions: createHealthyClusterConditions(),
			Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
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
	require.Equal(t, statusHealthy, current1.OverallStatus, "First call should return healthy status")

	// Second call with same timestamp should return nil (no change)
	current2, _, err2 := controller.getPacemakerStatus(ctx)
	require.NoError(t, err2, "Second getPacemakerStatus should not return an error")
	require.Nil(t, current2, "Second call with unchanged timestamp should return nil")
}

func TestHealthCheck_updateOperatorStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   *HealthStatus
		previous *HealthStatus
	}{
		{
			name: "error_status_sets_degraded",
			status: &HealthStatus{
				OverallStatus: statusError,
				Warnings:      []string{"Test warning"},
				Errors:        []string{"Test error"},
			},
			previous: &HealthStatus{
				OverallStatus: statusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
		},
		{
			name: "healthy_status_clears_degraded",
			status: &HealthStatus{
				OverallStatus: statusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			previous: &HealthStatus{
				OverallStatus: statusHealthy,
				CRLastUpdated: time.Now().Add(-1 * time.Minute),
			},
		},
		{
			name: "warning_status_clears_degraded",
			status: &HealthStatus{
				OverallStatus: statusWarning,
				Warnings:      []string{"Test warning"},
				Errors:        []string{},
			},
			previous: &HealthStatus{
				OverallStatus: statusHealthy,
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
	previousHealthy := &HealthStatus{OverallStatus: statusHealthy, Warnings: []string{}, Errors: []string{}}
	currentWithWarnings := &HealthStatus{
		OverallStatus: statusWarning,
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
		OverallStatus: statusHealthy,
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
		OverallStatus: statusError,
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
		OverallStatus: statusWarning,
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
		OverallStatus: statusWarning,
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
		OverallStatus: statusHealthy,
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

func TestHealthCheck_buildHealthStatusFromCR(t *testing.T) {
	tests := []struct {
		name           string
		cr             *v1alpha1.PacemakerCluster
		expectedStatus HealthStatusValue
		expectErrors   bool
		errorContains  string // Optional: check error contains this substring
	}{
		{
			name: "healthy_cluster",
			cr: &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
				},
			},
			expectedStatus: statusHealthy,
			expectErrors:   false,
		},
		{
			name: "unhealthy_kubelet",
			cr: &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, v1alpha1.PacemakerClusterResourceNameKubelet),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					},
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			errorContains:  string(v1alpha1.PacemakerClusterResourceNameKubelet),
		},
		{
			name: "unhealthy_etcd",
			cr: &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, v1alpha1.PacemakerClusterResourceNameEtcd),
					},
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			errorContains:  string(v1alpha1.PacemakerClusterResourceNameEtcd),
		},
		{
			name: "insufficient_nodes",
			cr: &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createInsufficientNodesClusterConditions(),
					Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
					},
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			errorContains:  "Insufficient nodes",
		},
		{
			name: "no_nodes",
			cr: &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes:       &[]v1alpha1.PacemakerClusterNodeStatus{},
				},
			},
			expectedStatus: statusError,
			expectErrors:   true,
			errorContains:  "No nodes found",
		},
		{
			name: "unpopulated_status",
			cr: &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{}, // Zero LastUpdated
			},
			expectedStatus: statusUnknown,
			expectErrors:   true,
			errorContains:  "Internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &HealthCheck{}
			status := controller.buildHealthStatusFromCR(tt.cr)

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

func TestHealthCheck_checkClusterConditions(t *testing.T) {
	tests := []struct {
		name          string
		conditions    []metav1.Condition
		nodes         *[]v1alpha1.PacemakerClusterNodeStatus
		expectErrors  bool
		errorContains []string // Multiple strings to check
	}{
		{
			name:       "healthy_cluster",
			conditions: createHealthyClusterConditions(),
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors: false,
		},
		{
			name:       "insufficient_nodes",
			conditions: createInsufficientNodesClusterConditions(),
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true,
			errorContains: []string{"Insufficient nodes"},
		},
		{
			name:       "excessive_nodes",
			conditions: createExcessiveNodesClusterConditions(),
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
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
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors:  true,
			errorContains: []string{"maintenance mode"},
		},
		{
			name:       "empty_conditions",
			conditions: []metav1.Condition{},
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true, // Empty conditions means we can't verify cluster health
			errorContains: []string{"No cluster conditions available"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &HealthCheck{}
			pacemakerStatus := &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: tt.conditions,
					Nodes:      tt.nodes,
				},
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			controller.checkClusterConditions(pacemakerStatus, status)

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

func TestHealthCheck_checkNodeStatuses(t *testing.T) {
	tests := []struct {
		name          string
		nodes         *[]v1alpha1.PacemakerClusterNodeStatus
		expectErrors  bool
		errorContains string
	}{
		{
			name: "healthy_nodes",
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors: false,
		},
		{
			name: "unhealthy_kubelet",
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, v1alpha1.PacemakerClusterResourceNameKubelet),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors:  true,
			errorContains: string(v1alpha1.PacemakerClusterResourceNameKubelet),
		},
		{
			name: "unhealthy_etcd",
			nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				createUnhealthyNodeStatus("master-1", []string{"192.168.1.11"}, v1alpha1.PacemakerClusterResourceNameEtcd),
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true,
			errorContains: string(v1alpha1.PacemakerClusterResourceNameEtcd),
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
			controller := &HealthCheck{}
			pacemakerStatus := &v1alpha1.PacemakerCluster{
				Status: v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes:      tt.nodes,
				},
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			controller.checkNodeStatuses(pacemakerStatus, status)

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

func TestHealthCheck_checkNodeStatuses_MultipleUnhealthyResources(t *testing.T) {
	controller := &HealthCheck{}

	// Create a node with multiple unhealthy resources
	node := createHealthyNodeStatus("master-0", []string{"192.168.1.10"})
	// Mark node as unhealthy
	for i := range node.Conditions {
		if node.Conditions[i].Type == v1alpha1.NodeHealthyConditionType {
			node.Conditions[i].Status = metav1.ConditionFalse
			node.Conditions[i].Reason = v1alpha1.NodeHealthyReasonUnhealthy
		}
	}
	// Mark kubelet and etcd as unhealthy
	for i := range node.Resources {
		if node.Resources[i].Name == v1alpha1.PacemakerClusterResourceNameKubelet ||
			node.Resources[i].Name == v1alpha1.PacemakerClusterResourceNameEtcd {
			node.Resources[i].Conditions = createUnhealthyResourceConditions()
		}
	}

	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: v1alpha1.PacemakerClusterStatus{
			Nodes: &[]v1alpha1.PacemakerClusterNodeStatus{
				node,
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatuses(pacemakerStatus, status)

	// Should have 1 consolidated error for the unhealthy node (includes all resource issues)
	require.Len(t, status.Errors, 1, "Should have 1 consolidated error for unhealthy node")

	// Verify the single error mentions both resources
	nodeError := status.Errors[0]
	require.Contains(t, nodeError, "master-0", "Error should mention the node name")
	require.Contains(t, nodeError, "node is unhealthy", "Error should indicate node is unhealthy")
	require.Contains(t, nodeError, string(v1alpha1.PacemakerClusterResourceNameKubelet), "Error should mention Kubelet")
	require.Contains(t, nodeError, string(v1alpha1.PacemakerClusterResourceNameEtcd), "Error should mention Etcd")
}

func TestHealthCheck_getResourceIssue(t *testing.T) {
	controller := &HealthCheck{}

	tests := []struct {
		name       string
		conditions []metav1.Condition
		wantIssue  string
	}{
		{
			name:       "failed takes priority",
			conditions: []metav1.Condition{{Type: v1alpha1.ResourceOperationalConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "has failed",
		},
		{
			name:       "stopped",
			conditions: []metav1.Condition{{Type: v1alpha1.ResourceStartedConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is stopped",
		},
		{
			name:       "inactive",
			conditions: []metav1.Condition{{Type: v1alpha1.ResourceActiveConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is not active",
		},
		{
			name:       "unmanaged",
			conditions: []metav1.Condition{{Type: v1alpha1.ResourceManagedConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is unmanaged",
		},
		{
			name:       "fallback to generic unhealthy",
			conditions: []metav1.Condition{{Type: v1alpha1.ResourceHealthyConditionType, Status: metav1.ConditionFalse}},
			wantIssue:  "is unhealthy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := controller.getResourceIssue(tt.conditions)
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
		OverallStatus: statusHealthy,
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
		OverallStatus: statusWarning,
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
		OverallStatus: statusError,
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
	previousStatus := &HealthStatus{OverallStatus: statusHealthy, Warnings: []string{}, Errors: []string{}}
	currentStatus := &HealthStatus{
		OverallStatus: statusWarning,
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

func TestHealthCheck_getNodeConditionErrorsAndWarnings(t *testing.T) {
	controller := &HealthCheck{}
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
				{Type: v1alpha1.NodeInServiceConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeInServiceReasonInMaintenance, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in maintenance mode"},
			expectedWarnings: []string{},
		},
		{
			name: "in_standby_mode",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeActiveConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeActiveReasonStandby, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in standby mode"},
			expectedWarnings: []string{},
		},
		{
			name: "unclean_state",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeCleanConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeCleanReasonUnclean, LastTransitionTime: now},
			},
			expectedErrors:   []string{"unclean state (fencing/communication issue)"},
			expectedWarnings: []string{},
		},
		{
			name: "not_a_member",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeMemberConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeMemberReasonNotMember, LastTransitionTime: now},
			},
			expectedErrors:   []string{"not a cluster member"},
			expectedWarnings: []string{},
		},
		{
			name: "fencing_unavailable",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeFencingAvailableConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeFencingAvailableReasonUnavailable, LastTransitionTime: now},
			},
			expectedErrors:   []string{"fencing unavailable (no agents running)"},
			expectedWarnings: []string{},
		},
		{
			name: "fencing_unhealthy_but_available_is_warning",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue, Reason: v1alpha1.NodeFencingAvailableReasonAvailable, LastTransitionTime: now},
				{Type: v1alpha1.NodeFencingHealthyConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeFencingHealthyReasonUnhealthy, LastTransitionTime: now},
			},
			expectedErrors:   []string{},
			expectedWarnings: []string{"fencing at risk"},
		},
		{
			name: "pending_state_no_error_or_warning",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeReadyConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeReadyReasonPending, LastTransitionTime: now},
			},
			expectedErrors:   []string{},
			expectedWarnings: []string{},
		},
		{
			name: "multiple_errors",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeInServiceConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				{Type: v1alpha1.NodeActiveConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				{Type: v1alpha1.NodeCleanConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in maintenance mode", "in standby mode", "unclean state (fencing/communication issue)"},
			expectedWarnings: []string{},
		},
		{
			name: "error_and_warning_together",
			conditions: []metav1.Condition{
				{Type: v1alpha1.NodeInServiceConditionType, Status: metav1.ConditionFalse, LastTransitionTime: now},
				{Type: v1alpha1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue, Reason: v1alpha1.NodeFencingAvailableReasonAvailable, LastTransitionTime: now},
				{Type: v1alpha1.NodeFencingHealthyConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeFencingHealthyReasonUnhealthy, LastTransitionTime: now},
			},
			expectedErrors:   []string{"in maintenance mode"},
			expectedWarnings: []string{"fencing at risk"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Check errors and warnings using the separate functions
			errors := controller.getNodeConditionErrors(tt.conditions)
			warnings := controller.getFencingWarnings(tt.conditions)

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

// TestHealthCheck_FencingRedundancyWarning tests fencing redundancy warnings in various scenarios.
// Fencing redundancy degraded (FencingHealthy=False but FencingAvailable=True) should be
// reported as a warning, not an error, and should be captured regardless of overall node health.
func TestHealthCheck_FencingRedundancyWarning(t *testing.T) {
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
			nodeHealthy:            true, // Node is healthy overall, but fencing redundancy is degraded
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
			controller := &HealthCheck{}
			now := metav1.Now()

			// Build conditions based on test case
			nodeHealthyStatus := metav1.ConditionFalse
			nodeHealthyReason := v1alpha1.NodeHealthyReasonUnhealthy
			if tt.nodeHealthy {
				nodeHealthyStatus = metav1.ConditionTrue
				nodeHealthyReason = v1alpha1.NodeHealthyReasonHealthy
			}

			inServiceStatus := metav1.ConditionTrue
			inServiceReason := v1alpha1.NodeInServiceReasonInService
			if tt.inMaintenance {
				inServiceStatus = metav1.ConditionFalse
				inServiceReason = v1alpha1.NodeInServiceReasonInMaintenance
			}

			conditions := []metav1.Condition{
				{Type: v1alpha1.NodeHealthyConditionType, Status: nodeHealthyStatus, Reason: nodeHealthyReason, LastTransitionTime: now},
				{Type: v1alpha1.NodeOnlineConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: v1alpha1.NodeInServiceConditionType, Status: inServiceStatus, Reason: inServiceReason, LastTransitionTime: now},
				{Type: v1alpha1.NodeActiveConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: v1alpha1.NodeReadyConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: v1alpha1.NodeCleanConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				{Type: v1alpha1.NodeMemberConditionType, Status: metav1.ConditionTrue, LastTransitionTime: now},
				// Fencing available but not healthy - warning
				{Type: v1alpha1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue, Reason: v1alpha1.NodeFencingAvailableReasonAvailable, LastTransitionTime: now},
				{Type: v1alpha1.NodeFencingHealthyConditionType, Status: metav1.ConditionFalse, Reason: v1alpha1.NodeFencingHealthyReasonUnhealthy, LastTransitionTime: now},
			}

			healthyResources := []v1alpha1.PacemakerClusterResourceStatus{
				{Name: v1alpha1.PacemakerClusterResourceNameKubelet, Conditions: createHealthyResourceConditions()},
				{Name: v1alpha1.PacemakerClusterResourceNameEtcd, Conditions: createHealthyResourceConditions()},
			}

			node := v1alpha1.PacemakerClusterNodeStatus{
				NodeName:   "master-0",
				Conditions: conditions,
				Resources:  healthyResources,
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			controller.checkNodeConditions(node, status)

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
				OverallStatus: statusWarning,
				Warnings:      []string{"master-0: " + msgFencingRedundancyLost},
				Errors:        []string{},
			},
			currentStatus: &HealthStatus{
				OverallStatus: statusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			expectHealthyEvent:    false, // Not degraded before
			expectWarningsCleared: true,  // Had warnings before
		},
		{
			name: "error_without_warnings_to_healthy",
			previousStatus: &HealthStatus{
				OverallStatus: statusError,
				Warnings:      []string{},
				Errors:        []string{"Critical error"},
			},
			currentStatus: &HealthStatus{
				OverallStatus: statusHealthy,
				Warnings:      []string{},
				Errors:        []string{},
			},
			expectHealthyEvent:    true,  // Was degraded
			expectWarningsCleared: false, // No warnings before
		},
		{
			name: "error_with_warnings_to_error_without_warnings",
			previousStatus: &HealthStatus{
				OverallStatus: statusError,
				Warnings:      []string{"master-0: " + msgFencingRedundancyLost},
				Errors:        []string{"Critical error"},
			},
			currentStatus: &HealthStatus{
				OverallStatus: statusError,
				Warnings:      []string{},
				Errors:        []string{"Critical error"},
			},
			expectHealthyEvent:    false, // Still degraded
			expectWarningsCleared: true,  // Warnings cleared while still degraded
		},
		{
			name: "error_with_warnings_to_healthy",
			previousStatus: &HealthStatus{
				OverallStatus: statusError,
				Warnings:      []string{"master-0: " + msgFencingRedundancyLost},
				Errors:        []string{"Critical error"},
			},
			currentStatus: &HealthStatus{
				OverallStatus: statusHealthy,
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
