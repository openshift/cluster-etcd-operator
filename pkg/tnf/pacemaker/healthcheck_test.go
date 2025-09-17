package pacemaker

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/clock"

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

	return &HealthCheck{
		operatorClient: operatorClient,
		kubeClient:     kubeClient,
		eventRecorder:  eventRecorder,
		recordedEvents: make(map[string]time.Time),
		previousStatus: statusUnknown,
	}
}

func loadTestXML(t *testing.T, filename string) string {
	path := filepath.Join("testdata", filename)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "Failed to read test XML file: %s", filename)
	return string(data)
}

func getRecentFailuresXML(t *testing.T) string {
	// Load the template from testdata
	templateXML := loadTestXML(t, "recent_failures_template.xml")

	// Generate recent timestamps that are definitely within the 5-minute window
	now := time.Now()
	recentTime := now.Add(-1 * time.Minute).UTC().Format("Mon Jan 2 15:04:05 2006")
	recentTimeISO := now.Add(-1 * time.Minute).UTC().Format("2006-01-02 15:04:05.000000Z")

	// Replace the placeholders in the template
	xmlContent := strings.ReplaceAll(templateXML, "{{RECENT_TIMESTAMP}}", recentTime)
	xmlContent = strings.ReplaceAll(xmlContent, "{{RECENT_TIMESTAMP_ISO}}", recentTimeISO)

	return xmlContent
}

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

	controller := NewHealthCheck(
		livenessChecker,
		operatorClient,
		kubeClient,
		eventRecorder,
	)

	require.NotNil(t, controller, "Controller should not be nil")
}

func TestHealthCheck_getPacemakerStatus(t *testing.T) {
	controller := createTestHealthCheck()

	ctx := context.TODO()
	status, err := controller.getPacemakerStatus(ctx)

	// In test environment, exec.Execute will fail due to permissions.
	// getPacemakerStatus handles this gracefully by returning Unknown status without propagating the error.
	require.NoError(t, err, "getPacemakerStatus should not return an error even when exec fails")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusUnknown, status.OverallStatus, "Status should be Unknown when exec fails")
	require.NotEmpty(t, status.Errors, "Should have error messages when exec fails")
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
	require.NoError(t, err, "updateOperatorAvailability should not return an error")

	// Test healthy status
	status = &HealthStatus{
		OverallStatus: statusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	err = controller.updateOperatorStatus(ctx, status)
	require.NoError(t, err, "updateOperatorAvailability should not return an error")

	// Test warning status (should not update operator condition)
	status = &HealthStatus{
		OverallStatus: statusWarning,
		Warnings:      []string{"Test warning"},
		Errors:        []string{},
	}

	err = controller.updateOperatorStatus(ctx, status)
	require.NoError(t, err, "updateOperatorAvailability should not return an error")
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

	// Immediately record the same fencing event - should be deduplicated (1 hour window)
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
	controller.recordedEvents[oldEventKey] = time.Now().Add(-2 * time.Hour) // 2 hours ago (older than longest window)
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

func TestHealthCheck_parsePacemakerStatusXML(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy cluster XML
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	status, err := controller.parsePacemakerStatusXML(healthyXML)

	require.NoError(t, err, "parsePacemakerStatusXML should not return an error")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusHealthy, status.OverallStatus, "Status should be Healthy for healthy cluster")
	require.Empty(t, status.Errors, "Should have no errors for healthy cluster")
	require.Empty(t, status.Warnings, "Should have no warnings for healthy cluster")
}

func TestHealthCheck_parsePacemakerStatusXML_OfflineNode(t *testing.T) {
	controller := &HealthCheck{}

	// Test with offline node XML
	offlineXML := loadTestXML(t, "offline_node.xml")
	status, err := controller.parsePacemakerStatusXML(offlineXML)

	require.NoError(t, err, "parsePacemakerStatusXML should not return an error")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error for offline node")
	require.NotEmpty(t, status.Errors, "Should have errors for offline node")
	require.Contains(t, status.Errors, "Node master-1 is not online")
}

func TestHealthCheck_parsePacemakerStatusXML_StandbyNode(t *testing.T) {
	controller := &HealthCheck{}

	// Test with standby node XML
	standbyXML := loadTestXML(t, "standby_node.xml")
	status, err := controller.parsePacemakerStatusXML(standbyXML)

	require.NoError(t, err, "parsePacemakerStatusXML should not return an error")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error for standby node")
	require.NotEmpty(t, status.Errors, "Should have errors for standby node")
	require.Contains(t, status.Errors, "Node master-1 is in standby (unexpected behavior; treated as offline)")
}

func TestHealthCheck_parsePacemakerStatusXML_RecentFailures(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent failures XML - use template with dynamic timestamps
	recentFailuresXML := getRecentFailuresXML(t)
	status, err := controller.parsePacemakerStatusXML(recentFailuresXML)

	require.NoError(t, err, "parsePacemakerStatusXML should not return an error")
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

func TestHealthCheck_collectNodeStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy nodes
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(healthyXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectNodeStatus(&result, status)

	require.Empty(t, status.Errors, "Should have no errors for online nodes")
	require.Empty(t, status.Warnings, "Should have no warnings for expected node count")
}

func TestHealthCheck_collectNodeStatus_MismatchedCount(t *testing.T) {
	controller := &HealthCheck{}

	// Create result with only 1 node (mismatch with expected 2)
	result := &PacemakerResult{
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: booleanValueTrue},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectNodeStatus(result, status)

	require.Empty(t, status.Errors, "Should have no errors for single online node")
	require.NotEmpty(t, status.Warnings, "Should have warning for node count mismatch")
	require.Contains(t, status.Warnings[0], "Expected 2 nodes, found 1")
}

func TestHealthCheck_collectNodeStatus_NoNodes(t *testing.T) {
	controller := &HealthCheck{}

	// Create result with no nodes
	result := &PacemakerResult{
		Nodes: Nodes{
			Node: []Node{},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectNodeStatus(result, status)

	require.NotEmpty(t, status.Errors, "Should have error for no nodes")
	require.Contains(t, status.Errors[0], "No nodes found")
}

func TestHealthCheck_collectNodeStatus_StandbyNode(t *testing.T) {
	controller := &HealthCheck{}

	// Create result with one node in standby
	result := &PacemakerResult{
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: booleanValueTrue, Standby: booleanValueFalse},
				{Name: "master-1", Online: booleanValueTrue, Standby: booleanValueTrue},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectNodeStatus(result, status)

	require.NotEmpty(t, status.Errors, "Should have error for standby node")
	require.Contains(t, status.Errors[0], "Node master-1 is in standby (unexpected behavior; treated as offline)")
}

func TestHealthCheck_collectNodeStatus_OfflineAndStandbyNode(t *testing.T) {
	controller := &HealthCheck{}

	// Create result with one offline node and one standby node
	result := &PacemakerResult{
		Nodes: Nodes{
			Node: []Node{
				{Name: "master-0", Online: booleanValueFalse, Standby: booleanValueFalse},
				{Name: "master-1", Online: booleanValueTrue, Standby: booleanValueTrue},
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectNodeStatus(result, status)

	require.Len(t, status.Errors, 2, "Should have 2 errors (one for offline, one for standby)")
	require.Contains(t, status.Errors[0], "Node master-0 is not online")
	require.Contains(t, status.Errors[1], "Node master-1 is in standby (unexpected behavior; treated as offline)")
}

func TestHealthCheck_collectResourceStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy resources
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(healthyXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectResourceStatus(&result, status)

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

func TestHealthCheck_collectRecentFailures(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent failures - use template with dynamic timestamps
	recentFailuresXML := getRecentFailuresXML(t)
	var result PacemakerResult
	err := xml.Unmarshal([]byte(recentFailuresXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectRecentFailures(&result, status)

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

func TestHealthCheck_collectFencingEvents(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent fencing events - use template with dynamic timestamps
	recentFailuresXML := getRecentFailuresXML(t)
	var result PacemakerResult
	err := xml.Unmarshal([]byte(recentFailuresXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.collectFencingEvents(&result, status)

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

func TestHealthCheck_parsePacemakerStatusXML_LargeXML(t *testing.T) {
	controller := &HealthCheck{}

	// Create XML output that exceeds maxXMLSize
	largeXML := strings.Repeat("<data>test</data>", maxXMLSize/20+1)

	status, err := controller.parsePacemakerStatusXML(largeXML)

	// Should fail due to invalid XML structure (not proper pacemaker format)
	require.Error(t, err, "Should return error for invalid XML")
	require.Nil(t, status, "Status should be nil on parse error")
}

func TestHealthCheck_parsePacemakerStatusXML_InvalidXML(t *testing.T) {
	controller := &HealthCheck{}

	invalidXML := "<invalid>not pacemaker xml</invalid>"

	status, err := controller.parsePacemakerStatusXML(invalidXML)

	require.Error(t, err, "Should return error for invalid XML structure")
	require.Nil(t, status, "Status should be nil on parse error")
}

func TestHealthCheck_parsePacemakerStatusXML_MissingState(t *testing.T) {
	controller := &HealthCheck{}

	// XML without pacemaker state
	xmlWithoutState := `<?xml version="1.0"?>
<pacemaker-result>
  <summary>
    <stack type="corosync">
    </stack>
  </summary>
</pacemaker-result>`

	status, err := controller.parsePacemakerStatusXML(xmlWithoutState)

	require.Error(t, err, "Should return error for missing pacemaker state")
	require.Contains(t, err.Error(), "missing pacemaker state information")
	require.Nil(t, status, "Status should be nil on validation error")
}
