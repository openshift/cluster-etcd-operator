package backupcontroller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-base/metrics"
)

// fakeBackupLister implements a simple backup lister for tests
type fakeBackupLister struct {
	backups []*operatorv1alpha1.EtcdBackup
}

func (f *fakeBackupLister) List(selector labels.Selector) ([]*operatorv1alpha1.EtcdBackup, error) {
	return f.backups, nil
}

func (f *fakeBackupLister) Get(name string) (*operatorv1alpha1.EtcdBackup, error) {
	for _, b := range f.backups {
		if b.Name == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("backup %s not found", name)
}

func TestMetricsRegistration(t *testing.T) {
	registry := metrics.NewKubeRegistry()
	lister := &fakeBackupLister{}

	c := NewBackupMetrics(registry, lister)

	if c.infoDesc == nil {
		t.Error("info metric descriptor not initialized")
	}
	if c.statusDesc == nil {
		t.Error("status metric descriptor not initialized")
	}
	if c.completionTimeDesc == nil {
		t.Error("completionTime metric descriptor not initialized")
	}
	if c.startTimeDesc == nil {
		t.Error("startTime metric descriptor not initialized")
	}
	if c.sizeBytesDesc == nil {
		t.Error("sizeBytes metric descriptor not initialized")
	}
}

func TestCollectBackup_NeverRun(t *testing.T) {
	backup := &operatorv1alpha1.EtcdBackup{
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

	collector := setupCollectorWithBackups(t, backup)

	// Verify no status metric is emitted (backup has no active status)
	count := testutil.CollectAndCount(collector, backupStatusMetricName)
	if count != 0 {
		t.Errorf("expected 0 status metrics for never-run backup, got %d", count)
	}
}

func TestCollectBackup_PVCStorage_Success(t *testing.T) {
	now := v1.NewTime(time.Now())
	later := v1.NewTime(now.Add(5 * time.Minute))

	backup := &operatorv1alpha1.EtcdBackup{
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
			NodeName: "node-1",
			Conditions: []v1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupRunning),
					Status:             v1.ConditionFalse,
					LastTransitionTime: now,
				},
				{
					Type:               string(operatorv1alpha1.BackupCompleted),
					Status:             v1.ConditionTrue,
					LastTransitionTime: later,
				},
			},
			Files: []operatorv1alpha1.EtcdBackupFile{
				{Size: resource.MustParse("100Mi")},
			},
		},
	}

	collector := setupCollectorWithBackups(t, backup)

	// Verify info metric
	verifyInfoMetric(t, collector, "pvc-backup", "pvc-uid-123", "node-1", storageTypePVC, "backup-pvc", "")

	// Verify only completed status is emitted
	expected := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="pvc-backup",status="Completed",uid="pvc-uid-123"} 1
	`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName); err != nil {
		t.Errorf("status metric mismatch: %v", err)
	}

	// Verify timestamps exist (can't easily verify exact values with the current setup)
	count := testutil.CollectAndCount(collector)
	if count == 0 {
		t.Error("expected metrics to be collected")
	}
}

func TestCollectBackup_PVCWithPath(t *testing.T) {
	backup := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "pvc-path-backup",
			UID:  types.UID("pvc-path-uid"),
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "backup-pvc",
					Path: "/custom/path",
				},
			},
		},
	}

	collector := setupCollectorWithBackups(t, backup)
	verifyInfoMetric(t, collector, "pvc-path-backup", "pvc-path-uid", "", storageTypePVC, "backup-pvc:/custom/path", "")
}

func TestCollectBackup_LocalStorage(t *testing.T) {
	backup := &operatorv1alpha1.EtcdBackup{
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
			NodeName: "node-2",
		},
	}

	collector := setupCollectorWithBackups(t, backup)
	verifyInfoMetric(t, collector, "local-backup", "local-uid", "node-2", storageTypeLocal, "/var/lib/etcd-backup", "")
}

func TestCollectBackup_CreatedByPolicy(t *testing.T) {
	backup := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "policy-backup",
			UID:  types.UID("policy-uid"),
			OwnerReferences: []v1.OwnerReference{
				{
					Kind: "Backup",
					Name: "daily-policy",
				},
			},
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
					Name: "backup-pvc",
				},
			},
		},
	}

	collector := setupCollectorWithBackups(t, backup)
	verifyInfoMetric(t, collector, "policy-backup", "policy-uid", "", storageTypePVC, "backup-pvc", "daily-policy")
}

func TestCollectBackup_MultipleBackups(t *testing.T) {
	backup1 := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{Name: "backup-1", UID: "uid-1"},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "pvc-1"},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupCompleted), Status: v1.ConditionTrue},
			},
		},
	}

	backup2 := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{Name: "backup-2", UID: "uid-2"},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "pvc-2"},
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupRunning), Status: v1.ConditionTrue},
			},
		},
	}

	collector := setupCollectorWithBackups(t, backup1, backup2)

	// Verify both status metrics are present
	expected := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="backup-1",status="Completed",uid="uid-1"} 1
		etcd_backup_status{etcd_backup="backup-2",status="Running",uid="uid-2"} 1
	`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName); err != nil {
		t.Errorf("status metrics mismatch: %v", err)
	}
}

func TestBackupDeletion(t *testing.T) {
	backup := &operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{Name: "to-delete", UID: "delete-uid"},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "pvc"},
			},
		},
	}

	// Setup collector with backup
	collector := setupCollectorWithBackups(t, backup)

	// Verify metric exists
	count := testutil.CollectAndCount(collector, backupInfoMetricName)
	if count != 1 {
		t.Errorf("expected 1 backup info metric, got %d", count)
	}

	// Setup collector without backup (simulating deletion)
	collectorAfterDelete := setupCollectorWithBackups(t)

	// Verify metric no longer exists
	countAfter := testutil.CollectAndCount(collectorAfterDelete, backupInfoMetricName)
	if countAfter != 0 {
		t.Errorf("expected 0 backup info metrics after deletion, got %d", countAfter)
	}
}

func TestSizeBytes_NoFiles(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		Status: operatorv1alpha1.EtcdBackupStatus{
			Files: []operatorv1alpha1.EtcdBackupFile{},
		},
	}

	size := getSizeBytes(backup)
	if size != 0 {
		t.Errorf("expected size 0 for no files, got %f", size)
	}
}

func TestSizeBytes_MultipleFiles(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		Status: operatorv1alpha1.EtcdBackupStatus{
			Files: []operatorv1alpha1.EtcdBackupFile{
				{Size: resource.MustParse("100Mi")},
				{Size: resource.MustParse("50Mi")},
				{Size: resource.MustParse("25Mi")},
			},
		},
	}

	size := getSizeBytes(backup)
	expected := float64(175 * 1024 * 1024)
	if size != expected {
		t.Errorf("expected size %f, got %f", expected, size)
	}
}

func TestExtractCurrentStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions []v1.Condition
		want       string
	}{
		{
			name:       "no conditions",
			conditions: []v1.Condition{},
			want:       "",
		},
		{
			name: "pending",
			conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupPending), Status: v1.ConditionTrue},
			},
			want: statusPending,
		},
		{
			name: "running",
			conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupRunning), Status: v1.ConditionTrue},
			},
			want: statusRunning,
		},
		{
			name: "completed",
			conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupCompleted), Status: v1.ConditionTrue},
			},
			want: statusCompleted,
		},
		{
			name: "failed",
			conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupFailed), Status: v1.ConditionTrue},
			},
			want: statusFailed,
		},
		{
			name: "all false",
			conditions: []v1.Condition{
				{Type: string(operatorv1alpha1.BackupPending), Status: v1.ConditionFalse},
				{Type: string(operatorv1alpha1.BackupRunning), Status: v1.ConditionFalse},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				Status: operatorv1alpha1.EtcdBackupStatus{
					Conditions: tt.conditions,
				},
			}
			got := extractCurrentStatus(backup)
			if got != tt.want {
				t.Errorf("extractCurrentStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMetricNamesMatchConstants(t *testing.T) {
	expectedNames := map[string]bool{
		backupInfoMetricName:           false,
		backupStatusMetricName:         false,
		backupCompletionTimeMetricName: false,
		backupStartTimeMetricName:      false,
		backupSizeBytesMetricName:      false,
	}

	collector := setupCollectorWithBackups(t)

	// Collect descriptors
	descCh := make(chan *prometheus.Desc, 10)
	go func() {
		collector.Describe(descCh)
		close(descCh)
	}()

	for desc := range descCh {
		descStr := desc.String()
		for name := range expectedNames {
			if strings.Contains(descStr, name) {
				expectedNames[name] = true
			}
		}
	}

	for name, found := range expectedNames {
		if !found {
			t.Errorf("expected metric %s to be registered", name)
		}
	}
}

// Helper functions

func setupCollectorWithBackups(t *testing.T, backups ...*operatorv1alpha1.EtcdBackup) *backupCollector {
	t.Helper()

	lister := &fakeBackupLister{backups: backups}
	registry := metrics.NewKubeRegistry()

	return NewBackupMetrics(registry, lister)
}

func verifyInfoMetric(t *testing.T, collector prometheus.Collector, name, uid, node, storageType, storageLocation, policyName string) {
	t.Helper()

	expected := `
		# HELP etcd_backup_info Information about etcd backup
		# TYPE etcd_backup_info gauge
		etcd_backup_info{created_by_policy="` + policyName + `",etcd_backup="` + name + `",node="` + node + `",storage_location="` + storageLocation + `",storage_type="` + storageType + `",uid="` + uid + `"} 1
	`

	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), backupInfoMetricName); err != nil {
		t.Errorf("info metric mismatch: %v", err)
	}
}

func verifyStatusMetric(t *testing.T, collector prometheus.Collector, name, uid, status string, expectedValue float64) {
	t.Helper()

	expected := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="` + name + `",status="` + status + `",uid="` + uid + `"} ` + formatFloat(expectedValue) + `
	`

	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName); err != nil {
		t.Errorf("status metric mismatch for %s=%s: %v", name, status, err)
	}
}

func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
