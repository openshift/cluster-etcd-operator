package pacemaker

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics/testutil"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
)

func newTestDisruptionTracker() *DisruptionTracker {
	return NewDisruptionTracker()
}

func makeNodeStatus(nodeName string, resources []pacmkrv1.PacemakerClusterResourceStatus) pacmkrv1.PacemakerClusterNodeStatus {
	return pacmkrv1.PacemakerClusterNodeStatus{
		NodeName:  nodeName,
		Resources: resources,
	}
}

func makeResource(name string, started bool) pacmkrv1.PacemakerClusterResourceStatus {
	status := metav1.ConditionFalse
	reason := pacmkrv1.ResourceStartedReasonStopped
	if started {
		status = metav1.ConditionTrue
		reason = pacmkrv1.ResourceStartedReasonStarted
	}
	return pacmkrv1.PacemakerClusterResourceStatus{
		Name: pacmkrv1.PacemakerClusterResourceName(name),
		Conditions: []metav1.Condition{
			{
				Type:   pacmkrv1.ResourceStartedConditionType,
				Status: status,
				Reason: reason,
			},
		},
	}
}

func getDisruptionCount(t *testing.T, node, resource string) float64 {
	t.Helper()
	val, err := testutil.GetCounterMetricValue(resourceDisruptionCounter.WithLabelValues(node, resource))
	require.NoError(t, err, "failed to read disruption counter for node=%s resource=%s", node, resource)
	return val
}

func TestDisruptionTracker_NilNodes(t *testing.T) {
	dt := newTestDisruptionTracker()
	dt.TrackResourceStates(nil)
	require.Empty(t, dt.lastStarted, "nil input should not add any tracked state")
}

func TestDisruptionTracker_FirstCallRecordsStateOnly(t *testing.T) {
	dt := newTestDisruptionTracker()
	nodes := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus("node-a", []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource("Etcd", true),
		}),
	}

	before := getDisruptionCount(t, "node-a", "Etcd")
	dt.TrackResourceStates(nodes)
	after := getDisruptionCount(t, "node-a", "Etcd")

	require.Equal(t, before, after, "first observation should not increment counter")
	require.True(t, dt.lastStarted[resourceKey{node: "node-a", resource: "Etcd"}],
		"should record started=true")
}

func TestDisruptionTracker_FirstCallStoppedResource(t *testing.T) {
	dt := newTestDisruptionTracker()
	nodes := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus("node-b", []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource("Etcd", false),
		}),
	}

	before := getDisruptionCount(t, "node-b", "Etcd")
	dt.TrackResourceStates(nodes)
	after := getDisruptionCount(t, "node-b", "Etcd")

	require.Equal(t, before, after, "first observation of stopped resource should not increment")
	require.False(t, dt.lastStarted[resourceKey{node: "node-b", resource: "Etcd"}],
		"should record started=false")
}

func TestDisruptionTracker_StartedToStopped(t *testing.T) {
	dt := newTestDisruptionTracker()
	node := "disruption-node-1"
	resource := "disruption-res-1"

	started := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(resource, true),
		}),
	}
	stopped := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(resource, false),
		}),
	}

	dt.TrackResourceStates(started)
	before := getDisruptionCount(t, node, resource)

	dt.TrackResourceStates(stopped)
	after := getDisruptionCount(t, node, resource)

	require.Equal(t, before+1, after, "started→stopped should increment counter by 1")
}

func TestDisruptionTracker_StoppedToStartedNoIncrement(t *testing.T) {
	dt := newTestDisruptionTracker()
	node := "recovery-node"
	resource := "recovery-res"

	stopped := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(resource, false),
		}),
	}
	started := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(resource, true),
		}),
	}

	dt.TrackResourceStates(stopped)
	before := getDisruptionCount(t, node, resource)

	dt.TrackResourceStates(started)
	after := getDisruptionCount(t, node, resource)

	require.Equal(t, before, after, "stopped→started (recovery) should not increment counter")
}

func TestDisruptionTracker_NoChangeNoIncrement(t *testing.T) {
	dt := newTestDisruptionTracker()
	node := "stable-node"

	startedRes := "stable-started"
	stoppedRes := "stable-stopped"

	nodes := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(startedRes, true),
			makeResource(stoppedRes, false),
		}),
	}

	dt.TrackResourceStates(nodes)
	beforeStarted := getDisruptionCount(t, node, startedRes)
	beforeStopped := getDisruptionCount(t, node, stoppedRes)

	dt.TrackResourceStates(nodes)
	afterStarted := getDisruptionCount(t, node, startedRes)
	afterStopped := getDisruptionCount(t, node, stoppedRes)

	require.Equal(t, beforeStarted, afterStarted, "started→started should not increment")
	require.Equal(t, beforeStopped, afterStopped, "stopped→stopped should not increment")
}

func TestDisruptionTracker_MultipleResources(t *testing.T) {
	dt := newTestDisruptionTracker()
	node := "multi-res-node"

	initial := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource("Etcd", true),
			makeResource("Kubelet", true),
		}),
	}
	dt.TrackResourceStates(initial)

	etcdBefore := getDisruptionCount(t, node, "Etcd")
	kubeletBefore := getDisruptionCount(t, node, "Kubelet")

	onlyEtcdStopped := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource("Etcd", false),
			makeResource("Kubelet", true),
		}),
	}
	dt.TrackResourceStates(onlyEtcdStopped)

	etcdAfter := getDisruptionCount(t, node, "Etcd")
	kubeletAfter := getDisruptionCount(t, node, "Kubelet")

	require.Equal(t, etcdBefore+1, etcdAfter, "Etcd should increment")
	require.Equal(t, kubeletBefore, kubeletAfter, "Kubelet should not increment")
}

func TestDisruptionTracker_MultipleNodes(t *testing.T) {
	dt := newTestDisruptionTracker()
	nodeA := "multi-node-a"
	nodeB := "multi-node-b"
	res := "multi-node-res"

	initial := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(nodeA, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, true),
		}),
		makeNodeStatus(nodeB, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, true),
		}),
	}
	dt.TrackResourceStates(initial)

	aBefore := getDisruptionCount(t, nodeA, res)
	bBefore := getDisruptionCount(t, nodeB, res)

	onlyAStopped := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(nodeA, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, false),
		}),
		makeNodeStatus(nodeB, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, true),
		}),
	}
	dt.TrackResourceStates(onlyAStopped)

	aAfter := getDisruptionCount(t, nodeA, res)
	bAfter := getDisruptionCount(t, nodeB, res)

	require.Equal(t, aBefore+1, aAfter, "node-a should increment")
	require.Equal(t, bBefore, bAfter, "node-b should not increment")
}

func TestDisruptionTracker_MissingStartedCondition(t *testing.T) {
	dt := newTestDisruptionTracker()
	node := "no-condition-node"
	res := "no-condition-res"

	startedFirst := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, true),
		}),
	}
	dt.TrackResourceStates(startedFirst)

	before := getDisruptionCount(t, node, res)

	noCondition := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			{
				Name:       pacmkrv1.PacemakerClusterResourceName(res),
				Conditions: []metav1.Condition{},
			},
		}),
	}
	dt.TrackResourceStates(noCondition)
	after := getDisruptionCount(t, node, res)

	require.Equal(t, before+1, after,
		"missing Started condition treated as not-started, should increment from previously-started")
}

func TestDisruptionTracker_ConsecutiveDisruptions(t *testing.T) {
	dt := newTestDisruptionTracker()
	node := "bounce-node"
	res := "bounce-res"

	started := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, true),
		}),
	}
	stopped := &[]pacmkrv1.PacemakerClusterNodeStatus{
		makeNodeStatus(node, []pacmkrv1.PacemakerClusterResourceStatus{
			makeResource(res, false),
		}),
	}

	dt.TrackResourceStates(started)
	baseline := getDisruptionCount(t, node, res)

	dt.TrackResourceStates(stopped) // disruption 1
	dt.TrackResourceStates(started) // recovery
	dt.TrackResourceStates(stopped) // disruption 2

	final := getDisruptionCount(t, node, res)
	require.Equal(t, baseline+2, final, "two started→stopped transitions should increment counter twice")
}
