package backupcontroller

import (
	"sync"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	backupInfoMetricName           = "etcd_backup_info"
	backupStatusMetricName         = "etcd_backup_status"
	backupCompletionTimeMetricName = "etcd_backup_completion_time"
	backupStartTimeMetricName      = "etcd_backup_start_time"
	backupSizeBytesMetricName      = "etcd_backup_size_bytes"

	storageTypePVC   = "PVC"
	storageTypeLocal = "Local"

	statusPending   = "Pending"
	statusCompleted = "Completed"
	statusFailed    = "Failed"
)

type backupMetrics struct {
	info           *metrics.GaugeVec
	status         *metrics.GaugeVec
	completionTime *metrics.GaugeVec
	startTime      *metrics.GaugeVec
	sizeBytes      *metrics.GaugeVec

	mu             sync.RWMutex
	trackedBackups map[types.UID]backupState
}

type backupState struct {
	name            string
	uid             string
	node            string
	storageType     string
	storageLocation string
	policyName      string
	currentStatus   string
}

func createBackupMetrics(metricsRegistry metrics.KubeRegistry) *backupMetrics {
	info := metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           backupInfoMetricName,
			Help:           "Information about etcd backup",
			StabilityLevel: metrics.STABLE,
		},
		[]string{"etcd_backup", "uid", "node", "storage_type", "storage_location", "created_by_policy"},
	)

	status := metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           backupStatusMetricName,
			Help:           "The current status of the backup. Value of 1 indicates the labeled status is active.",
			StabilityLevel: metrics.STABLE,
		},
		[]string{"etcd_backup", "uid", "status"},
	)

	completionTime := metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           backupCompletionTimeMetricName,
			Help:           "Unix timestamp when the backup completed.",
			StabilityLevel: metrics.STABLE,
		},
		[]string{"etcd_backup", "uid"},
	)

	startTime := metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           backupStartTimeMetricName,
			Help:           "Unix timestamp when the backup started.",
			StabilityLevel: metrics.STABLE,
		},
		[]string{"etcd_backup", "uid"},
	)

	sizeBytes := metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           backupSizeBytesMetricName,
			Help:           "The file size of the backup snapshot.",
			StabilityLevel: metrics.STABLE,
		},
		[]string{"etcd_backup", "uid"},
	)

	metricsRegistry.MustRegister(info)
	metricsRegistry.MustRegister(status)
	metricsRegistry.MustRegister(completionTime)
	metricsRegistry.MustRegister(startTime)
	metricsRegistry.MustRegister(sizeBytes)

	return &backupMetrics{
		info:           info,
		status:         status,
		completionTime: completionTime,
		startTime:      startTime,
		sizeBytes:      sizeBytes,
		trackedBackups: make(map[types.UID]backupState),
	}
}

func extractBackupState(backup operatorv1alpha1.EtcdBackup) backupState {
	state := backupState{
		name: backup.Name,
		uid:  string(backup.UID),
	}

	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		state.storageType = storageTypePVC
		if backup.Spec.Storage.PVC != nil {
			state.storageLocation = backup.Spec.Storage.PVC.Name
			if backup.Spec.Storage.PVC.Path != "" {
				state.storageLocation = backup.Spec.Storage.PVC.Name + ":" + backup.Spec.Storage.PVC.Path
			}
		}
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		state.storageType = storageTypeLocal
		if backup.Spec.Storage.Local != nil {
			state.storageLocation = backup.Spec.Storage.Local.HostPath
		}
	}

	if backup.Status.NodeName != "" {
		state.node = backup.Status.NodeName
	}

	for _, owner := range backup.OwnerReferences {
		if owner.Kind == "Backup" {
			state.policyName = owner.Name
			break
		}
	}

	state.currentStatus = statusPending
	for _, condition := range backup.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case string(operatorv1alpha1.BackupPending):
			state.currentStatus = statusPending
		case string(operatorv1alpha1.BackupCompleted):
			state.currentStatus = statusCompleted
		case string(operatorv1alpha1.BackupFailed):
			state.currentStatus = statusFailed
		}
	}

	return state
}

func getStartTime(backup operatorv1alpha1.EtcdBackup) float64 {
	for _, condition := range backup.Status.Conditions {
		if condition.Type == string(operatorv1alpha1.BackupPending) && condition.Status == "True" {
			return float64(condition.LastTransitionTime.Unix())
		}
	}
	return 0
}

func getCompletionTime(backup operatorv1alpha1.EtcdBackup) float64 {
	for _, condition := range backup.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		if condition.Type == string(operatorv1alpha1.BackupCompleted) ||
			condition.Type == string(operatorv1alpha1.BackupFailed) {
			return float64(condition.LastTransitionTime.Unix())
		}
	}
	return 0
}

func getSizeBytes(backup operatorv1alpha1.EtcdBackup) float64 {
	var total int64
	for _, file := range backup.Status.Files {
		total += file.Size.Value()
	}
	return float64(total)
}

func (m *backupMetrics) recordBackup(backup operatorv1alpha1.EtcdBackup) {
	state := extractBackupState(backup)

	m.mu.Lock()
	m.trackedBackups[backup.UID] = state
	m.mu.Unlock()

	m.info.WithLabelValues(
		state.name,
		state.uid,
		state.node,
		state.storageType,
		state.storageLocation,
		state.policyName,
	).Set(1)

	// Update status metric: set current status to 1, others to 0
	for _, s := range []string{statusPending, statusCompleted, statusFailed} {
		value := float64(0)
		if s == state.currentStatus {
			value = 1
		}
		m.status.WithLabelValues(state.name, state.uid, s).Set(value)
	}

	if startTime := getStartTime(backup); startTime > 0 {
		m.startTime.WithLabelValues(state.name, state.uid).Set(startTime)
	}

	if completionTime := getCompletionTime(backup); completionTime > 0 {
		m.completionTime.WithLabelValues(state.name, state.uid).Set(completionTime)
	}

	if sizeBytes := getSizeBytes(backup); sizeBytes > 0 {
		m.sizeBytes.WithLabelValues(state.name, state.uid).Set(sizeBytes)
	}
}

func (m *backupMetrics) deleteBackup(backup operatorv1alpha1.EtcdBackup) {
	m.mu.Lock()
	state, exists := m.trackedBackups[backup.UID]
	if !exists {
		m.mu.Unlock()
		return
	}
	delete(m.trackedBackups, backup.UID)
	m.mu.Unlock()

	m.info.DeleteLabelValues(
		state.name,
		state.uid,
		state.node,
		state.storageType,
		state.storageLocation,
		state.policyName,
	)

	for _, s := range []string{statusPending, statusCompleted, statusFailed} {
		m.status.DeleteLabelValues(state.name, state.uid, s)
	}

	m.startTime.DeleteLabelValues(state.name, state.uid)
	m.completionTime.DeleteLabelValues(state.name, state.uid)
	m.sizeBytes.DeleteLabelValues(state.name, state.uid)
}

func MustRegisterDefaultBackupMetrics() *backupMetrics {
	return createBackupMetrics(legacyregistry.DefaultGatherer.(metrics.KubeRegistry))
}
