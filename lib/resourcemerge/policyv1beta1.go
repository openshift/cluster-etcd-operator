package resourcemerge

import (
	"k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// EnsurePodDisruptionBudgets ensures that the existing matches the required.
// modified is set to true when existing had to be updated with required.
func EnsurePodDisruptionBudgets(modified *bool, existing *v1beta1.PodDisruptionBudget, required v1beta1.PodDisruptionBudget) {
	EnsureObjectMeta(modified, &existing.ObjectMeta, required.ObjectMeta)

	if !equality.Semantic.DeepEqual(existing.Spec, required.Spec) {
		*modified = true
		existing.Spec = required.Spec
	}

}
