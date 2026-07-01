package metriccontroller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
)

var (
	clusterHealthyGauge = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "cluster",
		Name:           "healthy",
		Help:           "Whether the TNF cluster is healthy (1=healthy, 0=unhealthy)",
		StabilityLevel: metrics.ALPHA,
	})
	clusterInServiceGauge = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "cluster",
		Name:           "in_service",
		Help:           "Whether the TNF cluster is in service (1=in-service, 0=maintenance)",
		StabilityLevel: metrics.ALPHA,
	})
	clusterNodeCountAsExpectedGauge = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "cluster",
		Name:           "node_count_as_expected",
		Help:           "Whether the TNF cluster has the expected node count (1=expected, 0=mismatch)",
		StabilityLevel: metrics.ALPHA,
	})

	nodeHealthyGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "healthy",
		Help:           "Whether a TNF node is healthy (1=healthy, 0=unhealthy)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeOnlineGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "online",
		Help:           "Whether a TNF node is online (1=online, 0=offline)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeInServiceGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "in_service",
		Help:           "Whether a TNF node is in service (1=in-service, 0=maintenance)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeActiveGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "active",
		Help:           "Whether a TNF node is active (1=active, 0=standby)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeReadyGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "ready",
		Help:           "Whether a TNF node is ready (1=ready, 0=pending)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeCleanGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "clean",
		Help:           "Whether a TNF node is in a clean state (1=clean, 0=unclean)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeMemberGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "member",
		Help:           "Whether a TNF node is a cluster member (1=member, 0=not-member)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeFencingAvailableGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "fencing_available",
		Help:           "Whether a TNF node has at least one healthy fence device (1=available, 0=unavailable)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})
	nodeFencingHealthyGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "node",
		Name:           "fencing_healthy",
		Help:           "Whether all fence devices for a TNF node are healthy (1=all-healthy, 0=degraded)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node"})

	resourceHealthyGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "healthy",
		Help:           "Whether a TNF resource is healthy (1=healthy, 0=unhealthy)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceInServiceGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "in_service",
		Help:           "Whether a TNF resource is in service (1=in-service, 0=maintenance)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceManagedGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "managed",
		Help:           "Whether a TNF resource is managed by Pacemaker (1=managed, 0=unmanaged)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceEnabledGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "enabled",
		Help:           "Whether a TNF resource is enabled (1=enabled, 0=disabled)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceOperationalGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "operational",
		Help:           "Whether a TNF resource is operational (1=operational, 0=failed)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceActiveGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "active",
		Help:           "Whether a TNF resource is active (1=active, 0=inactive)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceStartedGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "started",
		Help:           "Whether a TNF resource is started (1=started, 0=stopped)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
	resourceSchedulableGauge = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "schedulable",
		Help:           "Whether a TNF resource is schedulable (1=schedulable, 0=unschedulable)",
		StabilityLevel: metrics.ALPHA,
	}, []string{"node", "resource"})
)

var clusterGauges = []*metrics.Gauge{
	clusterHealthyGauge,
	clusterInServiceGauge,
	clusterNodeCountAsExpectedGauge,
}

var nodeGaugeVecs = []*metrics.GaugeVec{
	nodeHealthyGauge,
	nodeOnlineGauge,
	nodeInServiceGauge,
	nodeActiveGauge,
	nodeReadyGauge,
	nodeCleanGauge,
	nodeMemberGauge,
	nodeFencingAvailableGauge,
	nodeFencingHealthyGauge,
}

var resourceGaugeVecs = []*metrics.GaugeVec{
	resourceHealthyGauge,
	resourceInServiceGauge,
	resourceManagedGauge,
	resourceEnabledGauge,
	resourceOperationalGauge,
	resourceActiveGauge,
	resourceStartedGauge,
	resourceSchedulableGauge,
}

type clusterConditionMapping struct {
	conditionType string
	gauge         *metrics.Gauge
}

type nodeConditionMapping struct {
	conditionType string
	gaugeVec      *metrics.GaugeVec
}

type resourceConditionMapping struct {
	conditionType string
	gaugeVec      *metrics.GaugeVec
}

var clusterConditionMetrics = []clusterConditionMapping{
	{pacmkrv1.ClusterHealthyConditionType, clusterHealthyGauge},
	{pacmkrv1.ClusterInServiceConditionType, clusterInServiceGauge},
	{pacmkrv1.ClusterNodeCountAsExpectedConditionType, clusterNodeCountAsExpectedGauge},
}

var nodeConditionMetrics = []nodeConditionMapping{
	{pacmkrv1.NodeHealthyConditionType, nodeHealthyGauge},
	{pacmkrv1.NodeOnlineConditionType, nodeOnlineGauge},
	{pacmkrv1.NodeInServiceConditionType, nodeInServiceGauge},
	{pacmkrv1.NodeActiveConditionType, nodeActiveGauge},
	{pacmkrv1.NodeReadyConditionType, nodeReadyGauge},
	{pacmkrv1.NodeCleanConditionType, nodeCleanGauge},
	{pacmkrv1.NodeMemberConditionType, nodeMemberGauge},
	{pacmkrv1.NodeFencingAvailableConditionType, nodeFencingAvailableGauge},
	{pacmkrv1.NodeFencingHealthyConditionType, nodeFencingHealthyGauge},
}

var resourceConditionMetrics = []resourceConditionMapping{
	{pacmkrv1.ResourceHealthyConditionType, resourceHealthyGauge},
	{pacmkrv1.ResourceInServiceConditionType, resourceInServiceGauge},
	{pacmkrv1.ResourceManagedConditionType, resourceManagedGauge},
	{pacmkrv1.ResourceEnabledConditionType, resourceEnabledGauge},
	{pacmkrv1.ResourceOperationalConditionType, resourceOperationalGauge},
	{pacmkrv1.ResourceActiveConditionType, resourceActiveGauge},
	{pacmkrv1.ResourceStartedConditionType, resourceStartedGauge},
	{pacmkrv1.ResourceSchedulableConditionType, resourceSchedulableGauge},
}

func conditionToFloat64(cond *metav1.Condition) float64 {
	if cond != nil && cond.Status == metav1.ConditionTrue {
		return 1.0
	}
	return 0.0
}
