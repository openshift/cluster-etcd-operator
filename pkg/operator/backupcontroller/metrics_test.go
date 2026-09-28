package backupcontroller

import (
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-base/metrics"
)

func TestMetricsRegistration(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	if m.info == nil {
		t.Error("info metric not initialized")
	}
	if m.status == nil {
		t.Error("status metric not initialized")
	}
	if m.completionTime == nil {
		t.Error("completionTime metric not initialized")
	}
	if m.startTime == nil {
		t.Error("startTime metric not initialized")
	}
	if m.sizeBytes == nil {
		t.Error("sizeBytes metric not initialized")
	}
}

func TestRecordBackup_NeverRun(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "never-started",
			UID:  types.UID("never-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state, tracked := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if !tracked {
		t.Error("expected backup to be tracked")
	}

	if state.currentStatus != statusPending {
		t.Errorf("expected status %s for never-run backup, got %s", statusPending, state.currentStatus)
	}
}

func TestRecordBackup_PVCStorage_Success(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	now := v1.NewTime(time.Now())
	later := v1.NewTime(now.Add(5 * time.Minute))

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "pvc-backup",
			UID:  types.UID("pvc-uid-123"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "backup-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupPending),
					Status:             v1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               string(operatorv1alpha1.BackupCompleted),
					Status:             v1.ConditionTrue,
					LastTransitionTime: later,
				},
			},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if state.storageType != storageTypePVC {
		t.Errorf("expected storage type %s, got %s", storageTypePVC, state.storageType)
	}

	if state.storageLocation != "backup-pvc" {
		t.Errorf("expected storage location backup-pvc, got %s", state.storageLocation)
	}

	if state.currentStatus != statusCompleted {
		t.Errorf("expected status %s, got %s", statusCompleted, state.currentStatus)
	}
}

func TestRecordBackup_PVCStorage_Failure(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	now := v1.NewTime(time.Now())
	later := v1.NewTime(now.Add(2 * time.Minute))

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "failed-backup",
			UID:  types.UID("failed-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "backup-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupPending),
					Status:             v1.ConditionTrue,
					LastTransitionTime: now,
				},
				{
					Type:               string(operatorv1alpha1.BackupFailed),
					Status:             v1.ConditionTrue,
					LastTransitionTime: later,
				},
			},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if state.currentStatus != statusFailed {
		t.Errorf("expected status %s, got %s", statusFailed, state.currentStatus)
	}
}

func TestRecordBackup_PVCWithPath(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "pvc-with-path",
			UID:  types.UID("pvc-path-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "my-pvc",
					Path: "/backups/2024",
				},
			},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	expected := "my-pvc:/backups/2024"
	if state.storageLocation != expected {
		t.Errorf("expected storage location %s, got %s", expected, state.storageLocation)
	}
}

func TestRecordBackup_LocalStorage(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "local-backup",
			UID:  types.UID("local-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
				Local: &operatorv1alpha1.EtcdBackupStorageLocal{
					HostPath: "/var/lib/etcd-backup",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupCompleted),
					Status:             v1.ConditionTrue,
					LastTransitionTime: v1.NewTime(time.Now()),
				},
			},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if state.storageType != storageTypeLocal {
		t.Errorf("expected storage type %s, got %s", storageTypeLocal, state.storageType)
	}

	if state.storageLocation != "/var/lib/etcd-backup" {
		t.Errorf("expected storage location /var/lib/etcd-backup, got %s", state.storageLocation)
	}
}

func TestRecordBackup_CreatedByPolicy(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "scheduled-backup",
			UID:  types.UID("scheduled-uid"),
			OwnerReferences: []v1.OwnerReference{
				{
					APIVersion: "config.openshift.io/v1alpha1",
					Kind:       "Backup",
					Name:       "daily-policy",
					UID:        types.UID("policy-uid"),
				},
			},
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if state.policyName != "daily-policy" {
		t.Errorf("expected policy name daily-policy, got %s", state.policyName)
	}
}

func TestRecordBackup_ConcurrentCompletions(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func(id int) {
			backup := operatorv1alpha1.EtcdBackup{
				ObjectMeta: v1.ObjectMeta{
					Name: string(rune('a' + id)),
					UID:  types.UID(string(rune('0' + id))),
				},
				Spec: operatorv1alpha1.EtcdBackupSpec{
					Storage: operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
						PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
							Name: "pvc",
						},
					},
				},
				Status: operatorv1alpha1.EtcdBackupStatus{
					Conditions: []v1.Condition{
						{
							Type:               string(operatorv1alpha1.BackupCompleted),
							Status:             v1.ConditionTrue,
							LastTransitionTime: v1.NewTime(time.Now()),
						},
					},
				},
			}
			m.recordBackup(backup)
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	m.mu.RLock()
	count := len(m.trackedBackups)
	m.mu.RUnlock()

	if count != 10 {
		t.Errorf("expected 10 backups after concurrent updates, got %d", count)
	}
}

func TestRecordBackup_RepeatedReconciliation(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	now := v1.NewTime(time.Now())
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "repeated-backup",
			UID:  types.UID("repeated-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupCompleted),
					Status:             v1.ConditionTrue,
					LastTransitionTime: now,
				},
			},
		},
	}

	m.recordBackup(backup)
	m.recordBackup(backup)
	m.recordBackup(backup)

	m.mu.RLock()
	count := len(m.trackedBackups)
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if count != 1 {
		t.Errorf("expected 1 tracked backup after repeated calls, got %d", count)
	}

	if state.currentStatus != statusCompleted {
		t.Errorf("expected status %s, got %s", statusCompleted, state.currentStatus)
	}
}

func TestDeleteBackup(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "delete-me",
			UID:  types.UID("delete-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	_, tracked := m.trackedBackups[backup.UID]
	m.mu.RUnlock()
	if !tracked {
		t.Error("expected backup to be tracked before deletion")
	}

	m.deleteBackup(backup)

	m.mu.RLock()
	_, tracked = m.trackedBackups[backup.UID]
	m.mu.RUnlock()
	if tracked {
		t.Error("expected backup to not be tracked after deletion")
	}
}

func TestDeleteBackup_NotTracked(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "never-tracked",
			UID:  types.UID("never-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
	}

	m.deleteBackup(backup)

	m.mu.RLock()
	_, tracked := m.trackedBackups[backup.UID]
	m.mu.RUnlock()
	if tracked {
		t.Error("deleting non-tracked backup should be no-op")
	}
}

func TestSizeBytes_NoFiles(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "test",
			UID:  types.UID("test-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Files: []operatorv1alpha1.EtcdBackupFile{},
		},
	}

	size := getSizeBytes(backup)
	if size != 0 {
		t.Errorf("expected size 0 when no files present, got %f", size)
	}
}

func TestSizeBytes_MultipleFiles(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "multi-file-backup",
			UID:  types.UID("multi-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Files: []operatorv1alpha1.EtcdBackupFile{
				{
					Path: "/backup/snapshot.db",
					Size: *resource.NewQuantity(1024*1024*100, resource.BinarySI), // 100 MiB
				},
				{
					Path: "/backup/static-pod-resources.tar.gz",
					Size: *resource.NewQuantity(1024*1024*50, resource.BinarySI), // 50 MiB
				},
			},
		},
	}

	size := getSizeBytes(backup)
	expected := float64(1024 * 1024 * 150) // 150 MiB
	if size != expected {
		t.Errorf("expected total size %f, got %f", expected, size)
	}
}

func TestNode_FromStatus(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "pvc-backup",
			UID:  types.UID("pvc-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			NodeName: "master-2",
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if state.node != "master-2" {
		t.Errorf("expected node master-2 from status, got %s", state.node)
	}
}

func TestNode_Empty_WhenStatusMissing(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "unbound-backup",
			UID:  types.UID("unbound-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
				Local: &operatorv1alpha1.EtcdBackupStorageLocal{
					HostPath: "/var/lib/etcd-backup",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			// No NodeName - backup not yet bound to a node
		},
	}

	m.recordBackup(backup)

	m.mu.RLock()
	state := m.trackedBackups[backup.UID]
	m.mu.RUnlock()

	if state.node != "" {
		t.Errorf("expected empty node when status.nodeName not set, got %s", state.node)
	}
}

func TestMetricNamesMatchConstants(t *testing.T) {
	expected := map[string]string{
		backupInfoMetricName:           "etcd_backup_info",
		backupStatusMetricName:         "etcd_backup_status",
		backupCompletionTimeMetricName: "etcd_backup_completion_time",
		backupStartTimeMetricName:      "etcd_backup_start_time",
		backupSizeBytesMetricName:      "etcd_backup_size_bytes",
	}

	for constant, want := range expected {
		if constant != want {
			t.Errorf("metric name mismatch: constant value is %q, expected %q", constant, want)
		}
	}
}

func TestMetricCollection(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	m := NewBackupMetrics(registry)

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-backup",
			UID:  types.UID("test-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "test-pvc",
				},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupPending),
					Status:             v1.ConditionTrue,
					LastTransitionTime: v1.NewTime(time.Now()),
				},
			},
		},
	}

	m.recordBackup(backup)

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	if len(metricFamilies) == 0 {
		t.Error("expected metrics to be collected, got 0")
	}

	found := false
	for _, mf := range metricFamilies {
		if mf.GetName() == backupInfoMetricName {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("expected to find %s metric", backupInfoMetricName)
	}
}
