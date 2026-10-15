package backupcontroller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestBackupMetricsNeverRun(t *testing.T) {
	backup := testutils.FakeEtcdBackup("never-started",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-pvc"},
		}),
	)
	collector := setupCollectorWithBackups(t, backup)

	require.Equal(t, 0, testutil.CollectAndCount(collector, backupStatusMetricName),
		"expected 0 status metrics for never-run backup")
}

func TestBackupMetricsCompleted(t *testing.T) {
	completedAt := metav1.NewTime(time.Unix(1700000300, 0))
	snapshotSize := int64(100 * 1024 * 1024)

	backup := testutils.FakeEtcdBackup("pvc-backup",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupCompleted(),
		func(b *operatorv1alpha1.EtcdBackup) {
			b.Status.NodeName = "node-1"
			b.Status.Conditions[0].LastTransitionTime = completedAt
			// Override default fixture files so the snapshot matches getSizeBytes naming rules.
			b.Status.Files = []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.db", SizeBytes: snapshotSize},
				{Path: "/backups/static_kuberesources_2026-09-28_120000.tar.gz", SizeBytes: 50 * 1024 * 1024},
			}
		},
	)

	collector := setupCollectorWithBackups(t, backup)

	requireInfoMetric(t, collector, "pvc-backup", "pvc-backup-uid", "node-1", storageTypePVC, "backup-pvc", "")

	expectedStatus := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="pvc-backup",status="Completed",uid="pvc-backup-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedStatus), backupStatusMetricName))

	expectedCompletionTime := fmt.Sprintf(`
		# HELP etcd_backup_completion_time Unix timestamp when the backup completed.
		# TYPE etcd_backup_completion_time gauge
		etcd_backup_completion_time{etcd_backup="pvc-backup",uid="pvc-backup-uid"} %d
	`, completedAt.Unix())
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedCompletionTime), backupCompletionTimeMetricName))

	expectedSize := fmt.Sprintf(`
		# HELP etcd_backup_size_bytes The file size of the backup snapshot.
		# TYPE etcd_backup_size_bytes gauge
		etcd_backup_size_bytes{etcd_backup="pvc-backup",uid="pvc-backup-uid"} %d
	`, snapshotSize)
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedSize), backupSizeBytesMetricName))

	require.Equal(t, 0, testutil.CollectAndCount(collector, backupStartTimeMetricName),
		"expected 0 start time metrics for completed backup")
}

func TestBackupMetricsFailed(t *testing.T) {
	backup := testutils.FakeEtcdBackup("pvc-backup-failed",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupFailed(),
		func(b *operatorv1alpha1.EtcdBackup) {
			b.Status.Conditions = append(b.Status.Conditions, metav1.Condition{
				Type:   string(operatorv1alpha1.BackupGarbageCollectionRequired),
				Status: metav1.ConditionTrue,
				Reason: string(operatorv1alpha1.BackupReasonFilesPartiallyCreated),
			})
		},
	)

	collector := setupCollectorWithBackups(t, backup)

	expectedStatus := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="pvc-backup-failed",status="Failed",uid="pvc-backup-failed-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedStatus), backupStatusMetricName))

	// Failed backups do not emit completion_time today (only Completed does).
	require.Equal(t, 0, testutil.CollectAndCount(collector, backupCompletionTimeMetricName),
		"expected 0 completion time metrics for failed backup")
}

func TestBackupMetricsRunning(t *testing.T) {
	startedAt := metav1.NewTime(time.Unix(1700000000, 0))

	backup := testutils.FakeEtcdBackup("pvc-backup-running",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc"},
		}),
		testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
			NodeName: "node-1",
			Conditions: []metav1.Condition{{
				Type:               string(operatorv1alpha1.BackupRunning),
				Status:             metav1.ConditionTrue,
				LastTransitionTime: startedAt,
			}},
		}),
	)

	collector := setupCollectorWithBackups(t, backup)

	requireInfoMetric(t, collector, "pvc-backup-running", "pvc-backup-running-uid", "node-1", storageTypePVC, "backup-pvc", "")

	expectedStatus := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="pvc-backup-running",status="Running",uid="pvc-backup-running-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedStatus), backupStatusMetricName))

	expectedStartTime := fmt.Sprintf(`
		# HELP etcd_backup_start_time Unix timestamp when the backup started.
		# TYPE etcd_backup_start_time gauge
		etcd_backup_start_time{etcd_backup="pvc-backup-running",uid="pvc-backup-running-uid"} %d
	`, startedAt.Unix())
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

func TestBackupMetricsCreatedByPolicy(t *testing.T) {
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
		etcd_backup_status{etcd_backup="backup-2",status="Running",uid="backup-2-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName))
}

func TestBackupMetricsDeletion(t *testing.T) {
	backup := testutils.FakeEtcdBackup("to-delete")

	collector := setupCollectorWithBackups(t, backup)
	require.Equal(t, 1, testutil.CollectAndCount(collector, backupInfoMetricName))

	collectorAfterDelete := setupCollectorWithBackups(t)
	require.Equal(t, 0, testutil.CollectAndCount(collectorAfterDelete, backupInfoMetricName),
		"expected 0 backup info metrics after deletion")
}

func TestGetSizeBytes(t *testing.T) {
	tests := []struct {
		name     string
		files    []operatorv1alpha1.EtcdBackupFile
		wantSize float64
		wantOk   bool
	}{
		{
			name:     "no-files",
			files:    []operatorv1alpha1.EtcdBackupFile{},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "valid-snapshot-with-archive",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.db", SizeBytes: 100 * 1024 * 1024},
				{Path: "/backups/static_kuberesources_2026-09-28_120000.tar.gz", SizeBytes: 50 * 1024 * 1024},
			},
			wantSize: float64(100 * 1024 * 1024),
			wantOk:   true,
		},
		{
			name: "partial-snapshot-excluded",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.db.part", SizeBytes: 100 * 1024 * 1024},
				{Path: "/backups/static_kuberesources_2026-09-28_120000.tar.gz", SizeBytes: 50 * 1024 * 1024},
			},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "nested-path-matches-basename",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/var/lib/etcd/backups/deep/path/snapshot_2026-09-28_120000.db", SizeBytes: 200 * 1024 * 1024},
			},
			wantSize: float64(200 * 1024 * 1024),
			wantOk:   true,
		},
		{
			name: "duplicate-snapshots-rejected",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.db", SizeBytes: 100 * 1024 * 1024},
				{Path: "/backups/snapshot_2026-09-28_130000.db", SizeBytes: 100 * 1024 * 1024},
			},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "invalid-size-zero-rejected",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.db", SizeBytes: 0},
			},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "invalid-size-negative-rejected",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.db", SizeBytes: -100 * 1024 * 1024},
			},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "only-archive-no-snapshot",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/static_kuberesources_2026-09-28_120000.tar.gz", SizeBytes: 50 * 1024 * 1024},
			},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "wrong-prefix-not-matched",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/backup_2026-09-28_120000.db", SizeBytes: 100 * 1024 * 1024},
			},
			wantSize: 0,
			wantOk:   false,
		},
		{
			name: "wrong-suffix-not-matched",
			files: []operatorv1alpha1.EtcdBackupFile{
				{Path: "/backups/snapshot_2026-09-28_120000.txt", SizeBytes: 100 * 1024 * 1024},
			},
			wantSize: 0,
			wantOk:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				Status: operatorv1alpha1.EtcdBackupStatus{Files: tt.files},
			}
			gotSize, gotOk := getSizeBytes(backup)
			require.Equal(t, tt.wantSize, gotSize)
			require.Equal(t, tt.wantOk, gotOk)
		})
	}
}

func TestGetStartTime(t *testing.T) {
	startedAt := metav1.NewTime(time.Unix(1700000000, 0))

	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       float64
	}{
		{
			name: "running-true",
			conditions: []metav1.Condition{{
				Type:               string(operatorv1alpha1.BackupRunning),
				Status:             metav1.ConditionTrue,
				LastTransitionTime: startedAt,
			}},
			want: float64(startedAt.Unix()),
		},
		{
			name: "running-false",
			conditions: []metav1.Condition{{
				Type:               string(operatorv1alpha1.BackupRunning),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: startedAt,
			}},
			want: 0,
		},
		{
			name:       "missing-running",
			conditions: []metav1.Condition{},
			want:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := operatorv1alpha1.EtcdBackup{
				Status: operatorv1alpha1.EtcdBackupStatus{Conditions: tt.conditions},
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
			want: 0, // getCompletionTime only considers Completed today
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
			name: "pending",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupPending), Status: metav1.ConditionTrue},
			},
			want: statusPending,
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
			name: "failed-with-garbage-collection-required",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupFailed), Status: metav1.ConditionTrue},
				{Type: string(operatorv1alpha1.BackupGarbageCollectionRequired), Status: metav1.ConditionTrue},
			},
			want: statusFailed,
		},
		{
			name: "all-false",
			conditions: []metav1.Condition{
				{Type: string(operatorv1alpha1.BackupPending), Status: metav1.ConditionFalse},
				{Type: string(operatorv1alpha1.BackupRunning), Status: metav1.ConditionFalse},
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
	backup2 := testutils.FakeEtcdBackup("real-lister-backup-2", testutils.WithBackupPending(""))

	collector := setupCollectorWithBackups(t, backup1, backup2)

	require.GreaterOrEqual(t, testutil.CollectAndCount(collector), 4,
		"expected at least 2 info + 2 status metrics")

	expected := `
		# HELP etcd_backup_status The current status of the backup. Value of 1 indicates the labeled status is active.
		# TYPE etcd_backup_status gauge
		etcd_backup_status{etcd_backup="real-lister-backup-1",status="Completed",uid="real-lister-backup-1-uid"} 1
		etcd_backup_status{etcd_backup="real-lister-backup-2",status="Pending",uid="real-lister-backup-2-uid"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupStatusMetricName))
}

func setupCollectorWithBackups(t *testing.T, backups ...*operatorv1alpha1.EtcdBackup) *backupCollector {
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

	return NewBackupMetrics(metrics.NewKubeRegistry(), backupInformer.Lister())
}

func requireInfoMetric(t *testing.T, collector prometheus.Collector, name, uid, node, storageType, storageLocation, policyName string) {
	t.Helper()

	expected := `
		# HELP etcd_backup_info Information about etcd backup
		# TYPE etcd_backup_info gauge
		etcd_backup_info{created_by_policy="` + policyName + `",etcd_backup="` + name + `",node="` + node + `",storage_location="` + storageLocation + `",storage_type="` + storageType + `",uid="` + uid + `"} 1
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expected), backupInfoMetricName))
}
