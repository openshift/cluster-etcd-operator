package resourceapply

import (
	"context"
	"github.com/openshift/cluster-etcd-operator/lib/resourcemerge"
	"k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	policyv1 "k8s.io/client-go/kubernetes/typed/policy/v1"
	"k8s.io/utils/pointer"
)

// ApplyPodDisruptionBudgets applies the required deployment to the cluster.
func ApplyPodDisruptionBudgets(ctx context.Context, client policyv1.PodDisruptionBudgetsGetter, required *v1.PodDisruptionBudget) (*v1.PodDisruptionBudget, bool, error) {
	existing, err := client.PodDisruptionBudgets(required.Namespace).Get(ctx, required.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		actual, err := client.PodDisruptionBudgets(required.Namespace).Create(ctx, required, metav1.CreateOptions{})
		return actual, true, err
	}
	if err != nil {
		return nil, false, err
	}

	modified := pointer.BoolPtr(false)
	resourcemerge.EnsurePodDisruptionBudgets(modified, existing, *required)
	if !*modified {
		return existing, false, nil
	}

	actual, err := client.PodDisruptionBudgets(required.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return actual, true, err
}
