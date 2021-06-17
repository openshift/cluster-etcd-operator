package resourceapply

import (
	"context"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	policyclientv1 "k8s.io/client-go/kubernetes/typed/policy/v1"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
)

func ApplyPodDisruptionBudget(client policyclientv1.PodDisruptionBudgetsGetter, shouldDelete bool, recorder events.Recorder, required *policyv1.PodDisruptionBudget) (*policyv1.PodDisruptionBudget, bool, error) {
	existing, err := client.PodDisruptionBudgets(required.Namespace).Get(context.TODO(), required.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && !shouldDelete {
		actual, err := client.PodDisruptionBudgets(required.Namespace).Create(context.TODO(), required, metav1.CreateOptions{})
		reportCreateEvent(recorder, required, err)
		return actual, true, err
	} else if apierrors.IsNotFound(err) && shouldDelete {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if shouldDelete {
		err := client.PodDisruptionBudgets(required.Namespace).Delete(context.TODO(), existing.Name, metav1.DeleteOptions{})
		if err != nil {
			return nil, false, err
		}
		reportDeleteEvent(recorder, required, err)
		return nil, true, nil
	}
	modified := resourcemerge.BoolPtr(false)
	existingCopy := existing.DeepCopy()

	resourcemerge.EnsureObjectMeta(modified, &existingCopy.ObjectMeta, required.ObjectMeta)
	contentSame := equality.Semantic.DeepEqual(existingCopy.Spec, required.Spec)
	if contentSame && !*modified {
		return existingCopy, false, nil
	}

	existingCopy.Spec = required.Spec

	if klog.V(4).Enabled() {
		klog.Infof("PodDisruptionBudget %q changes: %v", required.Name, JSONPatchNoError(existing, existingCopy))
	}

	actual, err := client.PodDisruptionBudgets(required.Namespace).Update(context.TODO(), existingCopy, metav1.UpdateOptions{})
	reportUpdateEvent(recorder, required, err)
	return actual, true, err
}
