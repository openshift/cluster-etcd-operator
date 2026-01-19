package startupmonitorcondition

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// startupMonitorPodConditionController monitors the startup monitor pod and reports unwanted state/condition/phase as a degraded condition
type startupMonitorPodConditionController struct {
	operatorClient operatorv1helpers.StaticPodOperatorClient

	targetName string
	podLister  corev1listers.PodNamespaceLister

	startupMonitorEnabledFn func() (bool, error)
}

// New returns a controller for monitoring the lifecycle of the startup monitor pod
func New(targetNamespace string,
	targetName string,
	operatorClient operatorv1helpers.StaticPodOperatorClient,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	startupMonitorEnabledFn func() (bool, error),
	eventRecorder events.Recorder) factory.Controller {
	fd := &startupMonitorPodConditionController{
		operatorClient:          operatorClient,
		targetName:              targetName,
		podLister:               kubeInformersForNamespaces.InformersFor(targetNamespace).Core().V1().Pods().Lister().Pods(targetNamespace),
		startupMonitorEnabledFn: startupMonitorEnabledFn,
	}
	return factory.New().WithSync(fd.sync).ResyncEvery(6*time.Minute).WithInformers(kubeInformersForNamespaces.InformersFor(targetNamespace).Core().V1().Pods().Informer(), operatorClient.Informer()).ToController("StartupMonitorPodCondition", eventRecorder)
}

func (fd *startupMonitorPodConditionController) sync(ctx context.Context, _ factory.SyncContext) (err error) {
	startupPodDegraded := &operatorv1.OperatorCondition{Type: "StartupMonitorPodDegraded", Status: operatorv1.ConditionFalse}
	startupPodContainerExcessiveRestartsDegraded := &operatorv1.OperatorCondition{Type: "StartupMonitorPodContainerExcessiveRestartsDegraded", Status: operatorv1.ConditionFalse}

	defer func() {
		if err == nil {
			if _, _, updateError := operatorv1helpers.UpdateStatus(ctx, fd.operatorClient,
				operatorv1helpers.UpdateConditionFn(*startupPodDegraded),
				operatorv1helpers.UpdateConditionFn(*startupPodContainerExcessiveRestartsDegraded)); updateError != nil {
				err = updateError
			}
		}
	}()

	// in practice we rely on operators to provide
	// a condition for checking we are running on a single node cluster
	if enabled, err := fd.startupMonitorEnabledFn(); err != nil {
		return err
	} else if !enabled {
		return nil
	}

	_, operatorStatus, _, err := fd.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	var currentNodeName string
	for _, ns := range operatorStatus.NodeStatuses {
		// only one node is allowed to be progressing at a time
		if ns.TargetRevision > 0 {
			currentNodeName = ns.NodeName
			break
		}
	}

	if len(currentNodeName) == 0 {
		// nothing to do, no node is progressing
		return nil
	}

	monitorPod, err := fd.podLister.Get(fmt.Sprintf("%s-startup-monitor-%s", fd.targetName, currentNodeName))
	if errors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err // unknown error
	}

	// sets the degraded condition when the pod has been in pending for more than 2 minutes
	withDegradedWhenInPendingPhaseFor(monitorPod, 2*time.Minute, startupPodDegraded)

	// sets the degraded condition when the pod failed with non-zero exit code
	withDegradedWhenHasFailedContainer(monitorPod, startupPodDegraded)

	// set the degraded condition when the pod has been restarted at least twice
	withDegradedWhenHasExcessiveRestarts(monitorPod, 2, startupPodContainerExcessiveRestartsDegraded)

	return nil
}

func withDegradedWhenInPendingPhaseFor(monitorPod *corev1.Pod, maxPendingDuration time.Duration, startupPodDegraded *operatorv1.OperatorCondition) {
	if monitorPod.Status.Phase != corev1.PodPending || monitorPod.Status.StartTime == nil || time.Now().Sub(monitorPod.Status.StartTime.Time) < maxPendingDuration {
		return
	}

	startupPodDegraded.Status = operatorv1.ConditionTrue
	pendingPodReason, pendingPodMessage := "Unknown", "unknown"
	if len(monitorPod.Status.Reason) > 0 {
		pendingPodReason = monitorPod.Status.Reason
	}
	if len(monitorPod.Status.Message) > 0 {
		pendingPodMessage = monitorPod.Status.Message
	}
	startupPodDegraded.Reason = pendingPodReason
	startupPodDegraded.Message = fmt.Sprintf("the pod %v has been in %v phase for more than max tolerated time (%v) due to %v", monitorPod.Name, corev1.PodPending, maxPendingDuration, pendingPodMessage)

	for _, containerStatus := range monitorPod.Status.ContainerStatuses {
		if containerStatus.State.Waiting == nil {
			continue
		}
		if len(containerStatus.State.Waiting.Reason) > 0 {
			startupPodDegraded.Reason = containerStatus.State.Waiting.Reason
		}
		if len(containerStatus.State.Waiting.Message) > 0 {
			waitingContainerMessage := fmt.Sprintf("at least one container %s is waiting since %v due to %s", containerStatus.Name, monitorPod.Status.StartTime, containerStatus.State.Waiting.Message)
			if pendingPodMessage == "unknown" {
				startupPodDegraded.Message = waitingContainerMessage
			} else {
				startupPodDegraded.Message = fmt.Sprintf("%s\n%s", startupPodDegraded.Message, waitingContainerMessage)
			}
		}
		return
	}
}

func withDegradedWhenHasFailedContainer(monitorPod *corev1.Pod, startupPodDegraded *operatorv1.OperatorCondition) {
	if monitorPod.Status.Phase != corev1.PodFailed {
		return
	}
	startupPodDegraded.Status = operatorv1.ConditionTrue
	startupPodDegraded.Reason = "Unknown"
	startupPodDegraded.Message = "unknown"

	for _, containerStatus := range monitorPod.Status.ContainerStatuses {
		if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
			failedContainerReason, failedContainerMessage := startupPodDegraded.Reason, startupPodDegraded.Message
			if len(containerStatus.State.Terminated.Reason) > 0 {
				failedContainerReason = containerStatus.State.Terminated.Reason
			}
			if len(containerStatus.State.Terminated.Message) > 0 {
				failedContainerMessage = containerStatus.State.Terminated.Message
			}
			startupPodDegraded.Reason = failedContainerReason
			startupPodDegraded.Message = fmt.Sprintf("at least one container %s in %s pod exited with %d (expected nonzero exit code), due to %v", containerStatus.Name, monitorPod.Name, containerStatus.State.Terminated.ExitCode, failedContainerMessage)
			return
		}
	}
}

func withDegradedWhenHasExcessiveRestarts(monitorPod *corev1.Pod, maxRestartCount int, startupPodContainerExcessiveRestartsDegraded *operatorv1.OperatorCondition) {
	for _, containerStatus := range monitorPod.Status.ContainerStatuses {
		if int(containerStatus.RestartCount) >= maxRestartCount {
			startupPodContainerExcessiveRestartsDegraded.Status = operatorv1.ConditionTrue
			startupPodContainerExcessiveRestartsDegraded.Reason = "ExcessiveRestarts"
			startupPodContainerExcessiveRestartsDegraded.Message = fmt.Sprintf("at least one container %s in %s pod has restarted %d times, max allowed is %d", containerStatus.Name, monitorPod.Name, containerStatus.RestartCount, maxRestartCount)
			return
		}
	}
}
