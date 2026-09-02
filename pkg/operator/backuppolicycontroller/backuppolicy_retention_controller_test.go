package backuppolicycontroller

import (
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type testCaseBackupPolicyRetentionController struct {
	backupPolicies []*operatorv1alpha1.EtcdBackupPolicy
	backups        []*operatorv1alpha1.EtcdBackup
	expectErr      bool
	validate       func(t *testing.T, operatorFake *operatorfake.Clientset)
}

func runBackupPolicyRetentionControllerTest(t *testing.T, tc testCaseBackupPolicyRetentionController) {
	t.Helper()
	operatorObjs := make([]runtime.Object, 0, len(tc.backupPolicies)+len(tc.backups))
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backupPolicies)
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backups)
	operatorFake := operatorfake.NewClientset(operatorObjs...)

	operatorSharedFactory := operatorinformers.NewSharedInformerFactory(operatorFake, 0)

	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()
	backupPoliciesInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackupPolicies()

	backupsInformerHasSynced := backupsInformer.Informer().HasSynced
	backupPoliciesInformerHasSynced := backupPoliciesInformer.Informer().HasSynced

	ctx := t.Context()
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), backupsInformerHasSynced, backupPoliciesInformerHasSynced)

	controller := &BackupPolicyRetentionController{
		backupsLister:        backupsInformer.Lister(),
		backupPoliciesLister: backupPoliciesInformer.Lister(),
		operatorClient:       operatorFake.OperatorV1alpha1(),
		featureGateAccessor:  backupFeatureGateAccessor,
	}

	for _, backupPolicy := range tc.backupPolicies {
		syncCtx := testutils.FakeSyncContext(t, backupPolicy.Name)
		err := controller.sync(t.Context(), syncCtx)
		if tc.expectErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}

	if tc.validate != nil {
		tc.validate(t, operatorFake)
	}
}

func requireDeletedBackups(names ...string) func(t *testing.T, operatorFake *operatorfake.Clientset) {
	return func(t *testing.T, operatorFake *operatorfake.Clientset) {
		t.Helper()
		deletedNames := []string{}
		for _, action := range operatorFake.Actions() {
			if deleteAction, ok := action.(k8stesting.DeleteActionImpl); ok {
				deletedNames = append(deletedNames, deleteAction.Name)
			}
		}

		require.ElementsMatch(t, names, deletedNames)
	}
}

func TestBackupPolicyRetentionPruneByQuantity(t *testing.T) {
	// Prune EtcdBackups by quantity
	t.Run("local", func(t *testing.T) {
		// Count per node in local storage mode
		runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
			backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
				testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
					testutils.WithBackupPolicyStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
						Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity, MaxQuantity: 1,
					}))},
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup-1",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{NodeName: "test-node-1"}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(2*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-2",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{NodeName: "test-node-2"}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(1*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-3",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{NodeName: "test-node-2"}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(0*time.Hour)),
			},
			validate: requireDeletedBackups("test-backup-2")})
	})
	t.Run("pvc", func(t *testing.T) {
		// Count all backups controlled by the policy in PVC storage mode
		runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
			backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
				testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
					testutils.WithBackupPolicyStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypePVC, PVC: &operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"},
					}),
					testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
						Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity, MaxQuantity: 1,
					}))},
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(2*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(1*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(0*time.Hour)),
			},
			validate: requireDeletedBackups("test-backup-1", "test-backup-2")})
	})
}

func TestBackupPolicyRetentionPruneBySize(t *testing.T) {
	// Prune EtcdBackups by size on disk
	t.Run("local", func(t *testing.T) {
		// Sum file size per node in local storage mode
		runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
			backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
				testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
					testutils.WithBackupPolicyStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
						Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxSize, MaxSize: *resource.NewQuantity(1000, resource.BinarySI),
					}))},
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup-1",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
						NodeName: "test-node-1",
						Files: []operatorv1alpha1.EtcdBackupFile{
							{Path: "1.db", Size: *resource.NewQuantity(300, resource.BinarySI)},
						}}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(2*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-2",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
						NodeName: "test-node-2",
						Files: []operatorv1alpha1.EtcdBackupFile{
							{Path: "2.db", Size: *resource.NewQuantity(500, resource.BinarySI)},
						}}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(1*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-3",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
					}),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
						NodeName: "test-node-2",
						Files: []operatorv1alpha1.EtcdBackupFile{
							{Path: "3.db", Size: *resource.NewQuantity(600, resource.BinarySI)},
						}}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(0*time.Hour)),
			},
			validate: requireDeletedBackups("test-backup-2")})
	})
	t.Run("pvc", func(t *testing.T) {
		// Sum file size for all backups controlled by the policy in PVC storage mode
		runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
			backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
				testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
					testutils.WithBackupPolicyStorage(operatorv1alpha1.EtcdBackupStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypePVC, PVC: &operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"},
					}),
					testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
						Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxSize, MaxSize: *resource.NewQuantity(1000, resource.BinarySI),
					}))},
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup-1",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
						Files: []operatorv1alpha1.EtcdBackupFile{
							{Path: "1.db", Size: *resource.NewQuantity(300, resource.BinarySI)},
						}}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(2*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-2",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
						Files: []operatorv1alpha1.EtcdBackupFile{
							{Path: "2.db", Size: *resource.NewQuantity(400, resource.BinarySI)},
						}}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(1*time.Hour)),
				testutils.FakeEtcdBackup("test-backup-3",
					testutils.WithBackupPolicy("test-backup-policy"),
					testutils.WithBackupStatus(operatorv1alpha1.EtcdBackupStatus{
						Files: []operatorv1alpha1.EtcdBackupFile{
							{Path: "3.db", Size: *resource.NewQuantity(500, resource.BinarySI)},
						}}),
					testutils.WithBackupCompleted(),
					testutils.WithBackupAge(0*time.Hour)),
			},
			validate: requireDeletedBackups("test-backup-1")})
	})
}

func TestBackupPolicyRetentionFailedBackups(t *testing.T) {
	// Delete failed backups that exceed FailedBackupsHistoryLimit
	runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly", func(backup *operatorv1alpha1.EtcdBackupPolicy) {
				backup.Spec.FailedBackupsHistoryLimit = 1
			})},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupFailed(), testutils.WithBackupAge(3*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(2*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupFailed(), testutils.WithBackupAge(1*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-4", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupFailed(), testutils.WithBackupAge(0*time.Hour)),
		},
		validate: requireDeletedBackups("test-backup-1", "test-backup-3")})
}

func TestBackupPolicyRetentionIgnoreStandaloneBackups(t *testing.T) {
	// Ignore backups that are not controlled by an EtcdBackupPolicy
	runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
				testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity, MaxQuantity: 2,
				}))},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupCompleted(), testutils.WithBackupAge(2*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(1*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(0*time.Hour)),
		},
		validate: requireDeletedBackups()})
}

func TestBackupPolicyRetentionIgnoreDeletedPolicy(t *testing.T) {
	// Ignore retention when the EtcdBackupPolicy has been deleted
	runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
				testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity, MaxQuantity: 1,
				}),
				testutils.WithBackupPolicyDeleted())},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(2*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(1*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupPolicy("test-backup-policy-2"), testutils.WithBackupCompleted(), testutils.WithBackupAge(0*time.Hour)),
		},
		validate: requireDeletedBackups()})
}

func TestBackupPolicyRetentionIgnoreFailedBackups(t *testing.T) {
	// Do not enforce retention on failed EtcdBackups
	// TODO: There should be some mechanism for cleaning up failed EtcdBackup objects after TTL
	runBackupPolicyRetentionControllerTest(t, testCaseBackupPolicyRetentionController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@hourly",
				testutils.WithBackupPolicyRetentionRules(operatorv1alpha1.EtcdBackupPolicyRetentionRule{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity, MaxQuantity: 2,
				}))},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(2*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted(), testutils.WithBackupAge(1*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupFailed(), testutils.WithBackupAge(0*time.Hour)),
		},
		validate: requireDeletedBackups()})
}
