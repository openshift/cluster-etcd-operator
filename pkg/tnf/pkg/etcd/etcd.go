package etcd

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
)

// RemoveStaticContainer informs CEO to remove its etcd container
func RemoveStaticContainer(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	klog.Info("Signaling CEO that TNF setup is ready for etcd container transition")

	_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    ceohelpers.OperatorConditionExternalEtcdReadyForTransition,
		Status:  operatorv1.ConditionTrue,
		Reason:  "PacemakerConfiguredForEtcdTransition",
		Message: "pacemaker's resource agent is ready to takeover the etcd container",
	}))

	if err != nil {
		return fmt.Errorf("error while updating ExternalEtcdReadyForTransition operator condition: %w", err)
	}

	// Wait for CEO to respond by removing static containers
	err = waitForStaticContainerRemoved(ctx, operatorClient)
	if err != nil {
		klog.Error(err, "Failed to wait for etcd container transition")
		return err
	}

	return nil
}

// waitForStaticContainerRemoved waits until the static etcd container has been removed
func waitForStaticContainerRemoved(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	klog.Info("Wait for static etcd removed")

	// the container is removed when all nodes run the latest revision
	err := WaitForStableRevision(ctx, operatorClient)
	if err != nil {
		return err
	}

	// Update the operator status to indicate that the transition has completed.
	// As soon as the etcd container is removed, this operator won't be able to update this status
	// unless the etcd container is restarted by the pacemaker resource agent.
	_, _, err = v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition,
		Status:  operatorv1.ConditionTrue,
		Reason:  "PacemakerResourceAgentIsNowRunningEtcd",
		Message: "pacemaker's resource agent is now running the etcd container",
	}))

	return err
}

// WaitForStableRevision waits until all nodes run the latest available revision
func WaitForStableRevision(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	klog.Info("Wait for updated revision")

	isStableFunc := func(context.Context) (done bool, err error) {
		isStable, err := ceohelpers.IsRevisionStable(operatorClient)
		if err != nil {
			klog.Error(err, "failed to get Etcd, but will ignore error for now...")
			return false, nil
		}
		return isStable, nil
	}

	// set immediate to false in order to give CEO some time to actually create a new revision if needed
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, 10*time.Minute, false, isStableFunc)
}
