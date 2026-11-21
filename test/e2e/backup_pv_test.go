package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

var nodeSelectorOperators = map[corev1.NodeSelectorOperator]selection.Operator{
	corev1.NodeSelectorOpIn:           selection.In,
	corev1.NodeSelectorOpNotIn:        selection.NotIn,
	corev1.NodeSelectorOpExists:       selection.Exists,
	corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
	corev1.NodeSelectorOpGt:           selection.GreaterThan,
	corev1.NodeSelectorOpLt:           selection.LessThan,
}

// matchesNodeLabels reports whether the required node affinity of a PV accepts a
// node when only the node's labels are available. It mirrors, using apimachinery
// label selectors, the labels-only matching that upstream Kubernetes' scheduler
// volume binding check applies to PV node affinity; it does not run the scheduler.
// Terms are ORed and the requirements of a term are ANDed. matchFields cannot be
// evaluated without node fields, so they are skipped, as Kubernetes node affinity
// matching does for a node without fields; a term with neither expressions nor
// fields matches nothing.
func matchesNodeLabels(t *testing.T, affinity *corev1.VolumeNodeAffinity, nodeLabels map[string]string) bool {
	t.Helper()
	if affinity == nil || affinity.Required == nil {
		return true
	}
	for _, term := range affinity.Required.NodeSelectorTerms {
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		selector := labels.NewSelector()
		for _, expr := range term.MatchExpressions {
			op, ok := nodeSelectorOperators[expr.Operator]
			require.Truef(t, ok, "unsupported node selector operator %q", expr.Operator)
			req, err := labels.NewRequirement(expr.Key, op, expr.Values)
			require.NoError(t, err)
			selector = selector.Add(*req)
		}
		if selector.Matches(labels.Set(nodeLabels)) {
			return true
		}
	}
	return false
}

func controlPlaneNode(name, hostname string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			corev1.LabelHostname:                    hostname,
			"node-role.kubernetes.io/control-plane": "",
			"node-role.kubernetes.io/master":        "",
		},
	}}
}

// The backup writer, garbage collection and file finder pods only share files when
// they run on the node holding the hostPath directory, so the PV's node affinity
// must reject every other node when matched against node labels.
func TestHostPathPVPinsToSelectedNode(t *testing.T) {
	t.Parallel()
	// The hostname label differs from the node name.
	selected := controlPlaneNode("master-0.example.internal", "master-0")
	other := controlPlaneNode("master-1.example.internal", "master-1")
	// Named like the selected node's hostname label, but a different host.
	nameTwin := controlPlaneNode("master-0", "master-2")

	pv, err := newHostPathPV("backup-pvc", selected)
	require.NoError(t, err)

	affinity := pv.Spec.NodeAffinity
	require.NotNil(t, affinity)
	require.True(t, matchesNodeLabels(t, affinity, selected.Labels), "the selected node must be accepted")
	require.False(t, matchesNodeLabels(t, affinity, other.Labels), "another control-plane node must be rejected")
	require.False(t, matchesNodeLabels(t, affinity, nameTwin.Labels), "a node named like the selected hostname must be rejected")
	for _, term := range affinity.Required.NodeSelectorTerms {
		require.Empty(t, term.MatchFields, "matchFields are not evaluated when PV node affinity is matched against node labels")
	}

	// The storage constraints of the fixture are unchanged.
	require.Equal(t, "backup-pvc-pv", pv.Name)
	require.Equal(t, "manual", pv.Spec.StorageClassName)
	require.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pv.Spec.AccessModes)
	require.Equal(t, resource.NewQuantity(10, "Gi").Value(), pv.Spec.Capacity.Storage().Value())
	require.NotNil(t, pv.Spec.HostPath)
	require.Equal(t, HostPathBasePath+"backup-pvc", pv.Spec.HostPath.Path)
}

func TestHostPathPVRequiresHostnameLabel(t *testing.T) {
	t.Parallel()
	for name, nodeLabels := range map[string]map[string]string{
		"missing": {"node-role.kubernetes.io/control-plane": ""},
		"empty":   {corev1.LabelHostname: "", "node-role.kubernetes.io/control-plane": ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-0", Labels: nodeLabels}}
			_, err := newHostPathPV("backup-pvc", node)
			require.ErrorContains(t, err, corev1.LabelHostname)
		})
	}
}
