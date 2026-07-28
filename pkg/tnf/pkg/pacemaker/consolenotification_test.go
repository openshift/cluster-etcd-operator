package pacemaker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/internal/testutil"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	clocktesting "k8s.io/utils/clock/testing"
)

// ---------------------------------------------------------------------------
// Functional-option builders for test data
// ---------------------------------------------------------------------------

type nodeOpt func(*pacmkrv1.PacemakerClusterNodeStatus)
type clusterOpt func(*pacmkrv1.PacemakerCluster)

func offline() nodeOpt {
	return setNodeCondition(pacmkrv1.NodeOnlineConditionType, metav1.ConditionFalse)
}

func fencingUnavailable() nodeOpt {
	return func(n *pacmkrv1.PacemakerClusterNodeStatus) {
		setNodeCondition(pacmkrv1.NodeHealthyConditionType, metav1.ConditionFalse)(n)
		setNodeCondition(pacmkrv1.NodeFencingAvailableConditionType, metav1.ConditionFalse)(n)
	}
}

func fencingDegraded() nodeOpt {
	return setNodeCondition(pacmkrv1.NodeFencingHealthyConditionType, metav1.ConditionFalse)
}

func unhealthyEtcd() nodeOpt {
	return func(n *pacmkrv1.PacemakerClusterNodeStatus) {
		setNodeCondition(pacmkrv1.NodeHealthyConditionType, metav1.ConditionFalse)(n)
		for i := range n.Resources {
			if n.Resources[i].Name == pacmkrv1.PacemakerClusterResourceNameEtcd {
				for j := range n.Resources[i].Conditions {
					if n.Resources[i].Conditions[j].Type == pacmkrv1.ResourceHealthyConditionType {
						n.Resources[i].Conditions[j].Status = metav1.ConditionFalse
					}
				}
				break
			}
		}
	}
}

func setNodeCondition(condType string, status metav1.ConditionStatus) nodeOpt {
	return func(n *pacmkrv1.PacemakerClusterNodeStatus) {
		for i := range n.Conditions {
			if n.Conditions[i].Type == condType {
				n.Conditions[i].Status = status
			}
		}
	}
}

func staleBy(d time.Duration) clusterOpt {
	return func(c *pacmkrv1.PacemakerCluster) {
		c.Status.LastUpdated = metav1.NewTime(time.Now().Add(-d))
	}
}

func inMaintenanceMode() clusterOpt {
	return func(c *pacmkrv1.PacemakerCluster) {
		c.Status.Conditions = createMaintenanceModeClusterConditions()
	}
}

func withNodes(nodes ...pacmkrv1.PacemakerClusterNodeStatus) clusterOpt {
	return func(c *pacmkrv1.PacemakerCluster) {
		ns := make([]pacmkrv1.PacemakerClusterNodeStatus, len(nodes))
		copy(ns, nodes)
		c.Status.Nodes = &ns
	}
}

func testNode(name, ip string, opts ...nodeOpt) pacmkrv1.PacemakerClusterNodeStatus {
	node := createHealthyNodeStatus(name, []string{ip})
	for _, opt := range opts {
		opt(&node)
	}
	return node
}

func testCluster(opts ...clusterOpt) *pacmkrv1.PacemakerCluster {
	cr := &pacmkrv1.PacemakerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: PacemakerClusterResourceName},
		Status: pacmkrv1.PacemakerClusterStatus{
			LastUpdated: metav1.Now(),
			Conditions:  createHealthyClusterConditions(),
		},
	}
	for _, opt := range opts {
		opt(cr)
	}
	return cr
}

// ---------------------------------------------------------------------------
// BuildHealthStatusFromCR tests (validates shared health evaluation)
// ---------------------------------------------------------------------------

func TestBuildHealthStatusFromCR_ConsoleScenarios(t *testing.T) {
	tests := []struct {
		name           string
		cr             *pacmkrv1.PacemakerCluster
		wantProblems   int
		wantSubstrings []string
	}{
		{
			name:         "healthy cluster returns no problems",
			cr:           testCluster(withNodes(testNode("master-0", "192.168.111.20"), testNode("master-1", "192.168.111.21"))),
			wantProblems: 0,
		},
		{
			name:           "stale status detected",
			cr:             testCluster(staleBy(10*time.Minute), withNodes(testNode("master-0", "192.168.111.20"))),
			wantProblems:   1,
			wantSubstrings: []string{"stale"},
		},
		{
			name:           "maintenance mode detected",
			cr:             testCluster(inMaintenanceMode(), withNodes(testNode("master-0", "192.168.111.20"))),
			wantProblems:   1,
			wantSubstrings: []string{"maintenance mode"},
		},
		{
			name: "offline node detected",
			cr: testCluster(withNodes(
				testNode("master-0", "192.168.111.20", offline()),
				testNode("master-1", "192.168.111.21"),
			)),
			wantProblems:   1,
			wantSubstrings: []string{"offline"},
		},
		{
			name: "offline node skips resource and fencing checks",
			cr: testCluster(withNodes(
				testNode("master-0", "192.168.111.20", offline(), fencingUnavailable(), unhealthyEtcd()),
			)),
			wantProblems:   1,
			wantSubstrings: []string{"offline"},
		},
		{
			name: "unhealthy etcd resource detected",
			cr: testCluster(withNodes(
				testNode("master-0", "192.168.111.20", unhealthyEtcd()),
				testNode("master-1", "192.168.111.21"),
			)),
			wantProblems:   1,
			wantSubstrings: []string{"Etcd"},
		},
		{
			name: "fencing unavailable detected",
			cr: testCluster(withNodes(
				testNode("master-0", "192.168.111.20", fencingUnavailable()),
				testNode("master-1", "192.168.111.21"),
			)),
			wantProblems:   1,
			wantSubstrings: []string{"fencing unavailable"},
		},
		{
			name: "fencing degraded detected as warning",
			cr: testCluster(withNodes(
				testNode("master-0", "192.168.111.20", fencingDegraded()),
				testNode("master-1", "192.168.111.21"),
			)),
			wantProblems:   1,
			wantSubstrings: []string{"fencing at risk"},
		},
		{
			name: "multiple problems aggregated",
			cr: testCluster(inMaintenanceMode(), withNodes(
				testNode("master-0", "192.168.111.20", offline()),
				testNode("master-1", "192.168.111.21"),
			)),
			wantProblems:   2,
			wantSubstrings: []string{"maintenance mode", "offline"},
		},
		{
			name:           "nil nodes reports error",
			cr:             testCluster(),
			wantProblems:   1,
			wantSubstrings: []string{"No nodes found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := BuildHealthStatusFromCR(tt.cr)
			problems := slices.Concat(status.Errors, status.Warnings)
			require.Equal(t, tt.wantProblems, len(problems), "problems: %v", problems)
			joined := strings.Join(problems, " | ")
			for _, sub := range tt.wantSubstrings {
				require.Contains(t, joined, sub)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Problem classification tests
// ---------------------------------------------------------------------------

func TestClassifyProblems(t *testing.T) {
	tests := []struct {
		name                string
		status              *HealthStatus
		wantDegraded        int
		wantTroubleshooting int
	}{
		{
			name:                "node offline routes to degraded",
			status:              &HealthStatus{Errors: []string{"Node master-0 is offline"}},
			wantDegraded:        1,
			wantTroubleshooting: 0,
		},
		{
			name:                "node unhealthy routes to degraded",
			status:              &HealthStatus{Errors: []string{"Node master-0 node is unhealthy: Etcd resource is unhealthy"}},
			wantDegraded:        1,
			wantTroubleshooting: 0,
		},
		{
			name:                "fencing warning routes to degraded",
			status:              &HealthStatus{Warnings: []string{"master-0: fencing at risk (agent running but not managed for recovery)"}},
			wantDegraded:        1,
			wantTroubleshooting: 0,
		},
		{
			name:                "maintenance mode routes to degraded",
			status:              &HealthStatus{Errors: []string{"Cluster is in maintenance mode"}},
			wantDegraded:        1,
			wantTroubleshooting: 0,
		},
		{
			name:                "stale status routes to troubleshooting",
			status:              &HealthStatus{Errors: []string{"Pacemaker status is stale (last updated: 2024-01-01T00:00:00Z)"}},
			wantDegraded:        0,
			wantTroubleshooting: 1,
		},
		{
			name:                "no status routes to troubleshooting",
			status:              &HealthStatus{Errors: []string{"PacemakerCluster CR has no status populated"}},
			wantDegraded:        0,
			wantTroubleshooting: 1,
		},
		{
			name: "mixed problems split correctly",
			status: &HealthStatus{
				Errors:   []string{"Node master-0 is offline", "Pacemaker status is stale (last updated: 2024-01-01T00:00:00Z)"},
				Warnings: []string{"master-1: fencing at risk (agent running but not managed for recovery)"},
			},
			wantDegraded:        2,
			wantTroubleshooting: 1,
		},
		{
			name:                "no problems produces empty lists",
			status:              &HealthStatus{},
			wantDegraded:        0,
			wantTroubleshooting: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			degraded, troubleshooting := classifyProblems(tt.status)
			require.Len(t, degraded, tt.wantDegraded, "degraded: %v", degraded)
			require.Len(t, troubleshooting, tt.wantTroubleshooting, "troubleshooting: %v", troubleshooting)
		})
	}
}

// ---------------------------------------------------------------------------
// Sync-level tests
// ---------------------------------------------------------------------------

func newTestController(cr *pacmkrv1.PacemakerCluster) (*consoleNotificationController, *fakedynamic.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	dynClient := fakedynamic.NewSimpleDynamicClient(scheme)

	return &consoleNotificationController{
		dynamicClient:     dynClient,
		recorder:          events.NewInMemoryRecorder("test", clocktesting.NewFakeClock(time.Now())),
		pacemakerInformer: testutil.CreateFakeInformer(cr),
	}, dynClient
}

func getCreatedName(t *testing.T, dynClient *fakedynamic.FakeDynamicClient) string {
	t.Helper()
	for _, a := range dynClient.Actions() {
		if a.GetVerb() == "create" {
			obj := a.(interface{ GetObject() runtime.Object }).GetObject()
			return obj.(*unstructured.Unstructured).GetName()
		}
	}
	t.Fatal("no create action found")
	return ""
}

func countActions(dynClient *fakedynamic.FakeDynamicClient, verb string) int {
	count := 0
	for _, a := range dynClient.Actions() {
		if a.GetVerb() == verb {
			count++
		}
	}
	return count
}

func TestSync_HealthyCluster_DeletesBothNotifications(t *testing.T) {
	cr := testCluster(withNodes(testNode("master-0", "192.168.111.20"), testNode("master-1", "192.168.111.21")))
	ctrl, dynClient := newTestController(cr)

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, countActions(dynClient, "delete"), "expected delete actions for both notification categories")
}

func TestSync_NodeOffline_CreatesDegradedNotification(t *testing.T) {
	cr := testCluster(withNodes(
		testNode("master-0", "192.168.111.20", offline()),
		testNode("master-1", "192.168.111.21"),
	))
	ctrl, dynClient := newTestController(cr)

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, categoryDegraded.name, getCreatedName(t, dynClient))
}

func TestSync_StaleStatus_CreatesTroubleshootingNotification(t *testing.T) {
	cr := testCluster(staleBy(10*time.Minute), withNodes(testNode("master-0", "192.168.111.20")))
	ctrl, dynClient := newTestController(cr)

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, categoryTroubleshooting.name, getCreatedName(t, dynClient))
}

func TestSync_FencingDegraded_CreatesDegradedNotification(t *testing.T) {
	cr := testCluster(withNodes(
		testNode("master-0", "192.168.111.20", fencingDegraded()),
		testNode("master-1", "192.168.111.21"),
	))
	ctrl, dynClient := newTestController(cr)

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, categoryDegraded.name, getCreatedName(t, dynClient))
}

func TestSync_UninitializedCR_DeletesBothNotifications(t *testing.T) {
	cr := &pacmkrv1.PacemakerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: PacemakerClusterResourceName},
	}
	ctrl, dynClient := newTestController(cr)

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, countActions(dynClient, "delete"), "expected delete actions for both categories")
	require.Equal(t, 0, countActions(dynClient, "create"), "should not create any notification")
}

func TestSync_CRNotFound_DeletesBothNotifications(t *testing.T) {
	ctrl, dynClient := newTestController(nil)

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, countActions(dynClient, "delete"), "expected delete actions for both categories")
}

func TestSync_ConsoleUnavailable_SkipsSilently(t *testing.T) {
	cr := testCluster(withNodes(
		testNode("master-0", "192.168.111.20", unhealthyEtcd()),
		testNode("master-1", "192.168.111.21"),
	))
	ctrl, _ := newTestController(cr)
	ctrl.consoleUnavailable = true

	err := ctrl.sync(context.Background(), nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// buildNotificationUnstructured tests
// ---------------------------------------------------------------------------

func TestBuildNotificationUnstructured_DegradedCategory(t *testing.T) {
	u, err := buildNotificationUnstructured(categoryDegraded, "Node master-0 is offline. Check pacemaker status for details.")
	require.NoError(t, err)
	require.Equal(t, categoryDegraded.name, u.GetName())

	text, _, _ := unstructured.NestedString(u.Object, "spec", "text")
	require.Contains(t, text, "offline")

	bg, _, _ := unstructured.NestedString(u.Object, "spec", "backgroundColor")
	require.Equal(t, notificationBackgroundColor, bg)

	href, _, _ := unstructured.NestedString(u.Object, "spec", "link", "href")
	require.Equal(t, categoryDegraded.linkHref, href)

	linkText, _, _ := unstructured.NestedString(u.Object, "spec", "link", "text")
	require.Equal(t, categoryDegraded.linkText, linkText)
}

func TestBuildNotificationUnstructured_TroubleshootingCategory(t *testing.T) {
	u, err := buildNotificationUnstructured(categoryTroubleshooting, "Pacemaker status is stale. Check pacemaker status for details.")
	require.NoError(t, err)
	require.Equal(t, categoryTroubleshooting.name, u.GetName())

	href, _, _ := unstructured.NestedString(u.Object, "spec", "link", "href")
	require.Equal(t, categoryTroubleshooting.linkHref, href)

	linkText, _, _ := unstructured.NestedString(u.Object, "spec", "link", "text")
	require.Equal(t, categoryTroubleshooting.linkText, linkText)
}
