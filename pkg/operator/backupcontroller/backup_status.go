package backupcontroller

import (
	"context"
	"fmt"
	"slices"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
)

func updateBackupRunning(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
) (*operatorv1alpha1.EtcdBackup, error) {
	backup = backup.DeepCopy()
	if job != nil {
		backup.Status.JobName = job.Name
	}
	setBackupStatusCondition(backup, metav1.Condition{
		Type:               string(operatorv1alpha1.BackupRunning),
		Reason:             string(operatorv1alpha1.BackupReasonJobStarted),
		Message:            "backup job is running",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: job.CreationTimestamp,
	})
	return backupClient.UpdateStatus(ctx, backup, metav1.UpdateOptions{})
}

func updateBackupFinished(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
	conditionType operatorv1alpha1.BackupConditionType,
	reason operatorv1alpha1.BackupConditionReason,
	message string,
	files []operatorv1alpha1.EtcdBackupFile,
) (*operatorv1alpha1.EtcdBackup, error) {
	transitionTime := metav1.Now()
	switch conditionType {
	case operatorv1alpha1.BackupCompleted:
		if message == "" {
			message = "backup job completed"
		}
		if job != nil && job.Status.CompletionTime != nil {
			transitionTime = *job.Status.CompletionTime
		}
	case operatorv1alpha1.BackupFailed:
		if message == "" {
			message = "backup job failed"
		}
		if job != nil {
			if c := jobFinishedCondition(job); c != nil && c.Type == batchv1.JobFailed {
				transitionTime = c.LastTransitionTime
			}
		}
	default:
		return nil, fmt.Errorf("invalid finished condition %q", conditionType)
	}

	backup = backup.DeepCopy()
	if job != nil {
		backup.Status.JobName = job.Name
	}
	backup.Status.Files = files
	setBackupStatusCondition(backup, metav1.Condition{
		Type:               string(conditionType),
		Reason:             string(reason),
		Message:            message,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: transitionTime,
	})
	return backupClient.UpdateStatus(ctx, backup, metav1.UpdateOptions{})
}

func addBackupFinalizer(ctx context.Context, backupClient operatorv1alpha1client.EtcdBackupInterface, backup *operatorv1alpha1.EtcdBackup) (*operatorv1alpha1.EtcdBackup, error) {
	if slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup) {
		return backup, nil
	}
	backup = backup.DeepCopy()
	backup.Finalizers = append(backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
	updated, err := backupClient.Update(ctx, backup, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error adding backup %q finalizer: %w", backup.Name, err)
	}
	return updated, nil
}

func removeBackupFinalizer(ctx context.Context, backupClient operatorv1alpha1client.EtcdBackupInterface, backup *operatorv1alpha1.EtcdBackup) error {
	idx := slices.Index(backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
	if idx < 0 {
		return nil
	}

	backup = backup.DeepCopy()
	backup.Finalizers = slices.Delete(backup.Finalizers, idx, idx+1)
	_, err := backupClient.Update(ctx, backup, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("error removing backup %q finalizer: %w", backup.Name, err)
	}
	return nil
}

func setBackupStatusCondition(backup *operatorv1alpha1.EtcdBackup, condition metav1.Condition) {
	backup.Status.Conditions = slices.DeleteFunc(backup.Status.Conditions, func(c metav1.Condition) bool {
		return c.Type == string(operatorv1alpha1.BackupRunning) || c.Type == string(operatorv1alpha1.BackupCompleted) || c.Type == string(operatorv1alpha1.BackupFailed)
	})
	backup.Status.Conditions = append(backup.Status.Conditions, condition)
}

// backupOutcome is the finished status recorded on a backup from its job.
type backupOutcome struct {
	condition operatorv1alpha1.BackupConditionType
	reason    operatorv1alpha1.BackupConditionReason
	message   string
	files     []operatorv1alpha1.EtcdBackupFile
}

// determineBackupOutcome derives the finished status of a backup from its finished job and the job's pods. The reason
// is only downgraded from the generic JobFailed to SetupFailed when it is certain that no backup files were written,
// since SetupFailed skips garbage collection.
func determineBackupOutcome(backupName string, job *batchv1.Job, pods []*corev1.Pod) backupOutcome {
	outcome := backupOutcome{
		condition: operatorv1alpha1.BackupCompleted,
		reason:    operatorv1alpha1.BackupReasonJobCompleted,
	}
	if isJobFailed(job) {
		outcome.condition = operatorv1alpha1.BackupFailed
		outcome.reason = operatorv1alpha1.BackupReasonJobFailed
	}

	// Parse termination message from most recently completed/failed pod that self-reported its status.
	// In the event of retry, the backup command will delete any remaining previously created backup files in the directory.
	terminationMessage, started := backuphelpers.FindPodTerminationMessage(pods)
	if started {
		// Backup container started, expect to be able to parse termination message
		termLog, err := backuphelpers.ParseTerminationMessage(terminationMessage)
		if err != nil {
			klog.Infof("BackupController failed to read termination message for backup %q: %v", backupName, err)
		}
		outcome.message, outcome.files = termLog.Message, termLog.Files
		if len(outcome.files) == 0 && outcome.condition == operatorv1alpha1.BackupFailed {
			if startedPodsCreatedNoFiles(job, pods) {
				// Every attempt reported graceful failure, no GC is required
				outcome.reason = operatorv1alpha1.BackupReasonSetupFailed
				if outcome.message == "" {
					outcome.message = "backup job failed without creating files"
				}
			} else {
				// An earlier attempt may have written partial files. Keep the generic job reason so that
				// garbage collection is not skipped.
				klog.Infof("BackupController backup %q reported no files, but not every attempt confirmed it created no files", backupName)
			}
		}
	} else if outcome.condition != operatorv1alpha1.BackupFailed {
		// Job completed, but its pods are no longer observable. Keep the default completed message.
		klog.Infof("BackupController unable to read termination message of completed backup %q from job %q pods", backupName, job.Name)
	} else if podHistoryComplete(job, pods) {
		// Every pod of the job is terminal and none started the backup container, most likely the init
		// container failed or PVC was unable to mount. No files can have been written.
		if terminationMessage == "" && len(pods) == 0 {
			terminationMessage = "backup job failed without creating pods"
		}
		outcome.reason = operatorv1alpha1.BackupReasonSetupFailed
		outcome.message = terminationMessage
		if outcome.message == "" {
			outcome.message = "failed to verify storage"
		}
	} else {
		// Pod history is missing or stale, so the backup container may have run and written partial files.
		// Keep the generic job reason so that garbage collection is not skipped.
		klog.Infof("BackupController unable to determine execution state of backup %q from job %q pods", backupName, job.Name)
		outcome.message = "unable to determine backup job outcome"
	}
	return outcome
}

// podHistoryComplete reports whether every pod the finished job accounted for is observed in a terminal phase.
// Job pods are tracked with finalizers, so every pod that terminated is counted in the job status (or listed as
// uncounted) before it can be removed. A job that counted no pods therefore never ran one to termination.
func podHistoryComplete(job *batchv1.Job, pods []*corev1.Pod) bool {
	if job.Status.Active > 0 || ptr.Deref(job.Status.Terminating, 0) > 0 {
		// Job still accounts for non-terminal pods that must be observed first
		return false
	}
	expectedPods := int64(job.Status.Failed) + int64(job.Status.Succeeded)
	if uncounted := job.Status.UncountedTerminatedPods; uncounted != nil {
		expectedPods += int64(len(uncounted.Failed)) + int64(len(uncounted.Succeeded))
	}
	if int64(len(pods)) < expectedPods {
		// Some pods the job accounted for are not observable, either deleted or not yet in the cache
		return false
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			// The job is finished, so a non-terminal pod means the cached pod status is stale
			return false
		}
	}
	return true
}

// startedPodsCreatedNoFiles reports whether it is certain that no attempt of the job created backup files: the pod
// history is complete, and every pod that started the backup container reported, via its termination message, that
// it created no files. A pod that crashed without a parseable message may have written partial files.
func startedPodsCreatedNoFiles(job *batchv1.Job, pods []*corev1.Pod) bool {
	if !podHistoryComplete(job, pods) {
		return false
	}
	for _, pod := range pods {
		message, started := backuphelpers.PodTerminationMessage(pod)
		if !started {
			continue
		}
		termLog, err := backuphelpers.ParseTerminationMessage(message)
		if err != nil || len(termLog.Files) > 0 {
			return false
		}
	}
	return true
}
