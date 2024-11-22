package startupmonitorcondition

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"
)

// startupMonitorPodConditionController monitors the startup monitor pod and reports unwanted state/condition/phase as a degraded condition
type startupMonitorPodConditionController struct {
	controllerInstanceName string
	operatorClient         operatorv1helpers.StaticPodOperatorClient

	targetName string
	podLister  corev1listers.PodNamespaceLister

	startupMonitorEnabledFn func() (bool, error)
}

// New returns a controller for monitoring the lifecycle of the startup monitor pod
func New(
	instanceName, targetNamespace, targetName string,
	operatorClient operatorv1helpers.StaticPodOperatorClient,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	startupMonitorEnabledFn func() (bool, error),
	eventRecorder events.Recorder) factory.Controller {
	fd := &startupMonitorPodConditionController{
		controllerInstanceName:  factory.ControllerInstanceName(instanceName, "StartupMonitorPodCondition"),
		operatorClient:          operatorClient,
		targetName:              targetName,
		podLister:               kubeInformersForNamespaces.InformersFor(targetNamespace).Core().V1().Pods().Lister().Pods(targetNamespace),
		startupMonitorEnabledFn: startupMonitorEnabledFn,
	}
	return factory.New().
		WithSync(fd.sync).
		WithControllerInstanceName(fd.controllerInstanceName).
		ResyncEvery(6*time.Minute).
		WithInformers(
			kubeInformersForNamespaces.InformersFor(targetNamespace).Core().V1().Pods().Informer(),
			operatorClient.Informer(),
		).
		ToController(
			fd.controllerInstanceName,
			eventRecorder,
		)
}

func (fd *startupMonitorPodConditionController) sync(ctx context.Context, _ factory.SyncContext) (err error) {
	startupPodDegraded := applyoperatorv1.OperatorCondition().WithType("StartupMonitorPodDegraded")
	startupPodContainerExcessiveRestartsDegraded := applyoperatorv1.OperatorCondition().WithType("StartupMonitorPodContainerExcessiveRestartsDegraded")
	status := applyoperatorv1.OperatorStatus()

	defer func() {
		if err == nil {
			status = status.WithConditions(startupPodDegraded, startupPodContainerExcessiveRestartsDegraded)
			if applyError := fd.operatorClient.ApplyOperatorStatus(ctx, fd.controllerInstanceName, status); applyError != nil {
				err = applyError
			}
		}
	}()

	// in practice we rely on operators to provide
	// a condition for checking we are running on a single node cluster
	if enabled, err := fd.startupMonitorEnabledFn(); err != nil {
		return err
	} else if !enabled {
		startupPodDegraded = startupPodDegraded.WithStatus(operatorv1.ConditionFalse)
		startupPodContainerExcessiveRestartsDegraded = startupPodContainerExcessiveRestartsDegraded.WithStatus(operatorv1.ConditionFalse)
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
		startupPodDegraded = startupPodDegraded.WithStatus(operatorv1.ConditionFalse)
		startupPodContainerExcessiveRestartsDegraded = startupPodContainerExcessiveRestartsDegraded.WithStatus(operatorv1.ConditionFalse)
		return nil
	}

	monitorPod, err := fd.podLister.Get(fmt.Sprintf("%s-startup-monitor-%s", fd.targetName, currentNodeName))
	if errors.IsNotFound(err) {
		startupPodDegraded = startupPodDegraded.WithStatus(operatorv1.ConditionFalse)
		startupPodContainerExcessiveRestartsDegraded = startupPodContainerExcessiveRestartsDegraded.WithStatus(operatorv1.ConditionFalse)
		return nil
	} else if err != nil {
		return err // unknown error
	}

	// by default, both conditions are in a non-degraded state
	startupPodDegraded = startupPodDegraded.WithStatus(operatorv1.ConditionFalse)
	startupPodContainerExcessiveRestartsDegraded = startupPodContainerExcessiveRestartsDegraded.WithStatus(operatorv1.ConditionFalse)

	// sets the degraded condition when the pod has been in pending for more than 2 minutes
	withDegradedWhenInPendingPhaseFor(monitorPod, 2*time.Minute, startupPodDegraded)

	// sets the degraded condition when the pod failed with non-zero exit code
	withDegradedWhenHasFailedContainer(monitorPod, startupPodDegraded)

	// set the degraded condition when the pod has been restarted at least twice
	withDegradedWhenHasExcessiveRestarts(monitorPod, 2, startupPodContainerExcessiveRestartsDegraded)

	return nil
}

func withDegradedWhenInPendingPhaseFor(monitorPod *corev1.Pod, maxPendingDuration time.Duration, startupPodDegraded *applyoperatorv1.OperatorConditionApplyConfiguration) {
	if monitorPod.Status.Phase != corev1.PodPending || monitorPod.Status.StartTime == nil || time.Now().Sub(monitorPod.Status.StartTime.Time) < maxPendingDuration {
		return
	}

	pendingPodReason, pendingPodMessage := "Unknown", "unknown"
	if len(monitorPod.Status.Reason) > 0 {
		pendingPodReason = monitorPod.Status.Reason
	}
	if len(monitorPod.Status.Message) > 0 {
		pendingPodMessage = monitorPod.Status.Message
	}
	startupPodDegraded = startupPodDegraded.WithStatus(operatorv1.ConditionTrue).
		WithReason(pendingPodReason).
		WithMessage(fmt.Sprintf("the pod %v has been in %v phase for more than max tolerated time (%v) due to %v", monitorPod.Name, corev1.PodPending, maxPendingDuration, pendingPodMessage))

	for _, containerStatus := range monitorPod.Status.ContainerStatuses {
		if containerStatus.State.Waiting == nil {
			continue
		}
		if len(containerStatus.State.Waiting.Reason) > 0 {
			startupPodDegraded = startupPodDegraded.WithReason(containerStatus.State.Waiting.Reason)
		}
		if len(containerStatus.State.Waiting.Message) > 0 {
			waitingContainerMessage := fmt.Sprintf("at least one container %s is waiting since %v due to %s", containerStatus.Name, monitorPod.Status.StartTime, containerStatus.State.Waiting.Message)
			if pendingPodMessage == "unknown" {
				startupPodDegraded = startupPodDegraded.WithMessage(waitingContainerMessage)
			} else {
				startupPodDegraded = startupPodDegraded.WithMessage(fmt.Sprintf("%s\n%s", ptr.Deref(startupPodDegraded.Message, ""), waitingContainerMessage))
			}
		}
		return
	}
}

func withDegradedWhenHasFailedContainer(monitorPod *corev1.Pod, startupPodDegraded *applyoperatorv1.OperatorConditionApplyConfiguration) {
	if monitorPod.Status.Phase != corev1.PodFailed {
		return
	}
	startupPodDegraded = startupPodDegraded.WithStatus(operatorv1.ConditionTrue).
		WithReason("Unknown").
		WithMessage("unknown")

	for _, containerStatus := range monitorPod.Status.ContainerStatuses {
		if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.ExitCode != 0 {
			if len(containerStatus.State.Terminated.Reason) > 0 {
				startupPodDegraded.WithReason(containerStatus.State.Terminated.Reason)

			}
			if len(containerStatus.State.Terminated.Message) > 0 {
				msg := fmt.Sprintf("at least one container %s in %s pod exited with %d (expected nonzero exit code), due to %v", containerStatus.Name, monitorPod.Name, containerStatus.State.Terminated.ExitCode, containerStatus.State.Terminated.Message)
				startupPodDegraded.WithMessage(msg)
			}
			return
		}
	}
}

func withDegradedWhenHasExcessiveRestarts(monitorPod *corev1.Pod, maxRestartCount int, startupPodContainerExcessiveRestartsDegraded *applyoperatorv1.OperatorConditionApplyConfiguration) {
	for _, containerStatus := range monitorPod.Status.ContainerStatuses {
		if int(containerStatus.RestartCount) >= maxRestartCount {
			startupPodContainerExcessiveRestartsDegraded = startupPodContainerExcessiveRestartsDegraded.WithStatus(operatorv1.ConditionTrue).
				WithReason("ExcessiveRestarts").
				WithMessage(fmt.Sprintf("at least one container %s in %s pod has restarted %d times, max allowed is %d", containerStatus.Name, monitorPod.Name, containerStatus.RestartCount, maxRestartCount))
			return
		}
	}
}
