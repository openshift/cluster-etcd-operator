package generation

import (
	"context"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// UpgradeInProgress reads live state so a deferred operation cannot run on an old decision.
func UpgradeInProgress(ctx context.Context, kube kubernetes.Interface, config configclient.ConfigV1Interface) (bool, error) {
	cv, err := config.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, c := range cv.Status.Conditions {
		if c.Type == configv1.OperatorProgressing && c.Status == configv1.ConditionTrue {
			return true, nil
		}
	}
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
		if a["machineconfiguration.openshift.io/state"] == "Working" {
			return true, nil
		}
	}
	return false, nil
}
