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

	// In test environment, exec.Execute will fail due to permissions
	require.NoError(t, err, "getPacemakerStatus should not return an error")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, StatusError, status.OverallStatus, "Status should be Error when exec fails")
	require.NotEmpty(t, status.Errors, "Should have error messages when exec fails")
}

func TestHealthCheck_updateOperatorAvailability(t *testing.T) {
	controller := createTestHealthCheck()
	ctx := context.TODO()

	// Test degraded status
	status := &HealthStatus{
		OverallStatus: StatusDegraded,
		Warnings:      []string{"Test warning"},
		Errors:        []string{"Test error"},
	}

	err := controller.updateOperatorAvailability(ctx, status)
	require.NoError(t, err, "updateOperatorAvailability should not return an error")

	// Test healthy status
	status = &HealthStatus{
		OverallStatus: StatusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	err = controller.updateOperatorAvailability(ctx, status)
	require.NoError(t, err, "updateOperatorAvailability should not return an error")

	// Test warning status (should not update operator condition)
	status = &HealthStatus{
		OverallStatus: StatusWarning,
		Warnings:      []string{"Test warning"},
		Errors:        []string{},
	}

	err = controller.updateOperatorAvailability(ctx, status)
	require.NoError(t, err, "updateOperatorAvailability should not return an error")
}

func TestHealthCheck_recordHealthCheckEvents(t *testing.T) {
	controller := createTestHealthCheck()

	// Test with warnings and errors
	status := &HealthStatus{
		OverallStatus: StatusWarning,
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
		OverallStatus: StatusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}

	controller.recordHealthCheckEvents(status)
}

func TestHealthCheck_parsePacemakerStatusXML(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy cluster XML
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	status, err := controller.parsePacemakerStatusXML(healthyXML)

	require.NoError(t, err, "parsePacemakerStatusXML should not return an error")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, StatusHealthy, status.OverallStatus, "Status should be Healthy for healthy cluster")
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
	require.Equal(t, StatusDegraded, status.OverallStatus, "Status should be Degraded for offline node")
	require.NotEmpty(t, status.Errors, "Should have errors for offline node")
	require.Contains(t, status.Errors, "Node master-1 is not online")
}

func TestHealthCheck_parsePacemakerStatusXML_RecentFailures(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent failures XML - use template with dynamic timestamps
	recentFailuresXML := getRecentFailuresXML(t)
	status, err := controller.parsePacemakerStatusXML(recentFailuresXML)

	require.NoError(t, err, "parsePacemakerStatusXML should not return an error")
	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, StatusWarning, status.OverallStatus, "Status should be Warning for recent failures")
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

func TestHealthCheck_parseNodes(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy nodes
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(healthyXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	allOnline := controller.parseNodes(&result, status)

	require.True(t, allOnline, "All nodes should be online")
	require.Empty(t, status.Errors, "Should have no errors for online nodes")
}

func TestHealthCheck_parseResources(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy resources
	healthyXML := loadTestXML(t, "healthy_cluster.xml")
	var result PacemakerResult
	err := xml.Unmarshal([]byte(healthyXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	allStarted := controller.parseResources(&result, status)

	require.True(t, allStarted, "All resources should be started")
	require.Empty(t, status.Errors, "Should have no errors for started resources")
}

func TestHealthCheck_determineOverallStatus(t *testing.T) {
	controller := &HealthCheck{}

	// Test healthy status
	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	result := controller.determineOverallStatus(true, true, status)
	require.Equal(t, StatusHealthy, result)

	// Test warning status
	status = &HealthStatus{Warnings: []string{"warning"}, Errors: []string{}}
	result = controller.determineOverallStatus(true, true, status)
	require.Equal(t, StatusWarning, result)

	// Test degraded status - nodes offline
	status = &HealthStatus{Warnings: []string{}, Errors: []string{}}
	result = controller.determineOverallStatus(false, true, status)
	require.Equal(t, StatusDegraded, result)
	require.Contains(t, status.Errors, "One or more nodes are offline")

	// Test degraded status - resources not started
	status = &HealthStatus{Warnings: []string{}, Errors: []string{}}
	result = controller.determineOverallStatus(true, false, status)
	require.Equal(t, StatusDegraded, result)
	require.Contains(t, status.Errors, "One or more critical resources are not started")

	// Test degraded status - existing errors
	status = &HealthStatus{Warnings: []string{}, Errors: []string{"existing error"}}
	result = controller.determineOverallStatus(true, true, status)
	require.Equal(t, StatusDegraded, result)
}

func TestHealthCheck_parseNodeHistory(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent failures - use template with dynamic timestamps
	recentFailuresXML := getRecentFailuresXML(t)
	var result PacemakerResult
	err := xml.Unmarshal([]byte(recentFailuresXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.parseNodeHistory(&result, status)

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

func TestHealthCheck_parseFenceHistory(t *testing.T) {
	controller := &HealthCheck{}

	// Test with recent fencing events - use template with dynamic timestamps
	recentFailuresXML := getRecentFailuresXML(t)
	var result PacemakerResult
	err := xml.Unmarshal([]byte(recentFailuresXML), &result)
	require.NoError(t, err)

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.parseFenceHistory(&result, status)

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
