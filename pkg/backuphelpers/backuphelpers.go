package backuphelpers

import (
	"fmt"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	AutomatedEtcdBackupFeatureGateName = "AutomatedEtcdBackup"

	LabelEtcdBackupPolicy   = "operator.openshift.io/etcd-backup-policy"
	AnnotationBackupStorage = "operator.openshift.io/etcd-backup-storage"
	AnnotationBackupGCRetry = "operator.openshift.io/etcd-backup-gc-retry"
	FinalizerEtcdBackup     = "operator.openshift.io/etcd-backup"
)

func AutoBackupFeatureGateEnabled(featureGateAccessor featuregates.FeatureGateAccess) (bool, error) {
	gates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates, error was: %w", err)
	}

	return gates.Enabled(AutomatedEtcdBackupFeatureGateName), nil
}

func IsBackupPending(backup *operatorv1alpha1.EtcdBackup) bool {
	return v1helpers.IsConditionTrue(backup.Status.Conditions, string(operatorv1alpha1.BackupPending))
}

func IsBackupCompleted(backup *operatorv1alpha1.EtcdBackup) bool {
	return v1helpers.IsConditionTrue(backup.Status.Conditions, string(operatorv1alpha1.BackupCompleted))
}

func IsBackupFailed(backup *operatorv1alpha1.EtcdBackup) bool {
	return v1helpers.IsConditionTrue(backup.Status.Conditions, string(operatorv1alpha1.BackupFailed))
}

func IsBackupFinished(backup *operatorv1alpha1.EtcdBackup) bool {
	for _, condition := range backup.Status.Conditions {
		if condition.Status == metav1.ConditionTrue &&
			(condition.Type == string(operatorv1alpha1.BackupCompleted) || condition.Type == string(operatorv1alpha1.BackupFailed)) {
			return true
		}
	}
	return false
}
