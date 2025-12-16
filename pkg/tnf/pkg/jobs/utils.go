package jobs

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// WaitForStopped waits for a Job to stop running (= completed or failed)
func WaitForStopped(ctx context.Context, kubeClient kubernetes.Interface, jobName string, jobNamespace string, timeout time.Duration) error {
	klog.Infof("Waiting for job %s to complete or fail (=not running anymore) (timeout: %v)", jobName, timeout)
	return waitWithConditionFunc(ctx, kubeClient, jobName, jobNamespace, timeout, IsStopped)
}

// WaitForCompletion waits for a Job to complete (= succeed)
func WaitForCompletion(ctx context.Context, kubeClient kubernetes.Interface, jobName string, jobNamespace string, timeout time.Duration) error {
	klog.Infof("Waiting for job %s to complete (=succeed) (timeout: %v)", jobName, timeout)
	return waitWithConditionFunc(ctx, kubeClient, jobName, jobNamespace, timeout, IsComplete)
}

// waitWithConditionFunc waits for a Job to fulfill given conditionFunc
func waitWithConditionFunc(ctx context.Context, kubeClient kubernetes.Interface, jobName string, jobNamespace string, timeout time.Duration, conditionFunc func(job batchv1.Job) bool) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := kubeClient.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, v1.GetOptions{})
		if err != nil {
			// Ignore errors (including NotFound) to avoid returning early. The job might be
			// temporarily missing during deletion/recreation cycles.
			klog.Warningf("Failed to get job %s: %v", jobName, err)
			return false, nil
		}

		// Check if job condition
		if conditionFunc(*job) {
			klog.Infof("Job %s condition fulfilled", jobName)
			return true, nil
		}

		return false, nil
	})
}

// DeleteAndWait deletes a job and waits until it disappears from the API
func DeleteAndWait(ctx context.Context, kubeClient kubernetes.Interface, jobName string, jobNamespace string) error {
	klog.V(4).Infof("deleting oldJob %s", jobName)
	oldJob, err := kubeClient.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, v1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("job %s didn't exist", jobName)
			return nil
		}
		return err
	}
	oldJobUID := oldJob.GetUID()

	err = kubeClient.BatchV1().Jobs(jobNamespace).Delete(ctx, jobName, v1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// wait until the oldJob is deleted
	klog.V(4).Infof("waiting for job %s to be deleted", jobName)
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 1*time.Minute, true, func(ctx context.Context) (bool, error) {
		newJob, err := kubeClient.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, v1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			klog.Warningf("error checking job %s deletion status: %v", jobName, err)
			return false, nil // retry on transient errors
		}
		// job might be recreated already, check UID
		return newJob.GetUID() != oldJobUID, nil
	})
}

func IsStopped(job batchv1.Job) bool {
	return IsComplete(job) || IsFailed(job)
}

func IsComplete(job batchv1.Job) bool {
	return IsConditionTrue(job.Status.Conditions, batchv1.JobComplete)
}

func IsFailed(job batchv1.Job) bool {
	return IsConditionTrue(job.Status.Conditions, batchv1.JobFailed)
}

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
