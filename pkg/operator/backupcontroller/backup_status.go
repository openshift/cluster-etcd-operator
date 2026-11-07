package backupcontroller

import (
	"context"
	"fmt"
	"slices"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	applyconfigurationsoperatorv1alpha1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applyconfigurationsmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
)

func applyBackupRunning(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
) (*operatorv1alpha1.EtcdBackup, error) {
	// Only Local backup jobs are pinned to a node
	var nodeName *string
	if job.Spec.Template.Spec.NodeName != "" {
		nodeName = &job.Spec.Template.Spec.NodeName
	}
	// Require the version of the backup that was observed as not finished, so that a stale cache can't replace a
	// finished status with running.
	return applyBackupStatusConditions(ctx, backupClient, backup, job, true, []metav1.Condition{
		{
			Type:               string(operatorv1alpha1.BackupRunning),
			Reason:             string(operatorv1alpha1.BackupReasonJobStarted),
			Message:            "backup job is running",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: job.CreationTimestamp,
		},
	}, nil, nodeName)
}

func applyBackupFinished(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
	condition operatorv1alpha1.BackupConditionType,
	reason operatorv1alpha1.BackupConditionReason,
	message string,
	files []operatorv1alpha1.EtcdBackupFile,
) (*operatorv1alpha1.EtcdBackup, error) {
	transitionTime := metav1.Now()
	switch condition {
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
		return nil, fmt.Errorf("invalid finished condition %q", condition)
	}

	// Finished is terminal and replaces any other conditions and files, so only apply it to the version of the backup
	// it was computed from. On conflict the backup is re-evaluated with its latest version.
	return applyBackupStatusConditions(ctx, backupClient, backup, job, true, []metav1.Condition{
		{
			Type:               string(condition),
			Reason:             string(reason),
			Message:            message,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: transitionTime,
		},
	}, files, nil)
}

// applyBackupStatusConditions applies the status conditions, files and node name of a backup. If
// requireResourceVersion is set, the apply fails with a conflict if the backup has changed since it was read.
func applyBackupStatusConditions(ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
	requireResourceVersion bool,
	conditions []metav1.Condition,
	files []operatorv1alpha1.EtcdBackupFile,
	nodeName *string,
) (*operatorv1alpha1.EtcdBackup, error) {
	return applyBackupStatus(ctx, backupClient, backup, requireResourceVersion, func(status *applyconfigurationsoperatorv1alpha1.EtcdBackupStatusApplyConfiguration) {
		if job != nil {
			status.JobName = &job.Name
		}
		if conditions != nil {
			status.Conditions = make([]applyconfigurationsmetav1.ConditionApplyConfiguration, len(conditions))
			for i, condition := range conditions {
				status.Conditions[i] = *applyconfigurationsmetav1.Condition().
					WithType(condition.Type).
					WithReason(condition.Reason).
					WithMessage(condition.Message).
					WithStatus(condition.Status).
					WithLastTransitionTime(condition.LastTransitionTime)
			}
		}
		if files != nil {
			status.Files = make([]applyconfigurationsoperatorv1alpha1.EtcdBackupFileApplyConfiguration, len(files))
			for i, file := range files {
				fileApply := applyconfigurationsoperatorv1alpha1.EtcdBackupFile().WithPath(file.Path)
				// sizeBytes must be at least 1 when set. Omitted means the file is empty (e.g. a partial file).
				if file.SizeBytes > 0 {
					fileApply.WithSizeBytes(file.SizeBytes)
				}
				status.Files[i] = *fileApply
			}
		}
		if nodeName != nil {
			status.NodeName = nodeName
		}
	})
}

func applyBackupStatus(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	requireResourceVersion bool,
	applyStatus func(status *applyconfigurationsoperatorv1alpha1.EtcdBackupStatusApplyConfiguration),
) (*operatorv1alpha1.EtcdBackup, error) {
	backupName := backup.Name
	backupApply, err := applyconfigurationsoperatorv1alpha1.ExtractEtcdBackupStatus(backup, backupStatusFieldManager)
	if err != nil {
		return nil, fmt.Errorf("error extracting backup %q status: %w", backupName, err)
	}
	if backupApply.Status == nil {
		backupApply.Status = applyconfigurationsoperatorv1alpha1.EtcdBackupStatus()
	}
	applyStatus(backupApply.Status)
	if requireResourceVersion && backup.ResourceVersion != "" {
		backupApply.WithResourceVersion(backup.ResourceVersion)
	}

	if backup, err = backupClient.ApplyStatus(ctx, backupApply, metav1.ApplyOptions{
		Force:        true,
		FieldManager: backupStatusFieldManager,
	}); err != nil {
		return nil, fmt.Errorf("error applying backup %q status: %w", backupName, err)
	}
	return backup, nil
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
