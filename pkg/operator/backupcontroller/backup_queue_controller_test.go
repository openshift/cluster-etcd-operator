package backupcontroller

import (
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type testCaseBackupQueueController struct {
	backups             []*operatorv1alpha1.EtcdBackup
	staleBackupCache    []*operatorv1alpha1.EtcdBackup
	nodes               []*corev1.Node
	activeCache         *activeBackupCache
	populateActiveCache bool
	expectError         bool
	validate            func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
}

func runBackupQueueControllerTest(t *testing.T, tc testCaseBackupQueueController) {
	t.Helper()
	operatorObjs := make([]runtime.Object, 0, len(tc.backups))
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backups)

	k8sObjs := make([]runtime.Object, 0, len(tc.nodes))
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.nodes)

	client := k8sfakeclient.NewSimpleClientset(k8sObjs...)
	operatorFake := operatorfake.NewSimpleClientset(operatorObjs...)

	sharedFactory := informers.NewSharedInformerFactory(client, 0)
	var operatorSharedFactory operatorinformers.SharedInformerFactory
	if tc.staleBackupCache == nil {
		operatorSharedFactory = operatorinformers.NewSharedInformerFactory(operatorFake, 0)
	} else {
		staleOperatorObjs := make([]runtime.Object, 0, len(tc.staleBackupCache))
		staleOperatorObjs = testutils.AppendRuntimeObjects(staleOperatorObjs, tc.staleBackupCache)

		staleOperatorFake := operatorfake.NewSimpleClientset(staleOperatorObjs...)
		operatorSharedFactory = operatorinformers.NewSharedInformerFactory(staleOperatorFake, 0)
	}

	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()
	nodesInformer := sharedFactory.Core().V1().Nodes()

	backupsInformerHasSynced := backupsInformer.Informer().HasSynced
	nodesInformerHasSynced := nodesInformer.Informer().HasSynced

	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), backupsInformerHasSynced, nodesInformerHasSynced)

	var activeCache activeBackupCache
	if tc.activeCache == nil {
		activeCache = newActiveBackupCache()
	} else {
		activeCache = *tc.activeCache
	}
	controller := BackupQueueController{
		backupsLister:       backupsInformer.Lister(),
		nodeLister:          nodesInformer.Lister(),
		operatorClient:      operatorFake.OperatorV1alpha1(),
		featureGateAccessor: backupFeatureGateAccessor,
		activeCache:         activeCache,
	}
	if tc.populateActiveCache {
		for _, backup := range tc.backups {
			if len(backup.Status.Conditions) > 0 && !backuphelpers.IsBackupFinished(backup) {
				activeCache.add(backup)
			}
		}
	}

	err := controller.sync(ctx, nil)
	if tc.expectError {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	if tc.validate != nil {
		tc.validate(t, client, operatorFake)
	}
}

func TestBackupQueueSelectAvailableNode(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node-1")),
			testutils.FakeEtcdBackup("new")},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected update action")

			backup := action.Object.(*operatorv1alpha1.EtcdBackup)
			require.Equal(t, backup.Name, "new")
			require.True(t, backuphelpers.IsBackupPending(backup), "Expected backup to be pending")
			require.Equal(t, backup.Status.NodeName, "test-node-2", "Expected backup to be assigned to an available node")
		},
	})
}

func TestBackupQueueAllNodesInUse(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending-1", testutils.WithBackupPending("test-node-1")),
			testutils.FakeEtcdBackup("pending-2", testutils.WithBackupPending("test-node-2")),
			testutils.FakeEtcdBackup("new")},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action")
		},
	})
}

func TestBackupQueueAlreadyPending(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node"))},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action")
		},
	})
}

func TestBackupQueueAlreadyPendingSameNode(t *testing.T) {
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"}}
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node"), testutils.WithBackupStorage(storage)),
			testutils.FakeEtcdBackup("new", testutils.WithBackupNodeName("test-node"), testutils.WithBackupStorage(storage))},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action")
		},
	})
}

func TestBackupQueueAlreadyPendingDifferentNodeSamePVC(t *testing.T) {
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"}}
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node-1"), testutils.WithBackupStorage(storage)),
			testutils.FakeEtcdBackup("new", testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupStorage(storage))},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action")
		},
	})
}

func TestBackupQueueAlreadyPendingDifferentNodeDifferentPVC(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node-1")),
			testutils.FakeEtcdBackup("new", testutils.WithBackupNodeName("test-node-2"))},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected update action")

			backup := action.Object.(*operatorv1alpha1.EtcdBackup)
			require.Equal(t, backup.Name, "new")
			require.True(t, backuphelpers.IsBackupPending(backup), "Expected backup to be pending")
		},
	})
}

func TestBackupQueueAlreadyPendingSameNodeLocal(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node-1"), testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
				Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
				Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"}})),
			testutils.FakeEtcdBackup("new", testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
				Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
				Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"}})),
		},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action")
		},
	})
}

func TestBackupQueueAlreadyPendingDifferentNodesLocal(t *testing.T) {
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
		Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"}}
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node-1"), testutils.WithBackupStorage(storage)),
			testutils.FakeEtcdBackup("new", testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupStorage(storage))},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected update action")

			backup := action.Object.(*operatorv1alpha1.EtcdBackup)
			require.Equal(t, backup.Name, "new")
			require.True(t, backuphelpers.IsBackupPending(backup), "Expected backup to be pending")
		},
	})
}

func TestBackupQueueStaleInformerCache(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node-1"))},
		staleBackupCache: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending")},
		populateActiveCache: true,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action")

			// Get request sent to verify mismatch between active cache and observed active backups from informer
			getAction, ok := testutils.GetStatusAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected get action")
			require.Equal(t, getAction.Name, "pending")
		},
	})
}

func TestBackupQueueSyncedInformerCache(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("pending", testutils.WithBackupPending("test-node"))},
		populateActiveCache: true,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			updateAction, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action, found %+v", updateAction)

			// No get request needed since active cache matched active backups observd from informer
			getAction, ok := testutils.GetStatusAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.Falsef(t, ok, "Expected no get action, found %+v", getAction)
		},
	})
}

func TestBackupQueueOrderByAge(t *testing.T) {
	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupAge(3*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupAge(2*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupAge(time.Hour)),
			testutils.FakeEtcdBackup("test-backup-4", testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupAge(0))},
		populateActiveCache: true,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			updateActions := testutils.ListStatusActions[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.Len(t, updateActions, 2, "Expected 2 update actions")

			var backup1, backup3 *operatorv1alpha1.EtcdBackup
			for _, action := range updateActions {
				backup := action.Object.(*operatorv1alpha1.EtcdBackup)
				switch backup.Name {
				case "test-backup-1":
					backup1 = backup
				case "test-backup-3":
					backup3 = backup
				default:
					require.FailNowf(t, "", "Didn't expect to see update for backup %s", backup.Name)
				}
				require.True(t, backuphelpers.IsBackupPending(backup), "Expected backup %s to be pending", backup.Name)
			}
			require.NotNil(t, backup1, "Expected to find update for test-backup-1")
			require.NotNil(t, backup3, "Expected to find update for test-backup-3")
			require.Equal(t, "test-node-1", backup1.Status.NodeName)
			require.Equal(t, "test-node-2", backup3.Status.NodeName)
		},
	})
}

func TestBackupQueueRemoveFinishedFromActive(t *testing.T) {
	backups := []*operatorv1alpha1.EtcdBackup{
		testutils.FakeEtcdBackup("completed", testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupCompleted()),
		testutils.FakeEtcdBackup("failed", testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupFailed()),
	}
	activeCache := newActiveBackupCache()
	for _, backup := range backups {
		activeCache.add(backup)
	}
	require.Len(t, activeCache.backups, 2, "Expected 2 active backups")
	require.Len(t, activeCache.nodes, 2, "Expected 2 nodes with active backups")
	require.Len(t, activeCache.pvcs, 2, "Expected 2 pvcs with active backups")
	for _, backup := range backups {
		require.Len(t, activeCache.nodes[backup.Status.NodeName], 1, "Expected 1 backup on node %s", backup.Status.NodeName)
		require.Len(t, activeCache.pvcs[backup.Spec.Storage.PVC.Name], 1, "Expected 1 backup on pvc %s", backup.Spec.Storage.PVC.Name)
	}

	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups:     backups,
		activeCache: &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			updateAction, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no update action, found %+v", updateAction)

			// No get request needed to remove inactive backups from the cache
			getAction, ok := testutils.GetStatusAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.Falsef(t, ok, "Expected no get action, found %+v", getAction)

			require.Len(t, activeCache.backups, 0)
			require.Len(t, activeCache.nodes, 0)
			require.Len(t, activeCache.pvcs, 0)
		},
	})
}

func TestBackupQueueRemoveDeletedFromActive(t *testing.T) {
	backup := testutils.FakeEtcdBackup("deleted", testutils.WithBackupPending("test-node-1"), testutils.WithBackupDeleted())
	activeCache := newActiveBackupCache()
	activeCache.add(backup)

	runBackupQueueControllerTest(t, testCaseBackupQueueController{
		backups:     []*operatorv1alpha1.EtcdBackup{},
		activeCache: &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Expect get request to verify backup is deleted before removing from backup cache
			action, ok := testutils.GetStatusAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected get action")
			require.Equal(t, backup.Name, action.Name)

			require.Len(t, activeCache.backups, 0)
			require.Len(t, activeCache.nodes, 0)
			require.Len(t, activeCache.pvcs, 0)
		},
	})
}
