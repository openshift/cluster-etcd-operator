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
		operatorClient:      createFakeOperatorClient(),
		kubeClient:          fake.NewSimpleClientset(),
		eventRecorder:       events.NewInMemoryRecorder("test", clock.RealClock{}),
		pacemakerRESTClient: nil,
		pacemakerInformer:   nil,
		recordedEvents:      make(map[string]time.Time),
		previousStatus:      statusUnknown,
	}
}

// createTestHealthCheckWithMockStatus creates a HealthCheck with a mocked Pacemaker CR
func createTestHealthCheckWithMockStatus(t *testing.T, mockStatus *v1alpha1.PacemakerCluster) *HealthCheck {
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
		operatorClient:      createFakeOperatorClient(),
		kubeClient:          fake.NewSimpleClientset(),
		eventRecorder:       events.NewInMemoryRecorder("test", clock.RealClock{}),
		pacemakerRESTClient: fakeClient,
		pacemakerInformer:   nil,
		recordedEvents:      make(map[string]time.Time),
		previousStatus:      statusUnknown,
	}
}

// =============================================================================
// Unit Tests
// =============================================================================
// Note: Test fixtures are defined in test_fixtures_test.go

func TestNewHealthCheck(t *testing.T) {
	controller, informer, err := NewHealthCheck(
		health.NewMultiAlivenessChecker(),
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
					Conditions: createHealthyClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
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
				Status: &v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
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
				Status: nil, // Status not populated yet
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
				Status: &v1alpha1.PacemakerClusterStatus{
					Conditions: createInsufficientNodesClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
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
				Status: &v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
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
				Status: &v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
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
			name: "unhealthy_fencing",
			mockStatus: &v1alpha1.PacemakerCluster{
				TypeMeta: metav1.TypeMeta{
					APIVersion: v1alpha1.SchemeGroupVersion.String(),
					Kind:       "PacemakerCluster",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: &v1alpha1.PacemakerClusterStatus{
					Conditions: createHealthyClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, v1alpha1.PacemakerClusterResourceNameFencingAgent),
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
			"Recent failed resource action: kubelet monitor on master-0 failed",
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

	// First recording - Error state (start in Error to test transitions properly)
	errorStatus := &HealthStatus{
		OverallStatus: statusError,
		Warnings:      []string{},
		Errors:        []string{"Test error"},
	}
	controller.recordHealthCheckEvents(errorStatus)

	// Count events after first recording
	initialEvents := len(recorder.Events())
	require.Equal(t, 1, initialEvents, "Should have recorded 1 error event")

	// Immediately record the same error again - should be deduplicated (5 min window)
	controller.recordHealthCheckEvents(errorStatus)
	afterDuplicateEvents := len(recorder.Events())
	require.Equal(t, initialEvents, afterDuplicateEvents, "Duplicate error should not be recorded")

	// Transition to Warning (operationally healthy) - should record PacemakerHealthy event
	warningStatus := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings: []string{
			"Recent failed resource action: kubelet monitor on master-0 failed",
		},
		Errors: []string{},
	}
	controller.recordHealthCheckEvents(warningStatus)
	afterWarningTransition := len(recorder.Events())
	require.Equal(t, 3, afterWarningTransition, "Should have recorded warning event + PacemakerHealthy transition event")

	// Stay in Warning - no transition event
	controller.recordHealthCheckEvents(warningStatus)
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
	controller.recordHealthCheckEvents(fencingStatus)
	afterFencingEvents := len(recorder.Events())
	require.Equal(t, 4, afterFencingEvents, "Fencing event should be recorded")

	// Immediately record the same fencing event - should be deduplicated (24 hour window)
	controller.recordHealthCheckEvents(fencingStatus)
	afterFencingDuplicateEvents := len(recorder.Events())
	require.Equal(t, afterFencingEvents, afterFencingDuplicateEvents, "Duplicate fencing event should not be recorded")

	// Transition from Warning to Healthy - no event since both are operationally healthy
	healthyStatus := &HealthStatus{
		OverallStatus: statusHealthy,
		Warnings:      []string{},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(healthyStatus)
	afterHealthyTransition := len(recorder.Events())
	require.Equal(t, afterFencingDuplicateEvents, afterHealthyTransition, "No transition event from Warning to Healthy (both are healthy states)")

	// Stay healthy - no event
	controller.recordHealthCheckEvents(healthyStatus)
	afterHealthyDuplicate := len(recorder.Events())
	require.Equal(t, afterHealthyTransition, afterHealthyDuplicate, "No event when staying healthy")

	// Transition to Error and back to Healthy - should record PacemakerHealthy event
	controller.recordHealthCheckEvents(errorStatus)
	beforeRecovery := len(recorder.Events())
	controller.recordHealthCheckEvents(healthyStatus)
	afterRecovery := len(recorder.Events())
	require.Greater(t, afterRecovery, beforeRecovery, "PacemakerHealthy event should be recorded on transition from Error to Healthy")
}

func TestHealthCheck_buildHealthStatusFromCR(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy cluster status
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Conditions:  createHealthyClusterConditions(),
			Nodes: []v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusHealthy, status.OverallStatus, "Status should be Healthy for healthy cluster")
	require.Empty(t, status.Errors, "Should have no errors for healthy cluster")
	require.Empty(t, status.Warnings, "Should have no warnings for healthy cluster")
}

func TestHealthCheck_buildHealthStatusFromCR_UnhealthyResource(t *testing.T) {
	tests := []struct {
		name             string
		unhealthyNode    string
		healthyNode      string
		unhealthyResource v1alpha1.PacemakerClusterResourceName
	}{
		{
			name:             "unhealthy_kubelet",
			unhealthyNode:    "master-0",
			healthyNode:      "master-1",
			unhealthyResource: v1alpha1.PacemakerClusterResourceNameKubelet,
		},
		{
			name:             "unhealthy_etcd",
			unhealthyNode:    "master-1",
			healthyNode:      "master-0",
			unhealthyResource: v1alpha1.PacemakerClusterResourceNameEtcd,
		},
		{
			name:             "unhealthy_fencing",
			unhealthyNode:    "master-0",
			healthyNode:      "master-1",
			unhealthyResource: v1alpha1.PacemakerClusterResourceNameFencingAgent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &HealthCheck{}

			pacemakerStatus := &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: []v1alpha1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus(tt.unhealthyNode, []string{"192.168.1.10"}, tt.unhealthyResource),
						createHealthyNodeStatus(tt.healthyNode, []string{"192.168.1.11"}),
					},
				},
			}

			status := controller.buildHealthStatusFromCR(pacemakerStatus)

			require.NotNil(t, status, "HealthStatus should not be nil")
			require.Equal(t, statusError, status.OverallStatus, "Status should be Error for unhealthy %s", tt.unhealthyResource)
			require.NotEmpty(t, status.Errors, "Should have errors for unhealthy %s", tt.unhealthyResource)

			// Verify error mentions both resource name and node name
			foundError := false
			for _, err := range status.Errors {
				if strings.Contains(err, string(tt.unhealthyResource)) && strings.Contains(err, tt.unhealthyNode) {
					foundError = true
					break
				}
			}
			require.True(t, foundError, "Should have error mentioning %s and %s", tt.unhealthyResource, tt.unhealthyNode)
		})
	}
}

func TestHealthCheck_buildHealthStatusFromCR_InsufficientNodes(t *testing.T) {
	controller := &HealthCheck{}

	// Test with insufficient nodes
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Conditions:  createInsufficientNodesClusterConditions(),
			Nodes: []v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error for insufficient nodes")
	require.NotEmpty(t, status.Errors, "Should have errors for insufficient nodes")
	require.Contains(t, status.Errors[0], "Insufficient")
}

func TestHealthCheck_buildHealthStatusFromCR_NoNodes(t *testing.T) {
	controller := &HealthCheck{}

	// Test with no nodes - should return Error
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Conditions:  createHealthyClusterConditions(),
			Nodes:       []v1alpha1.PacemakerClusterNodeStatus{},
		},
	}

	status := controller.buildHealthStatusFromCR(pacemakerStatus)

	require.NotNil(t, status, "HealthStatus should not be nil")
	require.Equal(t, statusError, status.OverallStatus, "Status should be Error when no nodes")
	require.NotEmpty(t, status.Errors, "Should have errors when no nodes")
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
	require.Contains(t, status.Errors[0], "Internal error")
}

func TestHealthCheck_checkClusterConditions(t *testing.T) {
	tests := []struct {
		name          string
		conditions    []metav1.Condition
		nodes         []v1alpha1.PacemakerClusterNodeStatus
		expectErrors  bool
		errorContains string
	}{
		{
			name:       "healthy_cluster",
			conditions: createHealthyClusterConditions(),
			nodes: []v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
			expectErrors: false,
		},
		{
			name:       "insufficient_nodes",
			conditions: createInsufficientNodesClusterConditions(),
			nodes: []v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
			},
			expectErrors:  true,
			errorContains: "Insufficient",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &HealthCheck{}
			pacemakerStatus := &v1alpha1.PacemakerCluster{
				Status: &v1alpha1.PacemakerClusterStatus{
					Conditions: tt.conditions,
					Nodes:      tt.nodes,
				},
			}

			status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
			controller.checkClusterConditions(pacemakerStatus, status)

			if tt.expectErrors {
				require.NotEmpty(t, status.Errors, "Should have errors")
				if tt.errorContains != "" {
					require.Contains(t, status.Errors[0], tt.errorContains)
				}
			} else {
				require.Empty(t, status.Errors, "Should have no errors")
			}
		})
	}
}

func TestHealthCheck_checkNodeStatuses(t *testing.T) {
	controller := &HealthCheck{}

	// Test with healthy nodes
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerClusterNodeStatus{
				createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatuses(pacemakerStatus, status)

	require.Empty(t, status.Errors, "Should have no errors for healthy nodes")
}

func TestHealthCheck_checkNodeStatuses_UnhealthyResource(t *testing.T) {
	controller := &HealthCheck{}

	// Test with one unhealthy kubelet
	pacemakerStatus := &v1alpha1.PacemakerCluster{
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerClusterNodeStatus{
				createUnhealthyNodeStatus("master-0", []string{"192.168.1.10"}, v1alpha1.PacemakerClusterResourceNameKubelet),
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatuses(pacemakerStatus, status)

	require.NotEmpty(t, status.Errors, "Should have errors for unhealthy kubelet")
	// Look for errors containing the resource name
	foundError := false
	for _, err := range status.Errors {
		if strings.Contains(err, string(v1alpha1.PacemakerClusterResourceNameKubelet)) {
			foundError = true
			break
		}
	}
	require.True(t, foundError, "Should have error mentioning Kubelet")
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
		Status: &v1alpha1.PacemakerClusterStatus{
			Nodes: []v1alpha1.PacemakerClusterNodeStatus{
				node,
				createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
			},
		},
	}

	status := &HealthStatus{Warnings: []string{}, Errors: []string{}}
	controller.checkNodeStatuses(pacemakerStatus, status)

	// Should have at least 2 errors (one for each unhealthy resource)
	// May have additional node-level unhealthy condition
	require.GreaterOrEqual(t, len(status.Errors), 2, "Should have at least 2 errors for multiple unhealthy resources")

	// Verify we have errors for both resources
	kubeletErrorFound := false
	etcdErrorFound := false
	for _, err := range status.Errors {
		if strings.Contains(err, string(v1alpha1.PacemakerClusterResourceNameKubelet)) {
			kubeletErrorFound = true
		}
		if strings.Contains(err, string(v1alpha1.PacemakerClusterResourceNameEtcd)) {
			etcdErrorFound = true
		}
	}
	require.True(t, kubeletErrorFound, "Should have error for unhealthy Kubelet")
	require.True(t, etcdErrorFound, "Should have error for unhealthy Etcd")
}

func TestHealthCheck_determineOverallStatus(t *testing.T) {
	tests := []struct {
		name           string
		warnings       []string
		errors         []string
		expectedStatus string
	}{
		{
			name:           "healthy_no_warnings_no_errors",
			warnings:       []string{},
			errors:         []string{},
			expectedStatus: statusHealthy,
		},
		{
			name:           "warning_with_warnings_no_errors",
			warnings:       []string{"warning"},
			errors:         []string{},
			expectedStatus: statusWarning,
		},
		{
			name:           "error_with_single_error",
			warnings:       []string{},
			errors:         []string{"existing error"},
			expectedStatus: statusError,
		},
		{
			name:           "error_with_warnings_and_errors",
			warnings:       []string{"warning"},
			errors:         []string{"error1", "error2"},
			expectedStatus: statusError,
		},
		{
			name:           "error_takes_precedence_over_warning",
			warnings:       []string{"warning1", "warning2"},
			errors:         []string{"error"},
			expectedStatus: statusError,
		},
	}

	controller := &HealthCheck{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := &HealthStatus{Warnings: tt.warnings, Errors: tt.errors}
			result := controller.determineOverallStatus(status)
			require.Equal(t, tt.expectedStatus, result)
		})
	}
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
			expectedReason: eventReasonResourceUnhealthy,
		},
		{
			name:           "node_unhealthy",
			errorMsg:       "Node master-0 is unhealthy: node has issues",
			expectedReason: eventReasonNodeUnhealthy,
		},
		{
			name:           "insufficient_nodes",
			errorMsg:       "Insufficient nodes in cluster (expected 2, found 1)",
			expectedReason: eventReasonInsufficientNodes,
		},
		{
			name:           "status_stale",
			errorMsg:       "Pacemaker status is stale",
			expectedReason: eventReasonStatusStale,
		},
		{
			name:           "cr_not_found",
			errorMsg:       "Failed to get PacemakerCluster CR: not found",
			expectedReason: eventReasonCRNotFound,
		},
		{
			name:           "cr_no_status",
			errorMsg:       "PacemakerCluster CR has no status populated",
			expectedReason: eventReasonCRNotFound,
		},
		{
			name:           "generic_error",
			errorMsg:       "Some unknown error",
			expectedReason: eventReasonGenericError,
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

	// Transition to Warning - should record warning but not healthy event (already in healthy state)
	warningStatus := &HealthStatus{
		OverallStatus: statusWarning,
		Warnings:      []string{"Some warning"},
		Errors:        []string{},
	}
	controller.recordHealthCheckEvents(warningStatus)
	afterWarning := len(recorder.Events())
	require.Equal(t, afterFirstHealthy+1, afterWarning, "Warning event should be recorded")

	// Transition from Warning to Healthy - should NOT record transition (both are healthy states)
	controller.recordHealthCheckEvents(healthyStatus)
	afterSecondHealthy := len(recorder.Events())
	require.Equal(t, afterWarning, afterSecondHealthy, "No transition event from Warning to Healthy (both are healthy states)")

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
			"Kubelet resource is unhealthy on node master-0",
			"Etcd resource is unhealthy on node master-0",
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
