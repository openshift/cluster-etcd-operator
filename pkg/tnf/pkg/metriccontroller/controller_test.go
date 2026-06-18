package metriccontroller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/clock"
)

// ---------------------------------------------------------------------------
// fakeInformer — copied from pacemaker/healthcheck_test.go (package-private)
// ---------------------------------------------------------------------------

func createFakeInformer(obj *pacmkrv1.PacemakerCluster) cache.SharedIndexInformer {
	store := cache.NewStore(func(obj any) (string, error) {
		if pc, ok := obj.(*pacmkrv1.PacemakerCluster); ok {
			return pc.Name, nil
		}
		return "", fmt.Errorf("object is not a PacemakerCluster")
	})
	if obj != nil {
		_ = store.Add(obj)
	}
	return &fakeInformer{store: store}
}

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

// ---------------------------------------------------------------------------
// CR builder
// ---------------------------------------------------------------------------

type crOption func(*pacmkrv1.PacemakerCluster)

func buildCluster(opts ...crOption) *pacmkrv1.PacemakerCluster {
	cr := &pacmkrv1.PacemakerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: pacmkrv1.PacemakerClusterStatus{
			Conditions: healthyClusterConditions(),
			Nodes:      healthyNodes("master-0", "master-1"),
		},
	}
	for _, opt := range opts {
		opt(cr)
	}
	return cr
}

func healthyClusterConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: pacmkrv1.ClusterHealthyConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ClusterInServiceConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ClusterNodeCountAsExpectedConditionType, Status: metav1.ConditionTrue},
	}
}

func healthyNodeConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeOnlineConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeInServiceConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeActiveConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeReadyConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeCleanConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeMemberConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeFencingAvailableConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.NodeFencingHealthyConditionType, Status: metav1.ConditionTrue},
	}
}

func healthyResourceConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: pacmkrv1.ResourceHealthyConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceInServiceConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceManagedConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceEnabledConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceOperationalConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceActiveConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceStartedConditionType, Status: metav1.ConditionTrue},
		{Type: pacmkrv1.ResourceSchedulableConditionType, Status: metav1.ConditionTrue},
	}
}

func healthyNodes(names ...string) *[]pacmkrv1.PacemakerClusterNodeStatus {
	nodes := make([]pacmkrv1.PacemakerClusterNodeStatus, len(names))
	for i, name := range names {
		nodes[i] = pacmkrv1.PacemakerClusterNodeStatus{
			NodeName:   name,
			Conditions: healthyNodeConditions(),
			Resources: []pacmkrv1.PacemakerClusterResourceStatus{
				{Name: pacmkrv1.PacemakerClusterResourceNameKubelet, Conditions: healthyResourceConditions()},
				{Name: pacmkrv1.PacemakerClusterResourceNameEtcd, Conditions: healthyResourceConditions()},
			},
		}
	}
	return &nodes
}

func withNodeCondition(nodeName, condType string, status metav1.ConditionStatus) crOption {
	return func(cr *pacmkrv1.PacemakerCluster) {
		if cr.Status.Nodes == nil {
			return
		}
		for i := range *cr.Status.Nodes {
			if (*cr.Status.Nodes)[i].NodeName == nodeName {
				setCondition(&(*cr.Status.Nodes)[i].Conditions, condType, status)
				return
			}
		}
	}
}

func withResourceCondition(nodeName string, resourceName pacmkrv1.PacemakerClusterResourceName, condType string, status metav1.ConditionStatus) crOption {
	return func(cr *pacmkrv1.PacemakerCluster) {
		if cr.Status.Nodes == nil {
			return
		}
		for i := range *cr.Status.Nodes {
			node := &(*cr.Status.Nodes)[i]
			if node.NodeName != nodeName {
				continue
			}
			for j := range node.Resources {
				if node.Resources[j].Name == resourceName {
					setCondition(&node.Resources[j].Conditions, condType, status)
					return
				}
			}
		}
	}
}

func withNilNodes() crOption {
	return func(cr *pacmkrv1.PacemakerCluster) {
		cr.Status.Nodes = nil
	}
}

func withNodes(names ...string) crOption {
	return func(cr *pacmkrv1.PacemakerCluster) {
		cr.Status.Nodes = healthyNodes(names...)
	}
}

func withoutNodeCondition(nodeName, condType string) crOption {
	return func(cr *pacmkrv1.PacemakerCluster) {
		if cr.Status.Nodes == nil {
			return
		}
		for i := range *cr.Status.Nodes {
			node := &(*cr.Status.Nodes)[i]
			if node.NodeName != nodeName {
				continue
			}
			filtered := make([]metav1.Condition, 0, len(node.Conditions))
			for _, c := range node.Conditions {
				if c.Type != condType {
					filtered = append(filtered, c)
				}
			}
			node.Conditions = filtered
			return
		}
	}
}

func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus) {
	for i := range *conditions {
		if (*conditions)[i].Type == condType {
			(*conditions)[i].Status = status
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func setupAndSync(t *testing.T, cr *pacmkrv1.PacemakerCluster) metrics.KubeRegistry {
	t.Helper()
	registry := metrics.NewKubeRegistry()
	informer := createFakeInformer(cr)
	recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
	controller := NewPacemakerMetricsController(informer, recorder, registry)
	err := controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder))
	require.NoError(t, err)
	return registry
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHealthyCluster(t *testing.T) {
	registry := setupAndSync(t, buildCluster())

	families, err := registry.Gather()
	require.NoError(t, err)

	familyNames := make(map[string]bool)
	totalSeries := 0
	for _, mf := range families {
		familyNames[mf.GetName()] = true
		for _, m := range mf.GetMetric() {
			require.InDelta(t, 1.0, m.GetGauge().GetValue(), 0.001,
				"metric %s should be 1.0 on healthy cluster", mf.GetName())
			totalSeries++
		}
	}

	require.Len(t, familyNames, 20, "expected 20 metric families")
	require.Equal(t, 53, totalSeries, "expected 53 series on 2-node cluster")

	require.True(t, familyNames["tnf_cluster_healthy"])
	require.True(t, familyNames["tnf_cluster_in_service"])
	require.True(t, familyNames["tnf_cluster_node_count_as_expected"])
	require.True(t, familyNames["tnf_node_online"])
	require.True(t, familyNames["tnf_resource_started"])
}

func TestNodeOffline(t *testing.T) {
	registry := setupAndSync(t, buildCluster(
		withNodeCondition("master-1", pacmkrv1.NodeOnlineConditionType, metav1.ConditionFalse),
		withNodeCondition("master-1", pacmkrv1.NodeHealthyConditionType, metav1.ConditionFalse),
	))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_node_online [ALPHA] Whether a TNF node is online (1=online, 0=offline)
# TYPE tnf_node_online gauge
tnf_node_online{node="master-0"} 1
tnf_node_online{node="master-1"} 0
`), "tnf_node_online"))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_node_healthy [ALPHA] Whether a TNF node is healthy (1=healthy, 0=unhealthy)
# TYPE tnf_node_healthy gauge
tnf_node_healthy{node="master-0"} 1
tnf_node_healthy{node="master-1"} 0
`), "tnf_node_healthy"))
}

func TestResourceStopped(t *testing.T) {
	registry := setupAndSync(t, buildCluster(
		withResourceCondition("master-0", pacmkrv1.PacemakerClusterResourceNameEtcd,
			pacmkrv1.ResourceStartedConditionType, metav1.ConditionFalse),
	))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_resource_started [ALPHA] Whether a TNF resource is started (1=started, 0=stopped)
# TYPE tnf_resource_started gauge
tnf_resource_started{node="master-0",resource="Etcd"} 0
tnf_resource_started{node="master-0",resource="Kubelet"} 1
tnf_resource_started{node="master-1",resource="Etcd"} 1
tnf_resource_started{node="master-1",resource="Kubelet"} 1
`), "tnf_resource_started"))
}

func TestMissingCR(t *testing.T) {
	registry := setupAndSync(t, nil)

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_cluster_healthy [ALPHA] Whether the TNF cluster is healthy (1=healthy, 0=unhealthy)
# TYPE tnf_cluster_healthy gauge
tnf_cluster_healthy 0
# HELP tnf_cluster_in_service [ALPHA] Whether the TNF cluster is in service (1=in-service, 0=maintenance)
# TYPE tnf_cluster_in_service gauge
tnf_cluster_in_service 0
# HELP tnf_cluster_node_count_as_expected [ALPHA] Whether the TNF cluster has the expected node count (1=expected, 0=mismatch)
# TYPE tnf_cluster_node_count_as_expected gauge
tnf_cluster_node_count_as_expected 0
`), "tnf_cluster_healthy", "tnf_cluster_in_service", "tnf_cluster_node_count_as_expected"))

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if strings.HasPrefix(mf.GetName(), "tnf_node_") || strings.HasPrefix(mf.GetName(), "tnf_resource_") {
			require.Empty(t, mf.GetMetric(), "expected no samples for %s after missing CR", mf.GetName())
		}
	}
}

func TestNilNodesPointer(t *testing.T) {
	registry := setupAndSync(t, buildCluster(withNilNodes()))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_cluster_healthy [ALPHA] Whether the TNF cluster is healthy (1=healthy, 0=unhealthy)
# TYPE tnf_cluster_healthy gauge
tnf_cluster_healthy 1
# HELP tnf_cluster_in_service [ALPHA] Whether the TNF cluster is in service (1=in-service, 0=maintenance)
# TYPE tnf_cluster_in_service gauge
tnf_cluster_in_service 1
# HELP tnf_cluster_node_count_as_expected [ALPHA] Whether the TNF cluster has the expected node count (1=expected, 0=mismatch)
# TYPE tnf_cluster_node_count_as_expected gauge
tnf_cluster_node_count_as_expected 1
`), "tnf_cluster_healthy", "tnf_cluster_in_service", "tnf_cluster_node_count_as_expected"))

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if strings.HasPrefix(mf.GetName(), "tnf_node_") || strings.HasPrefix(mf.GetName(), "tnf_resource_") {
			require.Empty(t, mf.GetMetric(), "expected no samples for %s with nil Nodes", mf.GetName())
		}
	}
}

func TestMissingCondition(t *testing.T) {
	registry := setupAndSync(t, buildCluster(
		withoutNodeCondition("master-0", pacmkrv1.NodeCleanConditionType),
	))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_node_clean [ALPHA] Whether a TNF node is in a clean state (1=clean, 0=unclean)
# TYPE tnf_node_clean gauge
tnf_node_clean{node="master-0"} 0
tnf_node_clean{node="master-1"} 1
`), "tnf_node_clean"))
}

func TestNodeRemovalBetweenSyncs(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	informer := createFakeInformer(buildCluster())
	recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
	controller := NewPacemakerMetricsController(informer, recorder, registry)
	syncCtx := factory.NewSyncContext("test", recorder)

	require.NoError(t, controller.Sync(context.TODO(), syncCtx))

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == "tnf_node_online" {
			require.Len(t, mf.GetMetric(), 2, "expected 2 node series before removal")
		}
	}

	oneNodeCR := buildCluster(withNodes("master-0"))
	require.NoError(t, informer.GetStore().Update(oneNodeCR))
	require.NoError(t, controller.Sync(context.TODO(), syncCtx))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_node_online [ALPHA] Whether a TNF node is online (1=online, 0=offline)
# TYPE tnf_node_online gauge
tnf_node_online{node="master-0"} 1
`), "tnf_node_online"))
}

func TestMultipleResourcesPerNode(t *testing.T) {
	registry := setupAndSync(t, buildCluster(
		withResourceCondition("master-0", pacmkrv1.PacemakerClusterResourceNameEtcd,
			pacmkrv1.ResourceStartedConditionType, metav1.ConditionFalse),
		withResourceCondition("master-1", pacmkrv1.PacemakerClusterResourceNameKubelet,
			pacmkrv1.ResourceStartedConditionType, metav1.ConditionFalse),
	))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_resource_started [ALPHA] Whether a TNF resource is started (1=started, 0=stopped)
# TYPE tnf_resource_started gauge
tnf_resource_started{node="master-0",resource="Etcd"} 0
tnf_resource_started{node="master-0",resource="Kubelet"} 1
tnf_resource_started{node="master-1",resource="Etcd"} 1
tnf_resource_started{node="master-1",resource="Kubelet"} 0
`), "tnf_resource_started"))
}

func TestLabelCorrectness(t *testing.T) {
	registry := setupAndSync(t, buildCluster(withNodes("node-alpha", "node-beta")))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_node_online [ALPHA] Whether a TNF node is online (1=online, 0=offline)
# TYPE tnf_node_online gauge
tnf_node_online{node="node-alpha"} 1
tnf_node_online{node="node-beta"} 1
`), "tnf_node_online"))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP tnf_resource_healthy [ALPHA] Whether a TNF resource is healthy (1=healthy, 0=unhealthy)
# TYPE tnf_resource_healthy gauge
tnf_resource_healthy{node="node-alpha",resource="Etcd"} 1
tnf_resource_healthy{node="node-alpha",resource="Kubelet"} 1
tnf_resource_healthy{node="node-beta",resource="Etcd"} 1
tnf_resource_healthy{node="node-beta",resource="Kubelet"} 1
`), "tnf_resource_healthy"))
}

func TestSyncIdempotency(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	informer := createFakeInformer(buildCluster())
	recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
	controller := NewPacemakerMetricsController(informer, recorder, registry)
	syncCtx := factory.NewSyncContext("test", recorder)

	require.NoError(t, controller.Sync(context.TODO(), syncCtx))
	first, err := registry.Gather()
	require.NoError(t, err)

	require.NoError(t, controller.Sync(context.TODO(), syncCtx))
	second, err := registry.Gather()
	require.NoError(t, err)

	require.Equal(t, len(first), len(second), "family count should be stable across syncs")
	for i := range first {
		require.Equal(t, first[i].GetName(), second[i].GetName())
		require.Equal(t, len(first[i].GetMetric()), len(second[i].GetMetric()),
			"series count for %s should be stable", first[i].GetName())
		for j := range first[i].GetMetric() {
			require.InDelta(t, first[i].GetMetric()[j].GetGauge().GetValue(),
				second[i].GetMetric()[j].GetGauge().GetValue(), 0.001,
				"value for %s should be stable", first[i].GetName())
		}
	}
}

func TestConditionToFloat64(t *testing.T) {
	require.InDelta(t, 1.0, conditionToFloat64(&metav1.Condition{Status: metav1.ConditionTrue}), 0.001)
	require.InDelta(t, 0.0, conditionToFloat64(&metav1.Condition{Status: metav1.ConditionFalse}), 0.001)
	require.InDelta(t, 0.0, conditionToFloat64(nil), 0.001)
}
