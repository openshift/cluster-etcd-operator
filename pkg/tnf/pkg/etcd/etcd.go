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

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// RemoveStaticContainer removes the CEO managed etcd container
func RemoveStaticContainer(ctx context.Context, operatorClient *operatorversionedclient.Clientset) error {
	klog.Info("Checking etcd operator")

	etcd, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		klog.Error(err, "Failed to get Etcd")
		return err
	}

	if !strings.Contains(etcd.Spec.UnsupportedConfigOverrides.String(), "useExternalEtcdSupport") {
		klog.Info("Patching Etcd")
		oldOverrides := etcd.Spec.UnsupportedConfigOverrides.Raw
		newOverrides, err := tools.AddToRawJson(oldOverrides, "useUnsupportedUnsafeEtcdContainerRemoval", true)
		newOverrides, err = tools.AddToRawJson(newOverrides, "useExternalEtcdSupport", true)
		if err != nil {
			klog.Error(err, "Failed to add useUnsupportedUnsafeEtcdContainerRemoval and useExternalEtcdSupport override to Etcd")
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
				klog.Infof("static etcd removed: node %s, current rev %v, latest rev %v", nodeStatus.NodeName, nodeStatus.CurrentRevision, etcd.Status.LatestAvailableRevision)
			} else {
				klog.Infof("static etcd not removed yet: node %s, current rev %v, latest rev %v", nodeStatus.NodeName, nodeStatus.CurrentRevision, etcd.Status.LatestAvailableRevision)
				removed = false
			}
		}
		return removed, nil
	}

	err := wait.PollUntilContextTimeout(ctx, 10*time.Second, 10*time.Minute, true, isRemoved)
	return err

}
