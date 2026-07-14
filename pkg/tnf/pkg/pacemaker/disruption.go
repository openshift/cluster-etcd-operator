package pacemaker

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
)

var resourceDisruptionCounter = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Namespace:      "tnf",
		Subsystem:      "resource",
		Name:           "disruption_total",
		Help:           "Number of started-to-stopped transitions observed for a TNF resource",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"node", "resource"},
)

type resourceKey struct {
	node     string
	resource string
}

// DisruptionTracker detects started-to-stopped resource transitions and
// increments an in-memory Prometheus counter. The counter survives API
// outages (lives in the CEO pod) so Prometheus can detect disruptions
// retrospectively via rate(tnf_resource_disruption_total[10m]) > 0.
type DisruptionTracker struct {
	mu          sync.Mutex
	lastStarted map[resourceKey]bool
}

func NewDisruptionTracker() *DisruptionTracker {
	legacyregistry.MustRegister(resourceDisruptionCounter)
	return &DisruptionTracker{
		lastStarted: make(map[resourceKey]bool),
	}
}

// TrackResourceStates compares current resource states against previously
// observed states. For each started→stopped transition, the disruption
// counter is incremented. On the first call (no prior state), states are
// recorded without incrementing.
func (dt *DisruptionTracker) TrackResourceStates(nodes *[]pacmkrv1.PacemakerClusterNodeStatus) {
	if nodes == nil {
		return
	}

	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, node := range *nodes {
		for _, resource := range node.Resources {
			key := resourceKey{
				node:     node.NodeName,
				resource: string(resource.Name),
			}

			startedCondition := FindCondition(resource.Conditions, pacmkrv1.ResourceStartedConditionType)
			currentlyStarted := startedCondition != nil && startedCondition.Status == metav1.ConditionTrue

			previouslyStarted, tracked := dt.lastStarted[key]

			if tracked && previouslyStarted && !currentlyStarted {
				resourceDisruptionCounter.WithLabelValues(node.NodeName, string(resource.Name)).Inc()
				klog.Infof("Resource disruption detected: %s on %s transitioned from started to stopped", resource.Name, node.NodeName)
			}

			dt.lastStarted[key] = currentlyStarted
		}
	}
}
