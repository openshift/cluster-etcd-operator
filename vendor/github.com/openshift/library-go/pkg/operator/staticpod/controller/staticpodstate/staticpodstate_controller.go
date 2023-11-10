package staticpodstate

import (
	"context"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// StaticPodStateController is a controller that watches static pods and will produce a failing status if the
//// static pods start crashing for some reason.
type StaticPodStateController struct {
	targetNamespace string
	staticPodName   string
	operandName     string

	operatorClient  v1helpers.StaticPodOperatorClient
	configMapGetter corev1client.ConfigMapsGetter
	podsGetter      corev1client.PodsGetter
	versionRecorder status.VersionGetter
}

// NewStaticPodStateController creates a controller that watches static pods and will produce a failing status if the
// static pods start crashing for some reason.
func NewStaticPodStateController(
	targetNamespace, staticPodName, operandName string,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	operatorClient v1helpers.StaticPodOperatorClient,
	configMapGetter corev1client.ConfigMapsGetter,
	podsGetter corev1client.PodsGetter,
	versionRecorder status.VersionGetter,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &StaticPodStateController{
		targetNamespace: targetNamespace,
		staticPodName:   staticPodName,
		operandName:     operandName,
		operatorClient:  operatorClient,
		configMapGetter: configMapGetter,
		podsGetter:      podsGetter,
		versionRecorder: versionRecorder,
	}
	return factory.New().WithInformers(
		operatorClient.Informer(),
		kubeInformersForTargetNamespace.Core().V1().Pods().Informer(),
	).WithSync(c.sync).ResyncEvery(time.Minute).ToController("StaticPodStateController", eventRecorder)
}

func describeWaitingContainerState(waiting *v1.ContainerStateWaiting) string {
	if waiting == nil {
		return "unknown reason"
	}
	return fmt.Sprintf("%s: %s", waiting.Reason, waiting.Message)
}

func (c *StaticPodStateController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, originalOperatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	if !management.IsOperatorManaged(operatorSpec.ManagementState) {
		return nil
	}

	errs := []error{}
	failingErrorCount := 0
	images := sets.NewString()
	podsFound := false
	for _, node := range originalOperatorStatus.NodeStatuses {
		pod, err := c.podsGetter.Pods(c.targetNamespace).Get(ctx, mirrorPodNameForNode(c.staticPodName, node.NodeName), metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
				failingErrorCount++
			}
			continue
		}
		podsFound = true
		images.Insert(pod.Spec.Containers[0].Image)

		for i, containerStatus := range pod.Status.ContainerStatuses {
			switch {
			case containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Reason != "PodInitializing":
				// if container status is waiting, but not initializing pod, increase the failing error counter
				// this usually means the container is stuck on initializing network
				errs = append(errs, fmt.Errorf("pod/%s container %q is waiting: %s", pod.Name, containerStatus.Name, describeWaitingContainerState(containerStatus.State.Waiting)))
				failingErrorCount++
			case containerStatus.State.Running != nil:
				maxNormalStartupDuration := 30 * time.Second // assume 30s for containers without probes
				if i < len(pod.Spec.Containers) {            // should always happen
					spec := pod.Spec.Containers[i]
					if spec.LivenessProbe != nil {
						maxNormalStartupDuration = maxFailureDuration(spec.LivenessProbe)
					}
					grace := 10 * time.Second
					maxNormalStartupDuration = max(maxNormalStartupDuration, maxFailureDuration(spec.ReadinessProbe)) + maxFailureDuration(spec.StartupProbe) + grace
				}

				if !containerStatus.Ready && time.Now().After(containerStatus.State.Running.StartedAt.Add(maxNormalStartupDuration)) {
					// When container is not ready, we can't determine whether the operator is failing or not and every container will become not
					// ready when created, so do not blip the failing state for it.
					// We will still reflect the container not ready state in error conditions, but we don't set the operator as failed.
					errs = append(errs, fmt.Errorf("pod/%s container %q started at %s is still not ready", pod.Name, containerStatus.Name, containerStatus.State.Running.StartedAt.Time))
				}
			case containerStatus.State.Terminated != nil:
				// Containers can be terminated gracefully to trigger certificate reload, do not report these as failures.
				errs = append(errs, fmt.Errorf("pod/%s container %q is terminated: %s: %s", pod.Name, containerStatus.Name, containerStatus.State.Terminated.Reason,
					containerStatus.State.Terminated.Message))
				// Only in case when the termination was caused by error.
				if containerStatus.State.Terminated.ExitCode != 0 {
					failingErrorCount++
				}
			}
		}
	}

	switch {
	case len(images) == 0:
		// only generate an event if we found pods, but none has an image.
		if podsFound {
			syncCtx.Recorder().Warningf("MissingVersion", "no image found for operand pod")
		}

	case len(images) > 1:
		syncCtx.Recorder().Eventf("MultipleVersions", "multiple versions found, probably in transition: %v", strings.Join(images.List(), ","))

	default: // we have one image
		// if have a consistent image and if that image the same as the current operand image, then we can update the version to reflect our new version
		if images.List()[0] == status.ImageForOperandFromEnv() {
			c.versionRecorder.SetVersion(
				c.operandName,
				status.VersionForOperandFromEnv(),
			)
			c.versionRecorder.SetVersion(
				"operator",
				status.VersionForOperatorFromEnv(),
			)

		} else {
			// otherwise, we have one image, but it is NOT the current operand image so we don't update the version
		}
	}

	// update failing condition
	cond := operatorv1.OperatorCondition{
		Type:   condition.StaticPodsDegradedConditionType,
		Status: operatorv1.ConditionFalse,
	}
	// Failing errors
	if failingErrorCount > 0 {
		cond.Status = operatorv1.ConditionTrue
		cond.Reason = "Error"
		cond.Message = v1helpers.NewMultiLineAggregate(errs).Error()
	}
	// Not failing errors
	if failingErrorCount == 0 && len(errs) > 0 {
		cond.Reason = "Error"
		cond.Message = v1helpers.NewMultiLineAggregate(errs).Error()
	}
	if _, _, updateError := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(cond), v1helpers.UpdateStaticPodConditionFn(cond)); updateError != nil {
		return updateError
	}

	return err
}

func maxFailureDuration(p *v1.Probe) time.Duration {
	if p == nil {
		return 0
	}

	return time.Duration(p.InitialDelaySeconds)*time.Second + time.Duration(p.FailureThreshold*p.PeriodSeconds)*time.Second
}

func max(x, y time.Duration) time.Duration {
	if x > y {
		return x
	}
	return y
}

func mirrorPodNameForNode(staticPodName, nodeName string) string {
	return staticPodName + "-" + nodeName
}
