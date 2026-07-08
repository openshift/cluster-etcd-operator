package metriccontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

const resyncInterval = 30 * time.Second

type pacemakerMetricsController struct {
	pacemakerInformer cache.SharedIndexInformer
}

func NewPacemakerMetricsController(
	pacemakerInformer cache.SharedIndexInformer,
	recorder events.Recorder,
	metricsRegistry metrics.KubeRegistry,
) factory.Controller {
	c := &pacemakerMetricsController{
		pacemakerInformer: pacemakerInformer,
	}

	for _, g := range clusterGauges {
		metricsRegistry.MustRegister(g)
	}
	for _, gv := range nodeGaugeVecs {
		metricsRegistry.MustRegister(gv)
	}
	for _, gv := range resourceGaugeVecs {
		metricsRegistry.MustRegister(gv)
	}

	return factory.New().
		ResyncEvery(resyncInterval).
		WithSync(c.sync).
		WithInformers(pacemakerInformer).
		ToController("PacemakerMetricsController", recorder)
}

func (c *pacemakerMetricsController) sync(ctx context.Context, _ factory.SyncContext) error {
	klog.V(4).Infof("syncing Pacemaker metrics")

	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(pacemaker.PacemakerClusterResourceName)
	if err != nil {
		return err
	}

	if !exists {
		klog.V(4).Infof("PacemakerCluster CR not found, resetting metrics")
		resetAllMetrics()
		return nil
	}

	cr, ok := item.(*pacmkrv1.PacemakerCluster)
	if !ok {
		return fmt.Errorf("unexpected object type in informer store: %T", item)
	}

	for _, gv := range nodeGaugeVecs {
		gv.Reset()
	}
	for _, gv := range resourceGaugeVecs {
		gv.Reset()
	}

	for _, m := range clusterConditionMetrics {
		cond := pacemaker.FindCondition(cr.Status.Conditions, m.conditionType)
		m.gauge.Set(conditionToFloat64(cond))
	}

	if cr.Status.Nodes == nil {
		klog.V(4).Infof("synced Pacemaker metrics: 0 nodes")
		return nil
	}

	nodes := *cr.Status.Nodes
	for i := range nodes {
		node := &nodes[i]
		nodeName := node.NodeName

		for _, m := range nodeConditionMetrics {
			cond := pacemaker.FindCondition(node.Conditions, m.conditionType)
			m.gaugeVec.WithLabelValues(nodeName).Set(conditionToFloat64(cond))
		}

		for j := range node.Resources {
			resource := &node.Resources[j]
			resourceName := string(resource.Name)

			for _, m := range resourceConditionMetrics {
				cond := pacemaker.FindCondition(resource.Conditions, m.conditionType)
				m.gaugeVec.WithLabelValues(nodeName, resourceName).Set(conditionToFloat64(cond))
			}
		}
	}

	klog.V(4).Infof("synced Pacemaker metrics: %d nodes", len(nodes))
	return nil
}

func resetAllMetrics() {
	for _, g := range clusterGauges {
		g.Set(0)
	}
	for _, gv := range nodeGaugeVecs {
		gv.Reset()
	}
	for _, gv := range resourceGaugeVecs {
		gv.Reset()
	}
}
