package backupcontroller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
)

func TestMetricsRegistration(t *testing.T) {
	c := setupCollectorWithBackups(t)

	require.NotNil(t, c.infoDesc)
	require.NotNil(t, c.statusDesc)
	require.NotNil(t, c.completionTimeDesc)
	require.NotNil(t, c.startTimeDesc)
	require.NotNil(t, c.sizeBytesDesc)
}

func TestBackupMetricsInactiveStatusAllZero(t *testing.T) {
	tests := []struct {
		name   string
		backup *operatorv1alpha1.EtcdBackup
	}{
		{
			name: "absent-conditions",
			backup: testutils.FakeEtcdBackup("never-started",
				testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
					PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-pvc"},
				}),
			),
		},
		{
			name: "pending-unrecognized",
			backup: testutils.FakeEtcdBackup("pending-backup",
				testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
					Conditions: []metav1.Condition{{
						// Pending was removed from the API; an unrecognized True
						// condition must not activate Running/Completed/Failed.
						Type:   "Pending",
						Status: metav1.ConditionTrue,
					}},
				}),
			),
		},
		{
			name: "all-false",
			backup: testutils.FakeEtcdBackup("false-conditions",
				testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
					Conditions: []metav1.Condition{
						{Type: string(operatorv1alpha1.BackupRunning), Status: metav1.ConditionFalse},
						{Type: string(operatorv1alpha1.BackupCompleted), Status: metav1.ConditionFalse},
						{Type: string(operatorv1alpha1.BackupFailed), Status: metav1.ConditionFalse},
					},
				}),
			),
		},
		{
			name: "all-unknown",
			backup: testutils.FakeEtcdBackup("unknown-conditions",
				testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
					Conditions: []metav1.Condition{
						{Type: string(operatorv1alpha1.BackupRunning), Status: metav1.ConditionUnknown},
						{Type: string(operatorv1alpha1.BackupCompleted), Status: metav1.ConditionUnknown},
						{Type: string(operatorv1alpha1.BackupFailed), Status: metav1.ConditionUnknown},
					},
				}),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := setupCollectorWithBackups(t, tt.backup)
			requireStatusMetrics(t, collector, tt.backup.Name, string(tt.backup.UID), "")
		})
	}
}

func TestBackupMetricsCompleted(t *testing.T) {
	completedAt := metav1.NewTime(time.Unix(1700000300, 0))
	snapshotSize := int64(100 * 1024 * 1024)
	staticSize := int64(50 * 1024 * 1024)

	backup := testutils.FakeEtcdBackup("pvc-backup",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupCompleted(),
		func(b *operatorv1alpha1.EtcdBackup) {
			b.Status.NodeName = "node-1"
			b.Status.Conditions[0].LastTransitionTime = completedAt
			b.Status.Files = []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot_2026-09-28_120000.db", SizeBytes: snapshotSize},
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources, Path: "/backups/static_kuberesources_2026-09-28_120000.tar.gz", SizeBytes: staticSize},
			}
		},
	)

	collector := setupCollectorWithBackups(t, backup)

	requireInfoMetric(t, collector, "pvc-backup", "pvc-backup-uid", "node-1", storageTypePVC, "backup-pvc", "")
	requireStatusMetrics(t, collector, "pvc-backup", "pvc-backup-uid", statusCompleted)

	expectedCompletionTime := fmt.Sprintf(`
		# HELP etcd_backup_completion_timestamp_seconds Unix timestamp when the backup completed.
		# TYPE etcd_backup_completion_timestamp_seconds gauge
		etcd_backup_completion_timestamp_seconds{etcd_backup="pvc-backup",uid="pvc-backup-uid"} %d
	`, completedAt.Unix())
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedCompletionTime), backupCompletionTimeMetricName))

	expectedSize := fmt.Sprintf(`
		# HELP etcd_backup_size_bytes Total recorded backup file size in bytes, by asset type.
		# TYPE etcd_backup_size_bytes gauge
		etcd_backup_size_bytes{asset_type="Snapshot",etcd_backup="pvc-backup",uid="pvc-backup-uid"} %d
		etcd_backup_size_bytes{asset_type="StaticKubeResources",etcd_backup="pvc-backup",uid="pvc-backup-uid"} %d
	`, snapshotSize, staticSize)
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedSize), backupSizeBytesMetricName))

	expectedStartTime := fmt.Sprintf(`
		# HELP etcd_backup_start_timestamp_seconds Unix timestamp when the backup was created.
		# TYPE etcd_backup_start_timestamp_seconds gauge
		etcd_backup_start_timestamp_seconds{etcd_backup="pvc-backup",uid="pvc-backup-uid"} %d
	`, backup.CreationTimestamp.Unix())
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedStartTime), backupStartTimeMetricName))
}

func TestBackupMetricsFailed(t *testing.T) {
	failedAt := metav1.NewTime(time.Unix(1700000400, 0))

	backup := testutils.FakeEtcdBackup("pvc-backup-failed",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed),
		func(b *operatorv1alpha1.EtcdBackup) {
			b.Status.Conditions[0].LastTransitionTime = failedAt
		},
	)

	collector := setupCollectorWithBackups(t, backup)

	requireStatusMetrics(t, collector, "pvc-backup-failed", "pvc-backup-failed-uid", statusFailed)

	expectedCompletionTime := fmt.Sprintf(`
		# HELP etcd_backup_completion_timestamp_seconds Unix timestamp when the backup failed.
		# TYPE etcd_backup_completion_timestamp_seconds gauge
		etcd_backup_completion_timestamp_seconds{etcd_backup="pvc-backup-failed",uid="pvc-backup-failed-uid"} %d
	`, failedAt.Unix())
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedCompletionTime), backupCompletionTimeMetricName))
}

func TestBackupMetricsRunning(t *testing.T) {
	createdAt := metav1.NewTime(time.Unix(1700000000, 0))

	backup := testutils.FakeEtcdBackup("pvc-backup-running",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
			NodeName: "node-1",
			Conditions: []metav1.Condition{{
				Type:   string(operatorv1alpha1.BackupRunning),
				Status: metav1.ConditionTrue,
			}},
		}),
		func(b *operatorv1alpha1.EtcdBackup) {
			b.CreationTimestamp = createdAt
		},
	)

	collector := setupCollectorWithBackups(t, backup)

	requireInfoMetric(t, collector, "pvc-backup-running", "pvc-backup-running-uid", "node-1", storageTypePVC, "backup-pvc", "")
	requireStatusMetrics(t, collector, "pvc-backup-running", "pvc-backup-running-uid", statusRunning)

	expectedStartTime := fmt.Sprintf(`
		# HELP etcd_backup_start_timestamp_seconds Unix timestamp when the backup was created.
		# TYPE etcd_backup_start_timestamp_seconds gauge
		etcd_backup_start_timestamp_seconds{etcd_backup="pvc-backup-running",uid="pvc-backup-running-uid"} %d
	`, createdAt.Unix())
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedStartTime), backupStartTimeMetricName))

	require.Equal(t, 0, testutil.CollectAndCount(collector, backupCompletionTimeMetricName),
		"expected 0 completion time metrics for running backup")
	require.Equal(t, 0, testutil.CollectAndCount(collector, backupSizeBytesMetricName),
		"expected 0 size metrics for running backup")
}

func TestBackupMetricsPVCWithPath(t *testing.T) {
	backup := testutils.FakeEtcdBackup("pvc-path-backup",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC: operatorv1alpha1.EtcdBackupStoragePvc{
				Name: "backup-pvc",
				Path: "/custom/path",
			},
		}),
	)

	collector := setupCollectorWithBackups(t, backup)
	requireInfoMetric(t, collector, "pvc-path-backup", "pvc-path-backup-uid", "", storageTypePVC, "backup-pvc:/custom/path", "")
}

func TestBackupMetricsLocalStorage(t *testing.T) {
	backup := testutils.FakeEtcdBackup("local-backup",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
			Local: operatorv1alpha1.EtcdBackupStorageLocal{
				HostPath: "/var/lib/etcd-backup",
			},
		}),
		testutils.WithBackupNodeName("node-2"),
	)

	collector := setupCollectorWithBackups(t, backup)
	requireInfoMetric(t, collector, "local-backup", "local-backup-uid", "node-2", storageTypeLocal, "/var/lib/etcd-backup", "")
}

func TestBackupMetricsLocalStorageEmptyHostPath(t *testing.T) {
	backup := testutils.FakeEtcdBackup("local-default-path",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
			Local: operatorv1alpha1.EtcdBackupStorageLocal{},
		}),
		testutils.WithBackupNodeName("node-2"),
	)

	collector := setupCollectorWithBackups(t, backup)
	requireInfoMetric(t, collector, "local-default-path", "local-default-path-uid", "node-2", storageTypeLocal, backuphelpers.BackupPathMount, "")
}

func TestBackupMetricsPolicyName(t *testing.T) {
	backup := testutils.FakeEtcdBackup("policy-backup",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupPolicy("daily-policy"),
	)

	collector := setupCollectorWithBackups(t, backup)
	requireInfoMetric(t, collector, "policy-backup", "policy-backup-uid", "", storageTypePVC, "backup-pvc", "daily-policy")
}

func TestBackupMetricsMultipleBackups(t *testing.T) {
	backup1 := testutils.FakeEtcdBackup("backup-1",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "pvc-1"},
		}),
		testutils.WithBackupCompleted(),
	)
	backup2 := testutils.FakeEtcdBackup("backup-2",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "pvc-2"},
		}),
		testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
			Conditions: []metav1.Condition{{
				Type:   string(operatorv1alpha1.BackupRunning),
				Status: metav1.ConditionTrue,
			}},
		}),
	)

	collector := setupCollectorWithBackups(t, backup1, backup2)

	expected := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="backup-1",status="Completed",uid="backup-1-uid"} 1
		etcd_backup_status{etcd_backup="backup-1",status="Failed",uid="backup-1-uid"} 0
		etcd_backup_status{etcd_backup="backup-1",status="Running",uid="backup-1-uid"} 0
		etcd_backup_status{etcd_backup="backup-2",status="Completed",uid="backup-2-uid"} 0
		etcd_backup_status{etcd_backup="backup-2",status="Failed",uid="backup-2-uid"} 0
		etcd_backup_status{etcd_backup="backup-2",status="Running",uid="backup-2-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName))
}

func TestBackupMetricsStatusTransition(t *testing.T) {
	backup := testutils.FakeEtcdBackup("transition-backup",
		testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
			Conditions: []metav1.Condition{{
				Type:   string(operatorv1alpha1.BackupRunning),
				Status: metav1.ConditionTrue,
			}},
		}),
	)

	collector, indexer := setupCollectorWithIndexer(t, backup)
	requireStatusMetrics(t, collector, "transition-backup", "transition-backup-uid", statusRunning)

	backup.Status.Conditions = []metav1.Condition{{
		Type:   string(operatorv1alpha1.BackupCompleted),
		Status: metav1.ConditionTrue,
	}}
	require.NoError(t, indexer.Update(backup))

	requireStatusMetrics(t, collector, "transition-backup", "transition-backup-uid", statusCompleted)
}

func TestBackupMetricsDeletion(t *testing.T) {
	backup := testutils.FakeEtcdBackup("to-delete",
		testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
			Conditions: []metav1.Condition{{
				Type:   string(operatorv1alpha1.BackupRunning),
				Status: metav1.ConditionTrue,
			}},
		}),
	)

	collector, indexer := setupCollectorWithIndexer(t, backup)
	require.Equal(t, 1, testutil.CollectAndCount(collector, backupInfoMetricName))
	requireStatusMetrics(t, collector, "to-delete", "to-delete-uid", statusRunning)

	require.NoError(t, indexer.Delete(backup))

	require.Equal(t, 0, testutil.CollectAndCount(collector, backupInfoMetricName),
		"expected 0 backup info metrics after deletion")
	require.Equal(t, 0, testutil.CollectAndCount(collector, backupStatusMetricName),
		"expected all status samples to disappear after deletion")
}

func TestSizeBytesByAssetType(t *testing.T) {
	tests := []struct {
		name  string
		files []operatorv1alpha1.EtcdBackupFile
		want  map[string]float64
	}{
		{
			name:  "no-files",
			files: []operatorv1alpha1.EtcdBackupFile{},
			want:  map[string]float64{},
		},
		{
			name: "both-assets",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db", SizeBytes: 100 * 1024 * 1024},
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources, Path: "/backups/static.tar.gz", SizeBytes: 50 * 1024 * 1024},
			},
			want: map[string]float64{
				assetTypeSnapshot:            float64(100 * 1024 * 1024),
				assetTypeStaticKubeResources: float64(50 * 1024 * 1024),
			},
		},
		{
			name: "snapshot-only",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db", SizeBytes: 200 * 1024 * 1024},
			},
			want: map[string]float64{
				assetTypeSnapshot: float64(200 * 1024 * 1024),
			},
		},
		{
			name: "static-only",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources, Path: "/backups/static.tar.gz", SizeBytes: 50 * 1024 * 1024},
			},
			want: map[string]float64{
				assetTypeStaticKubeResources: float64(50 * 1024 * 1024),
			},
		},
		{
			name: "recorded-empty-file-is-zero",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db"},
			},
			want: map[string]float64{
				assetTypeSnapshot: 0,
			},
		},
		{
			name: "sum-same-asset-type",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot-a.db", SizeBytes: 10},
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot-b.db", SizeBytes: 15},
			},
			want: map[string]float64{
				assetTypeSnapshot: 25,
			},
		},
		{
			name: "missing-asset-type-skipped",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot.db", SizeBytes: 100 * 1024 * 1024},
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources, Path: "/backups/static.tar.gz", SizeBytes: 50},
			},
			want: map[string]float64{
				assetTypeStaticKubeResources: 50,
			},
		},
		{
			name: "negative-size-skipped",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db", SizeBytes: -1},
			},
			want: map[string]float64{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				Status: operatorv1alpha1.EtcdBackupStatus{Files: tt.files},
			}
			require.Equal(t, tt.want, sizeBytesByAssetType(backup))
		})
	}
}

func TestGetStartTime(t *testing.T) {
	createdAt := metav1.NewTime(time.Unix(1700000000, 0))

	tests := []struct {
		name              string
		creationTimestamp metav1.Time
		want              float64
	}{
		{
			name:              "with-creation-timestamp",
			creationTimestamp: createdAt,
			want:              float64(createdAt.Unix()),
		},
		{
			name:              "zero-creation-timestamp",
			creationTimestamp: metav1.Time{},
			want:              0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: tt.creationTimestamp},
			}
			require.Equal(t, tt.want, getStartTime(backup))
		})
	}
}

func TestGetCompletionTime(t *testing.T) {
	completedAt := metav1.NewTime(time.Unix(1700000300, 0))
	failedAt := metav1.NewTime(time.Unix(1700000400, 0))

	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       float64
	}{
		{
			name: "completed-true",
			conditions: []metav1.Condition{{
				Type:               string(operatorv1alpha1.BackupCompleted),
				Status:             metav1.ConditionTrue,
				LastTransitionTime: completedAt,
			}},
			want: float64(completedAt.Unix()),
		},
		{
			name: "failed-only",
			conditions: []metav1.Condition{{
				Type:               string(operatorv1alpha1.BackupFailed),
				Status:             metav1.ConditionTrue,
				LastTransitionTime: failedAt,
			}},
			want: float64(failedAt.Unix()), // Failed timestamps power high-failure-rate alerts
		},
		{
			name:       "missing-completed",
			conditions: []metav1.Condition{},
			want:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				Status: operatorv1alpha1.EtcdBackupStatus{Conditions: tt.conditions},
			}
			require.Equal(t, tt.want, getCompletionTime(backup))
		})
	}
}

func TestExtractCurrentStatus(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       string
	}{
		{
			name:       "no-conditions",
			conditions: []metav1.Condition{},
			want:       "",
		},
		{
			name: "running",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupRunning), Status: metav1.ConditionTrue},
			},
			want: statusRunning,
		},
		{
			name: "completed",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupCompleted), Status: metav1.ConditionTrue},
			},
			want: statusCompleted,
		},
		{
			name: "failed",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupFailed), Status: metav1.ConditionTrue},
			},
			want: statusFailed,
		},
		{
			name: "all-false",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupRunning), Status: metav1.ConditionFalse},
				{Type: string(operatorv1alpha1.BackupCompleted), Status: metav1.ConditionFalse},
			},
			want: "",
		},
		{
			name: "unknown",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupRunning), Status: metav1.ConditionUnknown},
			},
			want: "",
		},
		{
			name: "pending-unrecognized",
			conditions: []metav1.Condition{
				{Type: "Pending", Status: metav1.ConditionTrue},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				Status: operatorv1alpha1.EtcdBackupStatus{Conditions: tt.conditions},
			}
			require.Equal(t, tt.want, extractCurrentStatus(backup))
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
		require.Truef(t, found, "expected metric %s to be registered", name)
	}
}

// TestBackupMetricsScrapeSafety verifies Collect() doesn't panic with the real generated
// lister. Production code must pass labels.Everything() to List(), not nil.
func TestBackupMetricsScrapeSafety(t *testing.T) {
	backup := testutils.FakeEtcdBackup("scrape-safety-test")
	collector := setupCollectorWithBackups(t, backup)

	require.NotPanics(t, func() {
		metricCh := make(chan prometheus.Metric, 10)
		go func() {
			collector.Collect(metricCh)
			close(metricCh)
		}()
		for range metricCh {
		}
	})

	require.GreaterOrEqual(t, testutil.CollectAndCount(collector, backupInfoMetricName), 1)
}

func TestBackupMetricsRealLister(t *testing.T) {
	backup1 := testutils.FakeEtcdBackup("real-lister-backup-1", testutils.WithBackupCompleted())
	// Unstarted backups emit all three status samples at 0.
	backup2 := testutils.FakeEtcdBackup("real-lister-backup-2")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "real-lister-backup-3-job"}}
	backup3 := testutils.FakeEtcdBackup("real-lister-backup-3", testutils.WithBackupRunning(job))

	collector := setupCollectorWithBackups(t, backup1, backup2, backup3)

	require.GreaterOrEqual(t, testutil.CollectAndCount(collector), 12,
		"expected at least 3 info + 9 status metrics")

	expected := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="real-lister-backup-1",status="Completed",uid="real-lister-backup-1-uid"} 1
		etcd_backup_status{etcd_backup="real-lister-backup-1",status="Failed",uid="real-lister-backup-1-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-1",status="Running",uid="real-lister-backup-1-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-2",status="Completed",uid="real-lister-backup-2-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-2",status="Failed",uid="real-lister-backup-2-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-2",status="Running",uid="real-lister-backup-2-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-3",status="Completed",uid="real-lister-backup-3-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-3",status="Failed",uid="real-lister-backup-3-uid"} 0
		etcd_backup_status{etcd_backup="real-lister-backup-3",status="Running",uid="real-lister-backup-3-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName))
}

func setupCollectorWithBackups(t *testing.T, backups ...*operatorv1alpha1.EtcdBackup) *backupCollector {
	t.Helper()
	collector, _ := setupCollectorWithIndexer(t, backups...)
	return collector
}

func setupCollectorWithIndexer(t *testing.T, backups ...*operatorv1alpha1.EtcdBackup) (*backupCollector, cache.Indexer) {
	t.Helper()

	objects := make([]runtime.Object, 0, len(backups))
	objects = testutils.AppendRuntimeObjects(objects, backups)

	fakeClient := operatorfake.NewSimpleClientset(objects...)
	informerFactory := operatorinformers.NewSharedInformerFactory(fakeClient, 0)
	backupInformer := informerFactory.Operator().V1alpha1().EtcdBackups()

	indexer := backupInformer.Informer().GetIndexer()
	for _, backup := range backups {
		require.NoError(t, indexer.Add(backup))
	}

	return NewBackupMetrics(metrics.NewKubeRegistry(), backupInformer.Lister()), indexer
}

func requireStatusMetrics(t *testing.T, collector prometheus.Collector, name, uid, activeStatus string) {
	t.Helper()

	completed, failed, running := float64(0), float64(0), float64(0)
	switch activeStatus {
	case statusCompleted:
		completed = 1
	case statusFailed:
		failed = 1
	case statusRunning:
		running = 1
	case "":
		// no recognized status active — all zeros
	default:
		t.Fatalf("unexpected active status %q", activeStatus)
	}

	expected := fmt.Sprintf(`
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="%s",status="Completed",uid="%s"} %g
		etcd_backup_status{etcd_backup="%s",status="Failed",uid="%s"} %g
		etcd_backup_status{etcd_backup="%s",status="Running",uid="%s"} %g
	`, name, uid, completed, name, uid, failed, name, uid, running)
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName))
}

func TestBackupMetricsSizeByAsset(t *testing.T) {
	tests := []struct {
		name          string
		files         []operatorv1alpha1.EtcdBackupFile
		requested     []operatorv1alpha1.EtcdBackupAssetType
		expectedSizes string
		wantCount     int
	}{
		{
			name: "both-recorded",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db", SizeBytes: 64675872},
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources, Path: "/backups/static.tar.gz", SizeBytes: 54228},
			},
			expectedSizes: `
				# HELP etcd_backup_size_bytes Total recorded backup file size in bytes, by asset type.
				# TYPE etcd_backup_size_bytes gauge
				etcd_backup_size_bytes{asset_type="Snapshot",etcd_backup="size-backup",uid="size-backup-uid"} 64675872
				etcd_backup_size_bytes{asset_type="StaticKubeResources",etcd_backup="size-backup",uid="size-backup-uid"} 54228
			`,
			wantCount: 2,
		},
		{
			name: "snapshot-only",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db", SizeBytes: 100},
			},
			expectedSizes: `
				# HELP etcd_backup_size_bytes Total recorded backup file size in bytes, by asset type.
				# TYPE etcd_backup_size_bytes gauge
				etcd_backup_size_bytes{asset_type="Snapshot",etcd_backup="size-backup",uid="size-backup-uid"} 100
			`,
			wantCount: 1,
		},
		{
			name: "static-only",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources, Path: "/backups/static.tar.gz", SizeBytes: 50},
			},
			expectedSizes: `
				# HELP etcd_backup_size_bytes Total recorded backup file size in bytes, by asset type.
				# TYPE etcd_backup_size_bytes gauge
				etcd_backup_size_bytes{asset_type="StaticKubeResources",etcd_backup="size-backup",uid="size-backup-uid"} 50
			`,
			wantCount: 1,
		},
		{
			name: "recorded-empty-file",
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db"},
			},
			expectedSizes: `
				# HELP etcd_backup_size_bytes Total recorded backup file size in bytes, by asset type.
				# TYPE etcd_backup_size_bytes gauge
				etcd_backup_size_bytes{asset_type="Snapshot",etcd_backup="size-backup",uid="size-backup-uid"} 0
			`,
			wantCount: 1,
		},
		{
			name: "partial-recorded",
			requested: []operatorv1alpha1.EtcdBackupAssetType{
				operatorv1alpha1.EtcdBackupAssetTypeSnapshot,
				operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources,
			},
			files: []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: "/backups/snapshot.db", SizeBytes: 10},
			},
			expectedSizes: `
				# HELP etcd_backup_size_bytes Total recorded backup file size in bytes, by asset type.
				# TYPE etcd_backup_size_bytes gauge
				etcd_backup_size_bytes{asset_type="Snapshot",etcd_backup="size-backup",uid="size-backup-uid"} 10
			`,
			wantCount: 1,
		},
		{
			name: "requested-but-not-recorded",
			requested: []operatorv1alpha1.EtcdBackupAssetType{
				operatorv1alpha1.EtcdBackupAssetTypeSnapshot,
				operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources,
			},
			files:     nil,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []func(*operatorv1alpha1.EtcdBackup){
				testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
					PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-pvc"},
				}),
				testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{Files: tt.files}),
			}
			if tt.requested != nil {
				opts = append(opts, testutils.WithBackupAssets(tt.requested...))
			}
			backup := testutils.FakeEtcdBackup("size-backup", opts...)
			collector := setupCollectorWithBackups(t, backup)

			require.Equal(t, tt.wantCount, testutil.CollectAndCount(collector, backupSizeBytesMetricName))
			if tt.wantCount == 0 {
				return
			}
			require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(tt.expectedSizes), backupSizeBytesMetricName))
		})
	}
}

func requireInfoMetric(t *testing.T, collector prometheus.Collector, name, uid, node, storageType, storageLocation, policyName string) {
	t.Helper()

	expected := `
		# HELP etcd_backup_info Information about etcd backup
		# TYPE etcd_backup_info gauge
		etcd_backup_info{etcd_backup="` + name + `",etcd_backup_policy="` + policyName + `",node="` + node + `",storage_location="` + storageLocation + `",storage_type="` + storageType + `",uid="` + uid + `"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupInfoMetricName))
}
