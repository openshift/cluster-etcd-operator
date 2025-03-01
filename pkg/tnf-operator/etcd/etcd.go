package etcd

import (
	"context"
	"strings"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	tools "github.com/openshift/cluster-etcd-operator/pkg/tnf-operator/tools"
)

// RemoveStaticContainer removes the CEO managed etcd container
func RemoveStaticContainer(ctx context.Context, operatorClient *operatorversionedclient.Clientset) (*operatorv1.Etcd, error) {
	klog.Info("Checking etcd operator")

	etcd, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		klog.Error(err, "Failed to get Etcd")
		return nil, err
	}

	if !strings.Contains(etcd.Spec.UnsupportedConfigOverrides.String(), "useUnsupportedUnsafeEtcdContainerRemoval") {
		klog.Info("Patching Etcd")
		oldOverrides := etcd.Spec.UnsupportedConfigOverrides.Raw
		newOverrides, err := tools.AddToRawJson(oldOverrides, "useUnsupportedUnsafeEtcdContainerRemoval", true)
		if err != nil {
			klog.Error(err, "Failed to add useUnsupportedUnsafeEtcdContainerRemoval override to Etcd")
			return nil, err
		}
		etcd.Spec.UnsupportedConfigOverrides = runtime.RawExtension{
			Raw: newOverrides,
		}

		etcd, err = operatorClient.OperatorV1().Etcds().Update(ctx, etcd, metav1.UpdateOptions{})
		if err != nil {
			klog.Error(err, "Failed to update Etcd")
			return nil, err
		}
	}
	return etcd, nil
}

// StaticContainerRemoved checks if the static etcd container has been removed
func StaticContainerRemoved(etcd *operatorv1.Etcd) bool {
	klog.Info("Wait for static etcd removed")
	removed := true
	for _, nodeStatus := range etcd.Status.NodeStatuses {
		if nodeStatus.CurrentRevision == etcd.Status.LatestAvailableRevision {
			klog.Info("static etcd removed", "node", nodeStatus.NodeName, "current revision", nodeStatus.CurrentRevision, "latest revision", etcd.Status.LatestAvailableRevision)
		} else {
			klog.Info("static etcd not removed yet", "node", nodeStatus.NodeName, "current revision", nodeStatus.CurrentRevision, "latest revision", etcd.Status.LatestAvailableRevision)
			removed = false
		}
	}
	return removed
}
