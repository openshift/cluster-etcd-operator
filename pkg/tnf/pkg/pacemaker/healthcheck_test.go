package pacemaker

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	fakerest "k8s.io/client-go/rest/fake"
	"k8s.io/utils/clock"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
)

// Test helper functions
func createTestHealthCheck() *HealthCheck {
	kubeClient := fake.NewSimpleClientset()
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
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
	eventRecorder := events.NewInMemoryRecorder("test", clock.RealClock{})

	// Note: This creates a HealthCheck with mock REST client
	// In a real scenario, we'd need to mock the Pacemaker CR
	return &HealthCheck{
		operatorClient:      operatorClient,
		kubeClient:          kubeClient,
		eventRecorder:       eventRecorder,
		pacemakerRESTClient: nil, // TODO: Mock this for actual tests
		pacemakerInformer:   nil, // TODO: Mock this for actual tests
		recordedEvents:      make(map[string]time.Time),
		previousStatus:      statusUnknown,
	}
}

// createTestHealthCheckWithMockStatus creates a HealthCheck with a mocked Pacemaker CR
func createTestHealthCheckWithMockStatus(t *testing.T, mockStatus *v1alpha1.PacemakerCluster) *HealthCheck {
	kubeClient := fake.NewSimpleClientset()
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
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
	eventRecorder := events.NewInMemoryRecorder("test", clock.RealClock{})

	// Create a fake REST client that returns the mock PacemakerStatus
	scheme := runtime.NewScheme()
	err := v1alpha1.AddToScheme(scheme)
	require.NoError(t, err)

	codec := serializer.NewCodecFactory(scheme)
	fakeClient := &fakerest.RESTClient{
		Client: fakerest.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			body, err := runtime.Encode(codec.LegacyCodec(v1alpha1.SchemeGroupVersion), mockStatus)
			if err != nil {
				return nil, err
			}

			header := http.Header{}
			header.Set("Content-Type", runtime.ContentTypeJSON)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		}),
		NegotiatedSerializer: codec,
		GroupVersion:         v1alpha1.SchemeGroupVersion,
		VersionedAPIPath:     "/apis/" + v1alpha1.SchemeGroupVersion.String(),
	}

	return &HealthCheck{
		operatorClient:      operatorClient,
		kubeClient:          kubeClient,
		eventRecorder:       eventRecorder,
		pacemakerRESTClient: fakeClient,
		pacemakerInformer:   nil, // Not needed for getPacemakerStatus tests
		recordedEvents:      make(map[string]time.Time),
		previousStatus:      statusUnknown,
	}
}

// Helper functions removed: loadTestXML and getRecentFailuresXML
// These were used for XML parsing tests, which have been replaced with CRD-based tests

// Tests
func TestNewHealthCheck(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
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

	livenessChecker := health.NewMultiAlivenessChecker()
	eventRecorder := events.NewInMemoryRecorder("test", clock.RealClock{})

	// Create a minimal rest.Config for testing
	restConfig := &rest.Config{
		Host: "https://localhost:6443",
	}

	controller, informer, err := NewHealthCheck(
		livenessChecker,
		operatorClient,
		kubeClient,
		eventRecorder,
		restConfig,
	)

	require.NoError(t, err, "NewHealthCheck should not return an error")
	require.NotNil(t, controller, "Controller should not be nil")
	require.NotNil(t, informer, "Informer should not be nil")
}

func TestHealthCheck_getPacemakerStatus(t *testing.T) {
	tests := []struct {
		name           string
		mockStatus     *v1alpha1.PacemakerCluster
		expectedStatus string
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
				Status: &v1alpha1.PacemakerClusterStatus{
					Summary: &v1alpha1.PacemakerSummary{
						PacemakerDaemonState: v1alpha1.PacemakerDaemonStateRunning,
						QuorumStatus:         v1alpha1.QuorumStatusQuorate,
					},
					Nodes: []v1alpha1.PacemakerNodeStatus{
						{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
						{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
					},
					Resources: []v1alpha1.PacemakerResourceStatus{
						{Name: "kubelet-clone-0", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
						{Name: "kubelet-clone-1", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
						{Name: "etcd-clone-0", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
						{Name: "etcd-clone-1", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
					},
					CollectionError: "",
					LastUpdated:     metav1.Now(),
				},
			},
			expectedStatus: statusHealthy,
			expectErrors:   false,
			expectWarnings: false,
		},
		{
			name: "collection_error",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: &v1alpha1.PacemakerClusterStatus{
					CollectionError: "Failed to execute pcs command",
					LastUpdated:     metav1.Now(),
				},
			},
			expectedStatus: statusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "collection_error_recent_update",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: &v1alpha1.PacemakerClusterStatus{
					Summary: &v1alpha1.PacemakerSummary{
						PacemakerDaemonState: "running",
						QuorumStatus:         v1alpha1.QuorumStatusQuorate,
					},
					CollectionError: "pcs command failed",
					LastUpdated:     metav1.Time{Time: time.Now().Add(-2 * time.Minute)}, // Recent update
				},
			},
			expectedStatus: statusUnknown,
			expectErrors:   true, // Should report error, not staleness
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
				Status: &v1alpha1.PacemakerClusterStatus{
					Summary: &v1alpha1.PacemakerSummary{
						PacemakerDaemonState: "running",
						QuorumStatus:         v1alpha1.QuorumStatusQuorate,
					},
					CollectionError: "",
					LastUpdated:     metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // > 5 min threshold
				},
			},
			expectedStatus: statusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
		{
			name: "stale_status_with_collection_error",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: &v1alpha1.PacemakerClusterStatus{
					Summary: &v1alpha1.PacemakerSummary{
						PacemakerDaemonState: "running",
						QuorumStatus:         v1alpha1.QuorumStatusQuorate,
					},
					CollectionError: "pcs command failed",
					LastUpdated:     metav1.Time{Time: time.Now().Add(-10 * time.Minute)}, // > 5 min threshold
				},
			},
			expectedStatus: statusUnknown,
			expectErrors:   true, // Should report staleness, not collection error
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
				Status: nil, // Status not populated yet
			},
			expectedStatus: statusUnknown,
			expectErrors:   true,
			expectWarnings: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := createTestHealthCheckWithMockStatus(t, tt.mockStatus)
			ctx := context.TODO()

			status, err := controller.getPacemakerStatus(ctx)

			require.NoError(t, err, "getPacemakerStatus should not return an error")
			require.NotNil(t, status, "HealthStatus should not be nil")
			require.Equal(t, tt.expectedStatus, status.OverallStatus, "Status should match expected")

			if tt.expectErrors {
				require.NotEmpty(t, status.Errors, "Should have errors")
			} else {
				require.Empty(t, status.Errors, "Should not have errors")
			}

			if tt.expectWarnings {
				require.NotEmpty(t, status.Warnings, "Should have warnings")
			} else {
				require.Empty(t, status.Warnings, "Should not have warnings")
			}
		})
	}
}

func TestHealthCheck_updateOperatorStatus(t *testing.T) {
	controller := createTestHealthCheck()
	ctx := context.TODO()

	// Test degraded status
	status := &HealthStatus{
		OverallStatus: statusError,
		Warnings:      []string{"Test warning"},
		Errors:        []string{"Test error"},
	}

	err := controller.updateOperatorStatus(ctx, status)
	require.NoError(t, err, "updateOperatorStatus should not return an error")

	// Test healthy status
	status = &HealthStatus{
		OverallStatus: statusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	err = controller.updateOperatorStatus(ctx, status)
	require.NoError(t, err, "updateOperatorStatus should not return an error")

	// Test warning status (should not update operator condition)
	status = &HealthStatus{
		OverallStatus: statusWarning,
		Warnings:      []string{"Test warning"},
		Errors:        []string{},
	}

	err = controller.updateOperatorStatus(ctx, status)
	require.NoError(t, err, "updateOperatorStatus should not return an error")
}

func TestHealthCheck_recordHealthCheckEvents(t *testing.T) {
	controller := createTestHealthCheck()

	// Test with warnings and errors
	status := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed (rc=1, Thu Sep 18 13:12:00 2025)",
			"Recent fencing event: reboot of master-1 success",
			"Some other warning",
		},
		Errors: []string{"Test error"},
	}

	controller.recordHealthCheckEvents(status)

	// Test with healthy status
	status = &HealthStatus{
		OverallStatus: statusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	controller.recordHealthCheckEvents(status)
}

func TestHealthCheck_eventDeduplication(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// First recording - should create events
	status := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed",
		},
		Errors: []string{"Test error"},
	}
	controller.recordHealthCheckEvents(status)

	// Count events after first recording
	initialEvents := len(recorder.Events())
	require.Greater(t, initialEvents, 0, "Should have recorded events")

	// Immediately record the same events again - should be deduplicated (5 min window)
	controller.recordHealthCheckEvents(status)
	afterDuplicateEvents := len(recorder.Events())
	require.Equal(t, initialEvents, afterDuplicateEvents, "Duplicate events should not be recorded")

	// Record a different error - should create new event
	status.Errors = []string{"Different error"}
	controller.recordHealthCheckEvents(status)
	afterNewErrorEvents := len(recorder.Events())
	require.Greater(t, afterNewErrorEvents, afterDuplicateEvents, "New error should be recorded")

	// Test fencing event deduplication (longer window)
	fencingStatus := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings: []string{
			"Recent fencing event: reboot of master-1 success",
		},
		Errors: []string{},
	}
	controller.recordHealthCheckEvents(fencingStatus)
	afterFencingEvents := len(recorder.Events())
	require.Greater(t, afterFencingEvents, afterNewErrorEvents, "First fencing event should be recorded")

	// Immediately record the same fencing event - should be deduplicated (24 hour window)
	controller.recordHealthCheckEvents(fencingStatus)
	afterFencingDuplicateEvents := len(recorder.Events())
	require.Equal(t, afterFencingEvents, afterFencingDuplicateEvents, "Duplicate fencing event should not be recorded")

	// Test healthy event only recorded on transition from unhealthy
	healthyStatus := &HealthStatus{
		OverallStatus: statusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(healthyStatus)
	afterHealthyEvents := len(recorder.Events())
	require.Greater(t, afterHealthyEvents, afterFencingDuplicateEvents, "Healthy event should be recorded when transitioning from unhealthy")

	// Record healthy again immediately - should NOT be recorded (only on transition)
	controller.recordHealthCheckEvents(healthyStatus)
	afterHealthyDuplicateEvents := len(recorder.Events())
	require.Equal(t, afterHealthyEvents, afterHealthyDuplicateEvents, "Healthy event should not be recorded when already healthy")

	// Transition to unhealthy and back to healthy - should record healthy event
	controller.recordHealthCheckEvents(status) // Become unhealthy
	controller.recordHealthCheckEvents(healthyStatus)
	afterTransitionEvents := len(recorder.Events())
	require.Greater(t, afterTransitionEvents, afterHealthyDuplicateEvents, "Healthy event should be recorded on transition from unhealthy to healthy")
}

func TestHealthCheck_eventDeduplication_DifferentWindows(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Record a resource action warning
	resourceActionStatus := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings: []string{
			"Recent failed resource action: etcd start on master-0 failed",
		},
		Errors: []string{},
	}
	controller.recordHealthCheckEvents(resourceActionStatus)
	initialEvents := len(recorder.Events())
	require.Equal(t, 1, initialEvents, "Should have recorded 1 event")

	// Record a fencing event warning
	fencingStatus := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings: []string{
			"Recent fencing event: reboot of master-0 success",
		},
		Errors: []string{},
	}
	controller.recordHealthCheckEvents(fencingStatus)
	afterFencingEvents := len(recorder.Events())
	require.Equal(t, 2, afterFencingEvents, "Should have recorded fencing event")

	// Verify both events are tracked separately
	controller.recordedEventsMu.Lock()
	require.Len(t, controller.recordedEvents, 2, "Should have 2 events in deduplication map")
	controller.recordedEventsMu.Unlock()

	// Try to record the same events again - both should be deduplicated
	controller.recordHealthCheckEvents(resourceActionStatus)
	controller.recordHealthCheckEvents(fencingStatus)
	afterDuplicates := len(recorder.Events())
	require.Equal(t, afterFencingEvents, afterDuplicates, "Duplicate events should not be recorded")
}

func TestHealthCheck_eventDeduplication_MultipleErrors(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Record multiple errors at once
	status := &HealthStatus{
		OverallStatus: statusError,
		Warnings:      []string{},
		Errors: []string{
			"Node master-0 is not online",
			"kubelet resource not started on all nodes (started on 1/2 nodes)",
			"etcd resource not started on all nodes (started on 1/2 nodes)",
		},
	}
	controller.recordHealthCheckEvents(status)
	initialEvents := len(recorder.Events())
	require.Equal(t, 3, initialEvents, "Should have recorded 3 error events")

	// Record the same errors again - all should be deduplicated
	controller.recordHealthCheckEvents(status)
	afterDuplicates := len(recorder.Events())
	require.Equal(t, initialEvents, afterDuplicates, "All duplicate errors should be deduplicated")

	// Record a subset with one new error
	status.Errors = []string{
		"Node master-0 is not online",  // Duplicate
		"Cluster does not have quorum", // New
	}
	controller.recordHealthCheckEvents(status)
	afterNewError := len(recorder.Events())
	require.Equal(t, initialEvents+1, afterNewError, "Only the new error should be recorded")
}

func TestHealthCheck_eventDeduplication_StatusTransitions(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Start with Unknown status (default)
	require.Equal(t, statusUnknown, controller.previousStatus, "Initial status should be Unknown")

	// Transition from Unknown to Healthy - should record
	healthyStatus := &HealthStatus{
		OverallStatus: statusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(healthyStatus)
	afterFirstHealthy := len(recorder.Events())
	require.Equal(t, 1, afterFirstHealthy, "Healthy event should be recorded on transition from Unknown")

	// Stay healthy - should not record
	controller.recordHealthCheckEvents(healthyStatus)
	require.Equal(t, afterFirstHealthy, len(recorder.Events()), "No event when staying healthy")

	// Transition to Warning - should not record healthy event
	warningStatus := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings:      []string{"Some warning"},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(warningStatus)
	afterWarning := len(recorder.Events())
	require.Greater(t, afterWarning, afterFirstHealthy, "Warning should be recorded")

	// Transition from Warning to Healthy - should record
	controller.recordHealthCheckEvents(healthyStatus)
	afterSecondHealthy := len(recorder.Events())
	require.Equal(t, afterWarning+1, afterSecondHealthy, "Healthy event should be recorded on transition from Warning")

	// Transition to Error
	errorStatus := &HealthStatus{
		OverallStatus: statusError,
		Warnings:      []string{},
		Errors:        []string{"Critical error"},
	}
	controller.recordHealthCheckEvents(errorStatus)
	afterError := len(recorder.Events())
	require.Greater(t, afterError, afterSecondHealthy, "Error should be recorded")

	// Transition from Error to Healthy - should record
	controller.recordHealthCheckEvents(healthyStatus)
	afterThirdHealthy := len(recorder.Events())
	require.Equal(t, afterError+1, afterThirdHealthy, "Healthy event should be recorded on transition from Error")
}

func TestHealthCheck_eventDeduplication_MixedEventTypes(t *testing.T) {
	controller := createTestHealthCheck()
	recorder := controller.eventRecorder.(events.InMemoryRecorder)

	// Record a status with warnings, errors, and multiple types of warnings
	status := &HealthStatus{
		OverallStatus: statusError,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed",
			"Recent fencing event: reboot of master-1 success",
			"Expected 2 nodes, found 1",
		},
		Errors: []string{
			"Node master-0 is not online",
			"etcd resource not started on all nodes (started on 1/2 nodes)",
		},
	}
	controller.recordHealthCheckEvents(status)
	initialEvents := len(recorder.Events())
	require.Equal(t, 5, initialEvents, "Should have recorded 5 events (3 warnings + 2 errors)")

	// Verify all events are in deduplication map
	controller.recordedEventsMu.Lock()
	require.Len(t, controller.recordedEvents, 5, "Should have 5 events in deduplication map")
	controller.recordedEventsMu.Unlock()

	// Record the same status again - all should be deduplicated
	controller.recordHealthCheckEvents(status)
	afterDuplicates := len(recorder.Events())
	require.Equal(t, initialEvents, afterDuplicates, "All duplicate events should not be recorded")
}

func TestHealthCheck_eventDeduplication_CleanupOldEntries(t *testing.T) {
	controller := createTestHealthCheck()

	// Add a very old entry to the recorded events map
	oldEventKey := "warning:Very old warning"
	controller.recordedEventsMu.Lock()
	controller.recordedEvents[oldEventKey] = time.Now().Add(-25 * time.Hour) // 25 hours ago (older than longest window of 24 hours)
	controller.recordedEventsMu.Unlock()

	// Trigger cleanup by recording a new event
	status := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings:      []string{"New warning"},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(status)

	// Verify old entry was cleaned up
	controller.recordedEventsMu.Lock()
	_, exists := controller.recordedEvents[oldEventKey]
	require.False(t, exists, "Old entry should have been cleaned up")
	controller.recordedEventsMu.Unlock()
}

func TestHealthCheck_buildHealthStatusFromCR(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy cluster status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: v1alpha1.PacemakerDaemonStateRunning,
				QuorumStatus:         v1alpha1.QuorumStatusQuorate,
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
			Resources: []v1alpha1.PacemakerResourceStatus{
				{Name: "kubelet-clone-0", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "kubelet-clone-1", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
				{Name: "etcd-clone-0", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "etcd-clone-1", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
			},
			NodeHistory:    []v1alpha1.PacemakerNodeHistoryEntry{},
			FencingHistory: []v1alpha1.PacemakerFencingEvent{},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusHealthy, status.OverallStatus, "Status should be Healthy for healthy cluster")
	require.Empty(t, status.Errors, "Should have no errors for healthy cluster")
	require.Empty(t, status.Warnings, "Should have no warnings for healthy cluster")
}

func TestHealthCheck_buildHealthStatusFromCR_OfflineNode(t *testing.T) {
	controller := &HealthCheck{}

	// Test with offline node status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: v1alpha1.PacemakerDaemonStateRunning,
				QuorumStatus:         v1alpha1.QuorumStatusQuorate,
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOffline, Mode: v1alpha1.NodeModeActive},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error for offline node")
	require.NotEmpty(t, status.Errors, "Should have errors for offline node")
	require.Contains(t, status.Errors, "Node master-1 is not online")
}

func TestHealthCheck_buildHealthStatusFromCR_StandbyNode(t *testing.T) {
	controller := &HealthCheck{}

	// Test with standby node status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: v1alpha1.PacemakerDaemonStateRunning,
				QuorumStatus:         v1alpha1.QuorumStatusQuorate,
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeStandby},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error for standby node")
	require.NotEmpty(t, status.Errors, "Should have errors for standby node")
	require.Contains(t, status.Errors, "Node master-1 is in standby (unexpected behavior; treated as offline)")
}

func TestHealthCheck_buildHealthStatusFromCR_RecentFailures(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent failures in status
	now := metav1.Now()
	rc1 := int32(1)
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: v1alpha1.PacemakerDaemonStateRunning,
				QuorumStatus:         v1alpha1.QuorumStatusQuorate,
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
			Resources: []v1alpha1.PacemakerResourceStatus{
				{Name: "kubelet-clone-0", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "kubelet-clone-1", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
				{Name: "etcd-clone-0", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "etcd-clone-1", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
			},
			NodeHistory: []v1alpha1.PacemakerNodeHistoryEntry{
				{Node: "master-0", Resource: "etcd-clone-0", Operation: "monitor", RC: &rc1, RCText: "error", LastRCChange: now},
			},
			FencingHistory: []v1alpha1.PacemakerFencingEvent{
				{Target: "master-1", Action: "reboot", Status: "success", Completed: now},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusWarning, status.OverallStatus, "Status should be Warning for recent failures")
	require.Empty(t, status.Errors, "Should have no errors for healthy cluster")
	require.NotEmpty(t, status.Warnings, "Should have warnings for recent failures")

	// Check for specific warnings
	foundFailedAction := false
	foundFencingEvent := false
	for _, warning := range status.Warnings {
		if strings.Contains(warning, "Recent failed resource action") {
			foundFailedAction = true
		}
		if strings.Contains(warning, "Recent fencing event") {
			foundFencingEvent = true
		}
	}
	require.True(t, foundFailedAction, "Should have warning for recent failed resource action")
	require.True(t, foundFencingEvent, "Should have warning for recent fencing event")
}

func TestHealthCheck_buildHealthStatusFromCR_NilSummary(t *testing.T) {
	controller := &HealthCheck{}

	// Test with nil Summary - should return Unknown status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary:     nil, // No summary data available
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusUnknown, status.OverallStatus, "Status should be Unknown when Summary is nil")
	require.Empty(t, status.Errors, "Should have no errors when Summary is nil (missing data = unknown)")
	require.Empty(t, status.Warnings, "Should have no warnings when Summary is nil")
}

func TestHealthCheck_buildHealthStatusFromCR_EmptyPacemakerDaemonState(t *testing.T) {
	controller := &HealthCheck{}

	// Test with empty PacemakerDaemonState - should return Unknown status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: "", // Empty state
				QuorumStatus:         v1alpha1.QuorumStatusQuorate,
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusUnknown, status.OverallStatus, "Status should be Unknown when PacemakerDaemonState is empty")
	require.Empty(t, status.Errors, "Should have no errors when PacemakerDaemonState is empty (missing data = unknown)")
	require.Empty(t, status.Warnings, "Should have no warnings when PacemakerDaemonState is empty")
}

func TestHealthCheck_buildHealthStatusFromCR_PacemakerNotRunning(t *testing.T) {
	controller := &HealthCheck{}

	// Test with pacemaker not running - should return Error status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: v1alpha1.PacemakerDaemonStateNotRunning, // Not running
				QuorumStatus:         v1alpha1.QuorumStatusQuorate,
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error when pacemaker is not running")
	require.NotEmpty(t, status.Errors, "Should have errors when pacemaker is not running")
	require.Contains(t, status.Errors, msgPacemakerNotRunning, "Should have specific error message for pacemaker not running")
}

func TestHealthCheck_buildHealthStatusFromCR_NoQuorum(t *testing.T) {
	controller := &HealthCheck{}

	// Test with no quorum - should return Error status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Summary: &v1alpha1.PacemakerSummary{
				PacemakerDaemonState: v1alpha1.PacemakerDaemonStateRunning,
				QuorumStatus:         v1alpha1.QuorumStatusNoQuorum, // No quorum
			},
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
			Resources: []v1alpha1.PacemakerResourceStatus{
				{Name: "kubelet-clone-0", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "kubelet-clone-1", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
				{Name: "etcd-clone-0", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "etcd-clone-1", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error when cluster has no quorum")
	require.NotEmpty(t, status.Errors, "Should have errors when cluster has no quorum")
	require.Contains(t, status.Errors, msgClusterNoQuorum, "Should have specific error message for no quorum")
}

func TestHealthCheck_buildHealthStatusFromCR_NilStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test defensive check for nil Status (shouldn't happen in practice but good to test)
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: nil, // Nil status
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusUnknown, status.OverallStatus, "Status should be Unknown when Status is nil")
	require.NotEmpty(t, status.Errors, "Should have internal error when Status is nil")
	require.Contains(t, status.Errors, "Internal error: nil Status in buildHealthStatusFromCR", "Should have specific internal error message")
}

func TestHealthCheck_checkNodeStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy nodes
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatus(pacemakerStatus, status)

	require.Empty(t, status.Errors, "Should have no errors for online nodes")
	require.Empty(t, status.Warnings, "Should have no warnings for expected node count")
}

func TestHealthCheck_checkNodeStatus_MismatchedCount(t *testing.T) {
	controller := &HealthCheck{}

	// Create status with only 1 node (mismatch with expected 2)
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatus(pacemakerStatus, status)

	require.Empty(t, status.Errors, "Should have no errors for single online node")
	require.NotEmpty(t, status.Warnings, "Should have warning for node count mismatch")
	require.Contains(t, status.Warnings[0], "Expected 2 nodes, found 1")
}

func TestHealthCheck_checkNodeStatus_NoNodes(t *testing.T) {
	controller := &HealthCheck{}

	// Create status with no nodes - this should be treated as missing data, not an error
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerNodeStatus{},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatus(pacemakerStatus, status)

	// Empty nodes list means we don't have node information - no error should be raised
	require.Empty(t, status.Errors, "Empty nodes list should not generate errors (missing data = unknown status)")
	require.Empty(t, status.Warnings, "Empty nodes list should not generate warnings")
}

func TestHealthCheck_checkNodeStatus_StandbyNode(t *testing.T) {
	controller := &HealthCheck{}

	// Create status with one node in standby
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeStandby},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatus(pacemakerStatus, status)

	require.NotEmpty(t, status.Errors, "Should have error for standby node")
	require.Contains(t, status.Errors[0], "Node master-1 is in standby (unexpected behavior; treated as offline)")
}

func TestHealthCheck_checkNodeStatus_OfflineAndStandbyNode(t *testing.T) {
	controller := &HealthCheck{}

	// Create status with one offline node and one standby node
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerNodeStatus{
				{Name: "master-0", OnlineStatus: v1alpha1.NodeOnlineStatusOffline, Mode: v1alpha1.NodeModeActive},
				{Name: "master-1", OnlineStatus: v1alpha1.NodeOnlineStatusOnline, Mode: v1alpha1.NodeModeStandby},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatus(pacemakerStatus, status)

	require.Len(t, status.Errors, 2, "Should have 2 errors (one for offline, one for standby)")
	require.Contains(t, status.Errors[0], "Node master-0 is not online")
	require.Contains(t, status.Errors[1], "Node master-1 is in standby (unexpected behavior; treated as offline)")
}

func TestHealthCheck_checkResourceStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy resources
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Resources: []v1alpha1.PacemakerResourceStatus{
				{Name: "kubelet-clone-0", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "kubelet-clone-1", ResourceAgent: resourceAgentKubelet, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
				{Name: "etcd-clone-0", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-0"},
				{Name: "etcd-clone-1", ResourceAgent: resourceAgentEtcd, Role: "Started", ActiveStatus: v1alpha1.ResourceActiveStatusActive, Node: "master-1"},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkResourceStatus(pacemakerStatus, status)

	require.Empty(t, status.Errors, "Should have no errors for started resources")
}

func TestHealthCheck_determineOverallStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test healthy status
	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	result := controller.determineOverallStatus(status)
	require.Equal(t, statusHealthy, result)

	// Test warning status
	status = &HealthStatus{Warnings: []string{"warning"}, Errors: []string{}}
	result = controller.determineOverallStatus(status)
	require.Equal(t, statusWarning, result)

	// Test degraded status - with errors
	status = &HealthStatus{Warnings: []string{}, Errors: []string{"existing error"}}
	result = controller.determineOverallStatus(status)
	require.Equal(t, statusError, result)

	// Test degraded status - with multiple errors
	status = &HealthStatus{Warnings: []string{"warning"}, Errors: []string{"error1", "error2"}}
	result = controller.determineOverallStatus(status)
	require.Equal(t, statusError, result)
}

func TestHealthCheck_newUnknownHealthStatus(t *testing.T) {
	errMsg := "test error message"
	status := newUnknownHealthStatus(errMsg)

	require.NotNil(t, status, "Status should not be nil")
	require.Equal(t, statusUnknown, status.OverallStatus, "OverallStatus should be Unknown")
	require.Len(t, status.Errors, 1, "Should have one error")
	require.Equal(t, errMsg, status.Errors[0], "Error message should match")
	require.Empty(t, status.Warnings, "Warnings should be empty")
}

func TestHealthCheck_checkRecentFailures(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent failures
	now := metav1.Now()
	rc1 := int32(1)
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			NodeHistory: []v1alpha1.PacemakerNodeHistoryEntry{
				{Node: "master-0", Resource: "etcd-clone-0", Operation: "monitor", RC: &rc1, RCText: "error", LastRCChange: now},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkRecentFailures(pacemakerStatus, status)

	require.NotEmpty(t, status.Warnings, "Should have warnings for recent failures")
	foundFailedAction := false
	for _, warning := range status.Warnings {
		if strings.Contains(warning, "Recent failed resource action") {
			foundFailedAction = true
			break
		}
	}
	require.True(t, foundFailedAction, "Should have warning for recent failed resource action")
}

func TestHealthCheck_checkFencingEvents(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent fencing events
	now := metav1.Now()
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			FencingHistory: []v1alpha1.PacemakerFencingEvent{
				{Target: "master-1", Action: "reboot", Status: "success", Completed: now},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkFencingEvents(pacemakerStatus, status)

	require.NotEmpty(t, status.Warnings, "Should have warnings for recent fencing events")
	foundFencingEvent := false
	for _, warning := range status.Warnings {
		if strings.Contains(warning, "Recent fencing event") {
			foundFencingEvent = true
			break
		}
	}
	require.True(t, foundFencingEvent, "Should have warning for recent fencing event")
}

func TestHealthCheck_validateResourceCount(t *testing.T) {
	controller := &HealthCheck{}

	// Test with correct count
	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.validateResourceCount(resourceEtcd, 2, status)
	require.Empty(t, status.Errors, "Should have no errors for correct count")

	// Test with insufficient count
	status = &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.validateResourceCount(resourceKubelet, 1, status)
	require.NotEmpty(t, status.Errors, "Should have error for insufficient count")
	require.Contains(t, status.Errors[0], "kubelet resource not started on all nodes (started on 1/2 nodes)")
}
