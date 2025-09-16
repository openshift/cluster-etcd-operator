package etcd

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	OperatorConditionExternalEtcdReady = "ExternalEtcdReady"
)

// RemoveStaticContainer informs CEO to remove its etcd container
func RemoveStaticContainer(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	klog.Info("Signaling CEO that TNF setup is ready for etcd container removal")

	_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    OperatorConditionExternalEtcdReady,
		Status:  operatorv1.ConditionTrue,
		Reason:  "PacemakerConfiguredForEtcdTakeover",
		Message: "pacemaker's resource agent is ready to takeover the etcd container",
	}))

	if err != nil {
		return fmt.Errorf("error while updating ExternalEtcdReady operator condition: %w", err)
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
func waitForStaticContainerRemoved(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	klog.Info("Wait for static etcd removed")

	isRemoved := func(context.Context) (done bool, err error) {
		_, status, _, err := operatorClient.GetStaticPodOperatorState()
		if err != nil {
			klog.Error(err, "Failed to get Etcd, but will ignore error for now...")
			return false, nil
		}

		removed := true
		for _, nodeStatus := range status.NodeStatuses {
			if nodeStatus.CurrentRevision == status.LatestAvailableRevision {
				klog.Infof("static etcd removed: node %s, current rev %v, latest rev %v", nodeStatus.NodeName, nodeStatus.CurrentRevision, status.LatestAvailableRevision)
			} else {
				klog.Infof("static etcd not removed yet: node %s, current rev %v, latest rev %v", nodeStatus.NodeName, nodeStatus.CurrentRevision, status.LatestAvailableRevision)
				removed = false
			}
		}
		return removed, nil
	}

	// set immediate to false in order to give CEO some time to actually create a new revision if needed
	err := wait.PollUntilContextTimeout(ctx, 10*time.Second, 10*time.Minute, false, isRemoved)
	return err
}
