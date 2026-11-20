package generation

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// MCOUpdateInProgress reads control-plane Node annotations immediately before
// admission/mutation. ClusterVersion progression is deliberately not a gate:
// the rest of the upgrade can continue after these nodes finish their rollout.
func MCOUpdateInProgress(ctx context.Context, kube kubernetes.Interface) (bool, error) {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: tools.ControlPlaneNodeLabelSelector})
	if err != nil {
		return false, err
	}
	for _, node := range nodes.Items {
		a := node.Annotations
		current, desired := a["machineconfiguration.openshift.io/currentConfig"], a["machineconfiguration.openshift.io/desiredConfig"]
		if desired != "" && current != desired {
			return true, nil
		}
		if a["machineconfiguration.openshift.io/state"] == "Working" ||
			a["machineconfiguration.openshift.io/post-config-action"] == "Rebooting" {
			return true, nil
		}
		// Done/current==desired may be published before an uncordon request
		// is acknowledged. Keep that outstanding MCO operation inside the gate.
		drain := a["machineconfiguration.openshift.io/desiredDrain"]
		if drain != "" && drain != a["machineconfiguration.openshift.io/lastAppliedDrain"] {
			return true, nil
		}
	}
	return false, nil
}
