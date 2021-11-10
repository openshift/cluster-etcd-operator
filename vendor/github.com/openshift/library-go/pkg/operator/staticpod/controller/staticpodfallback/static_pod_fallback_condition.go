package staticpodfallback

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/staticpod/startupmonitor/annotations"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// staticPodFallbackConditionController knows how to detect and report that a static pod was rolled back to a previous revision
type staticPodFallbackConditionController struct {
	operatorClient operatorv1helpers.OperatorClient

	podLabelSelector labels.Selector
	podLister        corev1listers.PodNamespaceLister

	startupMonitorEnabledFn func() (bool, error)
}

// New creates a controller that detects and report roll back of a static pod
func New(targetNamespace string,
	podLabelSelector labels.Selector,
	operatorClient operatorv1helpers.OperatorClient,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	startupMonitorEnabledFn func() (bool, error),
	eventRecorder events.Recorder) factory.Controller {
	fd := &staticPodFallbackConditionController{
		operatorClient:          operatorClient,
		podLabelSelector:        podLabelSelector,
		podLister:               kubeInformersForNamespaces.InformersFor(targetNamespace).Core().V1().Pods().Lister().Pods(targetNamespace),
		startupMonitorEnabledFn: startupMonitorEnabledFn,
	}
	return factory.New().WithSync(fd.sync).ResyncEvery(6*time.Minute).WithInformers(kubeInformersForNamespaces.InformersFor(targetNamespace).Core().V1().Pods().Informer()).ToController("StaticPodStateFallback", eventRecorder)
}

// sync sets/unsets a StaticPodFallbackRevisionDegraded condition if a pod that matches the given label selector is annotated with FallbackForRevision
func (fd *staticPodFallbackConditionController) sync(ctx context.Context, _ factory.SyncContext) (err error) {
	degradedCondition := operatorv1.OperatorCondition{Type: "StaticPodFallbackRevisionDegraded", Status: operatorv1.ConditionFalse}
	defer func() {
		if err == nil {
			if _, _, updateError := operatorv1helpers.UpdateStatus(ctx, fd.operatorClient, operatorv1helpers.UpdateConditionFn(degradedCondition)); updateError != nil {
				err = updateError
			}
		}
	}()

	// we rely on operators to provide
	// a condition for checking we are running on a single node cluster
	if enabled, err := fd.startupMonitorEnabledFn(); err != nil {
		return err
	} else if !enabled {
		return nil
	}

	kasPods, err := fd.podLister.List(fd.podLabelSelector)
	if err != nil {
		return err
	}

	var conditionReason string
	var conditionMessage string
	for _, kasPod := range kasPods {
		if fallbackFor, ok := kasPod.Annotations[annotations.FallbackForRevision]; ok {
			reason := "Unknown"
			message := "unknown"
			if s, ok := kasPod.Annotations[annotations.FallbackReason]; ok {
				reason = s
			}
			if s, ok := kasPod.Annotations[annotations.FallbackMessage]; ok {
				message = s
			}

			message = fmt.Sprintf("a static pod %v was rolled back to revision %v due to %v", kasPod.Name, fallbackFor, message)
			if len(conditionMessage) > 0 {
				conditionMessage = fmt.Sprintf("%s\n%s", conditionMessage, message)
			} else {
				conditionMessage = message
			}
			if len(conditionReason) == 0 {
				conditionReason = reason
			}
		}
	}

	if len(conditionReason) > 0 || len(conditionMessage) > 0 {
		degradedCondition.Message = conditionMessage
		degradedCondition.Reason = conditionReason
		degradedCondition.Status = operatorv1.ConditionTrue
	}

	return nil
}
