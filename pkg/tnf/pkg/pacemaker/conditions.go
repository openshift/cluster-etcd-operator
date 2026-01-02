package pacemaker

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionSpec defines the parameters for building a condition
type ConditionSpec struct {
	Type        string
	TrueReason  string
	FalseReason string
	TrueMsg     string
	FalseMsg    string
}

// buildCondition creates a metav1.Condition based on whether the condition is true or false.
// This eliminates repetitive condition builder functions by parameterizing the differences.
func buildCondition(spec ConditionSpec, isTrue bool, now metav1.Time) metav1.Condition {
	if isTrue {
		return metav1.Condition{
			Type:               spec.Type,
			Status:             metav1.ConditionTrue,
			Reason:             spec.TrueReason,
			Message:            spec.TrueMsg,
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               spec.Type,
		Status:             metav1.ConditionFalse,
		Reason:             spec.FalseReason,
		Message:            spec.FalseMsg,
		LastTransitionTime: now,
	}
}

// findCondition finds a condition by type from a list of conditions.
// Returns nil if the condition is not found.
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// getConditionStatus gets the status of a condition by type from a list of conditions.
// Returns metav1.ConditionUnknown if the condition is not found.
func getConditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	if cond := findCondition(conditions, conditionType); cond != nil {
		return cond.Status
	}
	return metav1.ConditionUnknown
}
