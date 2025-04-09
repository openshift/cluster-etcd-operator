package etcd

import (
	"context"
	"strings"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/tools"
)

// RemoveStaticContainer removes the CEO managed etcd container
func RemoveStaticContainer(ctx context.Context, operatorClient *operatorversionedclient.Clientset) error {
	klog.Info("Checking etcd operator")

	etcd, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		klog.Error(err, "Failed to get Etcd")
		return err
	}

	if !strings.Contains(etcd.Spec.UnsupportedConfigOverrides.String(), "useUnsupportedUnsafeEtcdContainerRemoval") {
		klog.Info("Patching Etcd")
		oldOverrides := etcd.Spec.UnsupportedConfigOverrides.Raw
		newOverrides, err := tools.AddToRawJson(oldOverrides, "useUnsupportedUnsafeEtcdContainerRemoval", true)
		// TODO ADD 2ND FLAG!!
		if err != nil {
			klog.Error(err, "Failed to add useUnsupportedUnsafeEtcdContainerRemoval override to Etcd")
			return err
		}
		etcd.Spec.UnsupportedConfigOverrides = runtime.RawExtension{
			Raw: newOverrides,
		}

		etcd, err = operatorClient.OperatorV1().Etcds().Update(ctx, etcd, metav1.UpdateOptions{})
		if err != nil {
			klog.Error(err, "Failed to update Etcd")
			return err
		}

	}

	err = waitForStaticContainerRemoved(ctx, operatorClient)
	if err != nil {
		klog.Error(err, "Failed to wait for Etcd container removal")
		return err
	}

	return nil
}

// waitForStaticContainerRemoved waits until the static etcd container has been removed
func waitForStaticContainerRemoved(ctx context.Context, operatorClient *operatorversionedclient.Clientset) error {
	klog.Info("Wait for static etcd removed")

	isRemoved := func(context.Context) (done bool, err error) {
		etcd, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			klog.Error(err, "Failed to get Etcd, but will ignore error for now...")
			return false, nil
		}

		removed := true
		for _, nodeStatus := range etcd.Status.NodeStatuses {
			if nodeStatus.CurrentRevision == etcd.Status.LatestAvailableRevision {
				klog.Info("static etcd removed", "node", nodeStatus.NodeName, "current revision", nodeStatus.CurrentRevision, "latest revision", etcd.Status.LatestAvailableRevision)
			} else {
				klog.Info("static etcd not removed yet", "node", nodeStatus.NodeName, "current revision", nodeStatus.CurrentRevision, "latest revision", etcd.Status.LatestAvailableRevision)
				removed = false
			}
		}
		return removed, nil
	}

	err := wait.PollUntilContextTimeout(ctx, 15*time.Second, 5*time.Minute, true, isRemoved)
	return err

}
