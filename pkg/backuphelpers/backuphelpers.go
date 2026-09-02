package backuphelpers

import (
	"fmt"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

const (
	AutomatedEtcdBackupFeatureGateName = "AutomatedEtcdBackup"

	LabelEtcdBackupPolicy   = "operator.openshift.io/etcd-backup-policy"
	AnnotationBackupStorage = "operator.openshift.io/etcd-backup-storage"
	AnnotationBackupGCRetry = "operator.openshift.io/etcd-backup-gc-retry"
	FinalizerEtcdBackup     = "operator.openshift.io/etcd-backup"

	masterNodeLabel = "node-role.kubernetes.io/master"
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

// TODO(bhperry): Ideally this would be aware of the health of etcd on nodes so it selects the nodes most likely to succeed
func SelectBackupNodes(nodeLister corev1listers.NodeLister, selector labels.Selector) ([]*corev1.Node, error) {
	if selector == nil {
		if req, err := labels.NewRequirement(masterNodeLabel, selection.Exists, nil); err == nil {
			selector = labels.NewSelector().Add(*req)
		} else {
			return nil, fmt.Errorf("invalid selector: %w", err)
		}
	}

	masterNodes, err := nodeLister.List(selector)
	if err != nil {
		return nil, fmt.Errorf("error listing nodes: %w", err)
	}
	return masterNodes, nil
}
