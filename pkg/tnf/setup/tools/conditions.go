package tools

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func IsConditionTrue(conditions []batchv1.JobCondition, conditionType batchv1.JobConditionType) bool {
	return IsConditionPresentAndEqual(conditions, conditionType, corev1.ConditionTrue)
}

func IsConditionPresentAndEqual(conditions []batchv1.JobCondition, conditionType batchv1.JobConditionType, status corev1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}
