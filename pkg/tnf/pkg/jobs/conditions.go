package jobs

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// ReadyForEtcdContainerRemovalCondition signals that TNF setup has completed
	// and CEO should remove static etcd containers and enable external etcd support
	ReadyForEtcdContainerRemovalCondition = "ReadyForEtcdContainerRemoval"
)

// IsTNFReadyForEtcdContainerRemoval checks if the TNF setup job is ready for etcd container removal.
func IsTNFReadyForEtcdContainerRemoval(ctx context.Context, kubeClient kubernetes.Interface) (ready bool, err error) {
	var job *batchv1.Job
	job, err = getSetupJob(ctx, kubeClient)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	return isJobConditionTrue(job, ReadyForEtcdContainerRemovalCondition), nil
}

// SetTNFReadyForEtcdContainerRemoval sets the ReadyForEtcdContainerRemoval condition on the TNF setup job.
// This signals CEO that TNF setup is ready for etcd container removal.
func SetTNFReadyForEtcdContainerRemoval(ctx context.Context, kubeClient kubernetes.Interface) error {
	job, err := getSetupJob(ctx, kubeClient)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s/%s not found", operatorclient.TargetNamespace, tools.JobTypeSetup.GetNameLabelValue())
	}
	return setJobCondition(ctx, kubeClient, job, ReadyForEtcdContainerRemovalCondition, corev1.ConditionTrue, "TNFSetupReadyForCEOEtcdRemoval", "TNF setup job is ready for CEO etcd container removal")
}

func getSetupJob(ctx context.Context, kubeClient kubernetes.Interface) (*batchv1.Job, error) {
	job, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, tools.JobTypeSetup.GetNameLabelValue(), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		klog.Infof("job %s/%s not found", operatorclient.TargetNamespace, tools.JobTypeSetup.GetNameLabelValue())
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get job %s/%s: %w", operatorclient.TargetNamespace, tools.JobTypeSetup.GetNameLabelValue(), err)
	}
	return job, nil
}

// setJobCondition sets a custom condition on a TNF job
func setJobCondition(ctx context.Context, kubeClient kubernetes.Interface, job *batchv1.Job, conditionType string, status corev1.ConditionStatus, reason, message string) error {
	// Create the new condition
	newCondition := batchv1.JobCondition{
		Type:               batchv1.JobConditionType(conditionType),
		Status:             status,
		LastProbeTime:      metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	// Check if condition already exists
	conditionExists := false
	for i, condition := range job.Status.Conditions {
		if string(condition.Type) == conditionType {
			job.Status.Conditions[i] = newCondition
			conditionExists = true
			break
		}
	}

	if !conditionExists {
		job.Status.Conditions = append(job.Status.Conditions, newCondition)
	}

	// Update the job status
	_, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update job status for %s/%s: %w", job.GetNamespace(), job.GetName(), err)
	}

	klog.Infof("Set TNF job condition %s to True for job %s/%s", conditionType, job.GetNamespace(), job.GetName())
	return nil
}

// isJobConditionTrue checks if a custom condition is set to True on a job
func isJobConditionTrue(job *batchv1.Job, conditionType string) bool {
	if job.Status.Conditions == nil {
		return false
	}

	for _, condition := range job.Status.Conditions {
		if string(condition.Type) == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}
