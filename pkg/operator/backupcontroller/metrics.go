package backupcontroller

import (
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
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
	statusRunning   = "Running"
	statusCompleted = "Completed"
	statusFailed    = "Failed"
)

// backupCollector implements prometheus.Collector and generates metrics
// on-demand at scrape time by reading current EtcdBackup objects from the lister.
type backupCollector struct {
	backupsLister operatorv1alpha1listers.EtcdBackupLister

	infoDesc           *prometheus.Desc
	statusDesc         *prometheus.Desc
	completionTimeDesc *prometheus.Desc
	startTimeDesc      *prometheus.Desc
	sizeBytesDesc      *prometheus.Desc
}

// NewBackupMetrics creates and registers a backup collector with the provided registry.
// The collector dynamically generates metrics at scrape time from the EtcdBackupLister.
func NewBackupMetrics(metricsRegistry metrics.KubeRegistry, backupsLister operatorv1alpha1listers.EtcdBackupLister) *backupCollector {
	c := &backupCollector{
		backupsLister: backupsLister,
		infoDesc: prometheus.NewDesc(
			backupInfoMetricName,
			"Information about etcd backup",
			[]string{"etcd_backup", "uid", "node", "storage_type", "storage_location", "created_by_policy"},
			nil,
		),
		statusDesc: prometheus.NewDesc(
			backupStatusMetricName,
			"The current status of the backup. Value of 1 indicates the labeled status is active.",
			[]string{"etcd_backup", "uid", "status"},
			nil,
		),
		completionTimeDesc: prometheus.NewDesc(
			backupCompletionTimeMetricName,
			"Unix timestamp when the backup completed.",
			[]string{"etcd_backup", "uid"},
			nil,
		),
		startTimeDesc: prometheus.NewDesc(
			backupStartTimeMetricName,
			"Unix timestamp when the backup started.",
			[]string{"etcd_backup", "uid"},
			nil,
		),
		sizeBytesDesc: prometheus.NewDesc(
			backupSizeBytesMetricName,
			"The file size of the backup snapshot.",
			[]string{"etcd_backup", "uid"},
			nil,
		),
	}

	metricsRegistry.RawMustRegister(c)
	return c
}

// Describe implements prometheus.Collector
func (c *backupCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.infoDesc
	ch <- c.statusDesc
	ch <- c.completionTimeDesc
	ch <- c.startTimeDesc
	ch <- c.sizeBytesDesc
}

// Collect implements prometheus.Collector
func (c *backupCollector) Collect(ch chan<- prometheus.Metric) {
	backups, err := c.backupsLister.List(nil)
	if err != nil {
		klog.Errorf("backupCollector failed to list backups: %v", err)
		return
	}

	for _, backup := range backups {
		c.collectBackupMetrics(ch, *backup)
	}
}

func (c *backupCollector) collectBackupMetrics(ch chan<- prometheus.Metric, backup operatorv1alpha1.EtcdBackup) {
	name := backup.Name
	uid := string(backup.UID)
	node := backup.Status.NodeName
	storageType, storageLocation := extractStorageInfo(backup)
	policyName := extractPolicyName(backup)
	currentStatus := extractCurrentStatus(backup)

	// Emit info metric
	ch <- prometheus.MustNewConstMetric(
		c.infoDesc,
		prometheus.GaugeValue,
		1,
		name, uid, node, storageType, storageLocation, policyName,
	)

	// Emit status metric (only the active one)
	if currentStatus != "" {
		ch <- prometheus.MustNewConstMetric(
			c.statusDesc,
			prometheus.GaugeValue,
			1,
			name, uid, currentStatus,
		)
	}

	// Emit start time if available
	if startTime := getStartTime(backup); startTime > 0 {
		ch <- prometheus.MustNewConstMetric(
			c.startTimeDesc,
			prometheus.GaugeValue,
			startTime,
			name, uid,
		)
	}

	// Emit completion time if available
	if completionTime := getCompletionTime(backup); completionTime > 0 {
		ch <- prometheus.MustNewConstMetric(
			c.completionTimeDesc,
			prometheus.GaugeValue,
			completionTime,
			name, uid,
		)
	}

	// Emit size if available
	if sizeBytes := getSizeBytes(backup); sizeBytes > 0 {
		ch <- prometheus.MustNewConstMetric(
			c.sizeBytesDesc,
			prometheus.GaugeValue,
			sizeBytes,
			name, uid,
		)
	}
}

func extractStorageInfo(backup operatorv1alpha1.EtcdBackup) (string, string) {
	var storageType, storageLocation string = "", ""

	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		storageType = storageTypePVC
		if backup.Spec.Storage.PVC != nil {
			storageLocation = backup.Spec.Storage.PVC.Name
			if backup.Spec.Storage.PVC.Path != "" {
				storageLocation = backup.Spec.Storage.PVC.Name + ":" + backup.Spec.Storage.PVC.Path
			}
		}
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		storageType = storageTypeLocal
		if backup.Spec.Storage.Local != nil {
			storageLocation = backup.Spec.Storage.Local.HostPath
		}
	}

	return storageType, storageLocation
}

func extractPolicyName(backup operatorv1alpha1.EtcdBackup) string {
	for _, owner := range backup.OwnerReferences {
		if owner.Kind == "Backup" {
			return owner.Name
		}
	}
	return ""
}

func extractCurrentStatus(backup operatorv1alpha1.EtcdBackup) string {
	for _, condition := range backup.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case string(operatorv1alpha1.BackupPending):
			return statusPending
		case string(operatorv1alpha1.BackupRunning):
			return statusRunning
		case string(operatorv1alpha1.BackupCompleted):
			return statusCompleted
		case string(operatorv1alpha1.BackupFailed):
			return statusFailed
		}
	}
	return ""
}

func getStartTime(backup operatorv1alpha1.EtcdBackup) float64 {
	for _, condition := range backup.Status.Conditions {
		if condition.Type == string(operatorv1alpha1.BackupRunning) {
			return float64(condition.LastTransitionTime.Unix())
		}
	}
	return 0
}

func getCompletionTime(backup operatorv1alpha1.EtcdBackup) float64 {
	for _, condition := range backup.Status.Conditions {
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

// MustRegisterDefaultBackupMetrics creates backup metrics using the default registry.
// Deprecated: Use NewBackupMetrics with an explicit registry for production code.
// This function remains for test compatibility.
func MustRegisterDefaultBackupMetrics(backupsLister operatorv1alpha1listers.EtcdBackupLister) *backupCollector {
	return NewBackupMetrics(legacyregistry.DefaultGatherer.(metrics.KubeRegistry), backupsLister)
}
