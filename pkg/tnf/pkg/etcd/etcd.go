package etcd

import (
	"context"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
)

// RemoveStaticContainer informs CEO to remove its etcd container
func RemoveStaticContainer(ctx context.Context, operatorClient operatorversionedclient.Interface, kubeClient kubernetes.Interface) error {
	klog.Info("Signaling CEO that TNF setup is ready for etcd container removal")

	// Set the job status condition as signal to CEO
	err := jobs.SetTNFReadyForEtcdContainerRemoval(ctx, kubeClient)
	if err != nil {
		klog.Errorf("Failed to set TNF ready condition: %v", err)
		return err
	}

	// Wait for CEO to respond by removing static containers
	err = waitForStaticContainerRemoved(ctx, operatorClient)
	if err != nil {
		klog.Error(err, "Failed to wait for Etcd container removal")
		return err
	}

	return nil
}

// waitForStaticContainerRemoved waits until the static etcd container has been removed
func waitForStaticContainerRemoved(ctx context.Context, operatorClient operatorversionedclient.Interface) error {
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
