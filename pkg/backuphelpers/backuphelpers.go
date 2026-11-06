package backuphelpers

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

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

	LabelEtcdBackup          = "operator.openshift.io/etcd-backup"
	LabelEtcdBackupPolicy    = "operator.openshift.io/etcd-backup-policy"
	LabelEtcdBackupPolicyRun = "operator.openshift.io/etcd-backup-policy-run"

	AnnotationBackupStorage = "operator.openshift.io/etcd-backup-storage"

	FinalizerEtcdBackup = "operator.openshift.io/etcd-backup"

	ControlPlaneNodeLabelSelector = "node-role.kubernetes.io/control-plane"

	BackupPathMount = "/etc/kubernetes/cluster-backup"
)

type BackupTerminationLog struct {
	Message string                            `json:"message,omitempty"`
	Files   []operatorv1alpha1.EtcdBackupFile `json:"files"`
}

func AutoBackupFeatureGateEnabled(featureGateAccessor featuregates.FeatureGateAccess) (bool, error) {
	gates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates, error was: %w", err)
	}

	return gates.Enabled(AutomatedEtcdBackupFeatureGateName), nil
}

func IsBackupRunning(backup *operatorv1alpha1.EtcdBackup) bool {
	return v1helpers.IsConditionTrue(backup.Status.Conditions, string(operatorv1alpha1.BackupRunning))
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

func IsBackupPolicyScheduled(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) bool {
	for _, condition := range backupPolicy.Status.Conditions {
		if condition.Status == metav1.ConditionTrue && condition.Type == string(operatorv1alpha1.BackupPolicyScheduled) {
			return true
		}
	}
	return false
}

func EtcdBackupStorageFromPolicy(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) operatorv1alpha1.EtcdBackupStorage {
	return operatorv1alpha1.EtcdBackupStorage{
		Type:  backupPolicy.Spec.Storage.Type,
		PVC:   backupPolicy.Spec.Storage.PVC.EtcdBackupStoragePvc,
		Local: backupPolicy.Spec.Storage.Local.EtcdBackupStorageLocal,
	}
}

// LocalBackupHostPath returns the host directory where a Local backup is stored. The path is cleaned so that the
// backup job and the garbage collection job always resolve the same location, and defaults to BackupPathMount.
func LocalBackupHostPath(local operatorv1alpha1.EtcdBackupStorageLocal) string {
	if local.HostPath == "" {
		return BackupPathMount
	}
	return filepath.Clean(local.HostPath)
}

func ListBackupNodes(nodeLister corev1listers.NodeLister, selector labels.Selector) ([]*corev1.Node, error) {
	if selector == nil {
		if req, err := labels.NewRequirement(ControlPlaneNodeLabelSelector, selection.Exists, nil); err == nil {
			selector = labels.NewSelector().Add(*req)
		} else {
			return nil, fmt.Errorf("invalid selector: %w", err)
		}
	}

	controlPlaneNodes, err := nodeLister.List(selector)
	if err != nil {
		return nil, fmt.Errorf("error listing nodes: %w", err)
	}

	// Lister order is not stable, sort so node selection is deterministic
	slices.SortFunc(controlPlaneNodes, func(a, b *corev1.Node) int {
		return strings.Compare(a.Name, b.Name)
	})
	return controlPlaneNodes, nil
}

// FindPodTerminationMessage returns the most relevant termination message of the job's pods and whether
// the main container is known to have started.
func FindPodTerminationMessage(pods []*corev1.Pod) (message string, started bool) {
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded {
			// Prefer termination message of successful pod
			message, started = PodTerminationMessage(pod)
			break
		} else if podMessage, podStarted := PodTerminationMessage(pod); podStarted {
			// If no success message is found, the latest non-empty failure message will be returned instead
			started = true
			if podMessage != "" {
				message = podMessage
			}
		}
	}
	return message, started
}

func PodTerminationMessage(pod *corev1.Pod) (message string, started bool) {
	if len(pod.Status.ContainerStatuses) > 0 {
		if status := pod.Status.ContainerStatuses[0]; (status.Started != nil && *status.Started) || status.State.Terminated != nil {
			started = true
			if status.State.Terminated != nil {
				message = status.State.Terminated.Message
			}
		}
	}
	return message, started
}

func ParseTerminationMessage(message string) (BackupTerminationLog, error) {
	if message == "" {
		return BackupTerminationLog{}, fmt.Errorf("missing termination message")
	}
	data := BackupTerminationLog{}
	if err := json.Unmarshal([]byte(message), &data); err != nil {
		return BackupTerminationLog{}, fmt.Errorf("error reading termination message: %w", err)
	}

	for i, file := range data.Files {
		filePath, _ := strings.CutPrefix(file.Path, BackupPathMount)
		data.Files[i] = operatorv1alpha1.EtcdBackupFile{
			Path:      filePath,
			SizeBytes: file.SizeBytes,
		}
	}
	return data, nil
}
