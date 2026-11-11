package backupcontroller

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	fake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	clocktesting "k8s.io/utils/clock/testing"
)

type testCaseBackupGarbageCollectionController struct {
	backups     []*operatorv1alpha1.EtcdBackup
	jobs        []*batchv1.Job
	nodes       []*corev1.Node
	pvcs        []*corev1.PersistentVolumeClaim
	expectError bool
	retryQueue  *gcRetryQueue
	// operatorReactors are prepended to the fake operator client before the controller syncs
	operatorReactors []k8stesting.SimpleReactor
	validate         func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
}

func runBackupGarbageCollectionControllerTest(t *testing.T, tc testCaseBackupGarbageCollectionController) {
	t.Helper()
	operatorObjs := make([]runtime.Object, 0, len(tc.backups))
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backups)

	k8sObjs := make([]runtime.Object, 0, len(tc.jobs)+len(tc.nodes)+len(tc.pvcs))
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.jobs)
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.nodes)
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.pvcs)

	client := k8sfakeclient.NewSimpleClientset(k8sObjs...)
	operatorFake := fake.NewSimpleClientset(operatorObjs...)

	for _, reactor := range tc.operatorReactors {
		operatorFake.PrependReactor(reactor.Verb, reactor.Resource, reactor.Reaction)
	}

	sharedFactory := informers.NewSharedInformerFactory(client, 0)
	operatorSharedFactory := operatorinformers.NewSharedInformerFactory(operatorFake, 0)

	jobInformer := sharedFactory.Batch().V1().Jobs()
	nodesInformer := sharedFactory.Core().V1().Nodes()
	pvcsInformer := sharedFactory.Core().V1().PersistentVolumeClaims()
	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()

	jobInformerHasSynced := jobInformer.Informer().HasSynced
	nodesInformerHasSynced := nodesInformer.Informer().HasSynced
	pvcvsInformerHasSynced := pvcsInformer.Informer().HasSynced
	backupsInformerHasSynced := backupsInformer.Informer().HasSynced

	var retryQueue gcRetryQueue
	if tc.retryQueue != nil {
		retryQueue = *tc.retryQueue
	} else {
		retryQueue = newGCRetryQueue()
	}
	controller := BackupGarbageCollectionController{
		backupsLister:         backupsInformer.Lister(),
		jobsLister:            jobInformer.Lister().Jobs(operatorclient.TargetNamespace),
		nodesLister:           nodesInformer.Lister(),
		pvcsLister:            pvcsInformer.Lister().PersistentVolumeClaims(operatorclient.TargetNamespace),
		operatorClient:        operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
		retryQueue:            retryQueue,
	}
	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), jobInformerHasSynced, nodesInformerHasSynced, pvcvsInformerHasSynced, backupsInformerHasSynced)

	syncCtx := factory.NewSyncContext("backupGarbageCollectionController", events.NewInMemoryRecorder("test-job-controller", clocktesting.NewFakePassiveClock(time.Now())))
	err := controller.sync(ctx, syncCtx)
	if tc.expectError {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}

	if tc.validate != nil {
		tc.validate(t, syncCtx, client, operatorFake)
	}
}

func TestBackupGarbageCollectionNewJob(t *testing.T) {
	// Create a new GC Job for a completed/deleted EtcdBackup
	t.Run("local", func(t *testing.T) {
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup",
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
						Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etcdbackups"},
					}), testutils.WithBackupNodeName("test-node"), testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
			nodes: []*corev1.Node{testutils.FakeNode("test-node")},
			validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
					LabelSelector: labels.Set{"app": backupGCAppName}.String(),
				})
				require.NoError(t, err)
				require.Len(t, jobList.Items, 1)

				job := jobList.Items[0]
				require.Equal(t, job.Labels["app"], backupGCAppName)
				require.ElementsMatch(t, job.OwnerReferences, []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}})
				backupDir := "0/test-backup"
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
					Name:  backupGCFilesEnvName,
					Value: strings.Join([]string{backupDir + "/snapshot.db", backupDir + "/archive.tar.gz"}, ","),
				})
			},
		})
	})
	t.Run("pvc", func(t *testing.T) {
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
			pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
					LabelSelector: labels.Set{"app": backupGCAppName}.String(),
				})
				require.NoError(t, err)
				require.Len(t, jobList.Items, 1)

				job := jobList.Items[0]
				require.Equal(t, job.Labels["app"], backupGCAppName)
				require.ElementsMatch(t, job.OwnerReferences, []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}})
				backupDir := "test-backup"
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
					Name:  backupGCFilesEnvName,
					Value: strings.Join([]string{backupDir + "/snapshot.db", backupDir + "/archive.tar.gz"}, ","),
				})
			},
		})
	})
}

func TestBackupGarbageCollectionExistingJob(t *testing.T) {
	// Do not create a new GC job when one already exists for the EtcdBackup
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:       "existing-backup-gc-job",
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupGCAppName},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}}},
		}},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)

			job := jobList.Items[0]
			require.Equal(t, job.Name, "existing-backup-gc-job")
			require.Contains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
		},
	})
}

func TestBackupGarbageCollectionActiveJobOnStorageSkipsCreate(t *testing.T) {
	// Do not create a GC Job for a newly deleted EtcdBackup while another GC Job is active on the same storage backend
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"},
	}
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(storage)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(storage)),
		},
		pvcs: []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:       generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupGCAppName},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-1", UID: "test-backup-1-uid",
				}}},
		}},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			for _, action := range client.Actions() {
				require.False(t, action.Matches("create", "jobs"), "No GC Job should be created while one is active on the storage backend")
			}
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)
			require.ElementsMatch(t, jobList.Items[0].OwnerReferences, []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-1", UID: "test-backup-1-uid",
			}})
		},
	})
}

func TestBackupGarbageCollectionCompletedJobReplaced(t *testing.T) {
	// Replace a completed GC Job when another EtcdBackup on the same storage backend requires GC.
	// The completed job is finalized once, and not patched again before deletion.
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"},
	}
	jobName := generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"})
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(storage)),
		},
		pvcs: []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:       jobName,
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupGCAppName},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-1", UID: "test-backup-1-uid",
				}}},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}},
		}},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			patches := 0
			for _, action := range client.Actions() {
				if action.Matches("patch", "jobs") {
					patches++
				}
			}
			require.Equal(t, 1, patches, "Completed GC Job should only be finalized once")

			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)
			job := jobList.Items[0]
			require.False(t, isJobFinished(&job), "Completed job should have been replaced")
			require.ElementsMatch(t, job.OwnerReferences, []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-2", UID: "test-backup-2-uid",
			}})
		},
	})
}

func TestBackupGarbageCollectionFinalizeOnMissingStorageBackend(t *testing.T) {
	// Don't create GC Job when storage backend does not exist
	t.Run("local", func(t *testing.T) {
		// Node with local storage not found
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup",
					testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
						Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
						Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etcdbackups"},
					}), testutils.WithBackupNodeName("test-node"), testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
			validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
					LabelSelector: labels.Set{"app": backupGCAppName}.String(),
				})
				require.NoError(t, err)
				require.Len(t, jobList.Items, 0)

				backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "test-backup", v1.GetOptions{})
				require.NoError(t, err)
				require.NotContains(t, backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
			},
		})
	})
	t.Run("pvc", func(t *testing.T) {
		// PVC storage not found
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
			validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
					LabelSelector: labels.Set{"app": backupGCAppName}.String(),
				})
				require.NoError(t, err)
				require.Len(t, jobList.Items, 0)

				backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "test-backup", v1.GetOptions{})
				require.NoError(t, err)
				require.NotContains(t, backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
			},
		})
	})
}

func TestBackupGarbageCollectionMultipleBackupsPerJob(t *testing.T) {
	// Combine muliple deleted EtcdBackups on the same sharedStorage backend into one GC Job
	sharedStorage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC: operatorv1alpha1.EtcdBackupStoragePvc{
			Name: "test-backup-pvc",
		},
	}
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(sharedStorage)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(sharedStorage)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(),
				testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
					PVC: operatorv1alpha1.EtcdBackupStoragePvc{
						Name: "test-different-pvc",
					},
				})),
		},
		pvcs: []*corev1.PersistentVolumeClaim{
			testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc"),
			testutils.FakePVC(operatorclient.TargetNamespace, "test-different-pvc"),
		},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 2)
			job1, job2 := jobList.Items[0], jobList.Items[1]
			if len(job1.OwnerReferences) == 1 {
				job1, job2 = job2, job1
			}

			require.ElementsMatch(t,
				[]v1.OwnerReference{
					{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-1", UID: "test-backup-1-uid"},
					{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-2", UID: "test-backup-2-uid"},
				},
				job1.OwnerReferences,
			)
			require.ElementsMatch(t,
				[]v1.OwnerReference{
					{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-3", UID: "test-backup-3-uid"},
				},
				job2.OwnerReferences,
			)

			backupDir1 := "test-backup-1"
			backupDir2 := "test-backup-2"
			backupDir3 := "test-backup-3"
			require.Equal(t,
				corev1.EnvVar{Name: backupGCFilesEnvName, Value: strings.Join([]string{
					backupDir1 + "/snapshot.db", backupDir1 + "/archive.tar.gz",
					backupDir2 + "/snapshot.db", backupDir2 + "/archive.tar.gz"}, ",")},
				requireFind(t, job1.Spec.Template.Spec.Containers[0].Env, func(env corev1.EnvVar) bool {
					return env.Name == backupGCFilesEnvName
				}),
			)
			require.Equal(t,
				corev1.EnvVar{Name: backupGCFilesEnvName, Value: strings.Join([]string{backupDir3 + "/snapshot.db", backupDir3 + "/archive.tar.gz"}, ",")},
				requireFind(t, job2.Spec.Template.Spec.Containers[0].Env, func(env corev1.EnvVar) bool {
					return env.Name == backupGCFilesEnvName
				}),
			)
		},
	})
}

func TestBackupGarbageCollectionFailedBackupFinalizedWithoutGC(t *testing.T) {
	validate := func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
		jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
			LabelSelector: labels.Set{"app": backupGCAppName}.String(),
		})
		require.NoError(t, err)
		require.Len(t, jobList.Items, 0)

		backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "test-backup", v1.GetOptions{})
		require.NoError(t, err)
		require.Empty(t, backup.Finalizers)
	}

	t.Run("no files", func(t *testing.T) {
		// Finalize a failed EtcdBackup without creating a GC Job
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups:  []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupFailed(operatorv1alpha1.BackupReasonSetupFailed), testutils.WithBackupDeleted())},
			pvcs:     []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: validate,
		})
	})
	t.Run("deleted node", func(t *testing.T) {
		// Finalize a successful Local EtcdBackup that references a deleted node
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
				Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
				Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/backups"},
			}))},
			validate: validate,
		})
	})
	t.Run("deleted pvc", func(t *testing.T) {
		// Finalize a successful EtcdBackup that references a deleted PVC
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups:  []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
			validate: validate,
		})
	})
}

func TestBackupGarbageCollectionFailedBackupRequireGC(t *testing.T) {
	validate := func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
		jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
			LabelSelector: labels.Set{"app": backupGCAppName}.String(),
		})
		require.NoError(t, err)
		require.Len(t, jobList.Items, 1)

		backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "test-backup", v1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
	}

	t.Run("files partially created", func(t *testing.T) {
		// Create GC job for failed EtcdBackup that created some files but didn't complete
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed), testutils.WithBackupDeleted(), func(backup *operatorv1alpha1.EtcdBackup) {
				backup.Status.Files = []operatorv1alpha1.EtcdBackupFile{{
					Path: "/backups/backup.db.part", SizeBytes: 100,
				}}
			})},
			pvcs:     []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: validate,
		})
	})

	t.Run("files state unknown", func(t *testing.T) {
		// Create GC job for failed EtcdBackup that created some files but didn't complete
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups:  []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed), testutils.WithBackupDeleted())},
			pvcs:     []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: validate,
		})
	})

	t.Run("no gc condition", func(t *testing.T) {
		// Create GC job for failed EtcdBackup that created some files but didn't complete
		runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
			backups:  []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed), testutils.WithBackupDeleted())},
			pvcs:     []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: validate,
		})
	})
}

func TestBackupGarbageCollectionActiveBackupsIgnored(t *testing.T) {
	// Backups that haven't been deleted are ignored by the garbage collection controller
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-new-backup"),
			testutils.FakeEtcdBackup("test-running-backup", testutils.WithBackupRunning(&batchv1.Job{ObjectMeta: v1.ObjectMeta{Name: "test-running-backup"}})),
			testutils.FakeEtcdBackup("test-completed-backup", testutils.WithBackupCompleted()),
			testutils.FakeEtcdBackup("test-failed-backup", testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed)),
		},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 0)

			_, ok := testutils.GetAction[k8stesting.PatchActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no patch actions")

			backupList, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(t.Context(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backupList.Items, 4)
			for _, backup := range backupList.Items {
				require.Nil(t, backup.DeletionTimestamp)
				if backuphelpers.IsBackupRunning(&backup) || backuphelpers.IsBackupFinished(&backup) {
					require.Contains(t, backup.Finalizers, backuphelpers.FinalizerEtcdBackup, "Expected backup %q to have finalizer", backup.Name)
				}
			}
		},
	})
}

func TestBackupGarbageCollectionFailedJobRetry(t *testing.T) {
	// Retry a failed GC Job
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:       generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupGCAppName},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}},
				UID:               "failed-gc-job-uid",
				CreationTimestamp: v1.Time{Time: time.Now().Add(-time.Hour)},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
			}}},
		}},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)

			job := jobList.Items[0]
			require.ElementsMatch(t, job.OwnerReferences, []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
			}})
			require.False(t, isJobFailed(&job), "Job should have been replaced")
		},
	})
}

func TestBackupGarbageCollectionFailedJobBackoff(t *testing.T) {
	// Use exponential backoff when retrying failed GC Jobs
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:       generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupGCAppName},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}},
				UID:               "failed-gc-job-uid",
				CreationTimestamp: v1.Now(),
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
			}}},
		}},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)

			job := jobList.Items[0]
			require.ElementsMatch(t, job.OwnerReferences, []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
			}})
			require.True(t, isJobFailed(&job), "Job should not have been replaced yet, expected to wait for backoff")
			require.Contains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup, "Failed job awaiting retry should keep its finalizer")
		},
	})
}

func TestBackupGarbageCollectionCompletedJobsFinalized(t *testing.T) {
	// Finalize GC Jobs that have completed, and failed GC Jobs without any backups that still require GC, since they
	// will never be retried
	retryQueue := newGCRetryQueue()
	retryQueue.When("failed-backup-gc")
	retryQueue.backoffs["failed-backup-gc"] = gcBackoff{retryAt: time.Now()}
	retryQueue.When("completed-backup-gc")
	retryQueue.backoffs["completed-backup-gc"] = gcBackoff{retryAt: time.Now()}
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		retryQueue: &retryQueue,
		jobs: []*batchv1.Job{
			{
				ObjectMeta: v1.ObjectMeta{
					Name:       "failed-backup-gc-job",
					Namespace:  operatorclient.TargetNamespace,
					Labels:     map[string]string{"app": backupGCAppName},
					Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
					OwnerReferences: []v1.OwnerReference{{
						Kind: "EtcdBackup", UID: "failed-backup-gc",
					}},
				},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				}}},
			},
			{
				ObjectMeta: v1.ObjectMeta{
					Name:       "completed-backup-gc-job",
					Namespace:  operatorclient.TargetNamespace,
					Labels:     map[string]string{"app": backupGCAppName},
					Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
					OwnerReferences: []v1.OwnerReference{{
						Kind: "EtcdBackup", UID: "completed-backup-gc",
					}},
				},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				}}},
			},
		},
		pvcs: []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Backoff is reset for storage backends with a completed job, or when backup no longer exists
			require.Zero(t, retryQueue.NumRequeues("completed-backup-gc"))
			require.NotContains(t, retryQueue.backoffs, "completed-backup-gc")
			require.Zero(t, retryQueue.NumRequeues("failed-backup-gc"))
			require.NotContains(t, retryQueue.backoffs, "failed-backup-gc")

			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 2)
			for _, job := range jobList.Items {
				require.NotContains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup, "Expected job %q to be finalized", job.Name)
			}
		},
	})
}

func TestBackupGarbageCollectionCompletedJobKeptUntilFinalizersRemoved(t *testing.T) {
	// GC completed for a backup, but removing its finalizer fails. The completed job must keep its owner references
	// (and must not be replaced by a new job), otherwise the backup would be garbage collected again.
	collected := testutils.FakeEtcdBackup("collected", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(),
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-pvc"},
		}))
	queued := testutils.FakeEtcdBackup("queued", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(),
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
			Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
			PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-pvc"},
		}))
	completedJob := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			// GC jobs are looked up by their deterministic per-storage name
			Name:       generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-pvc"}),
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupGCAppName},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: collected.Name, UID: collected.UID,
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}

	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{collected, queued},
		jobs:    []*batchv1.Job{completedJob},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-pvc")},
		operatorReactors: []k8stesting.SimpleReactor{{
			Verb:     "update",
			Resource: "etcdbackups",
			Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("injected finalizer removal failure")
			},
		}},
		expectError: true,
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(t.Context(), completedJob.Name, v1.GetOptions{})
			require.NoError(t, err)
			require.Contains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
			require.Equal(t, completedJob.OwnerReferences, job.OwnerReferences)

			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1, "expected completed job to not be replaced")
		},
	})
}

func TestBackupGarbageCollectionCompletedJobKeptUntilFinalizerRemovalObserved(t *testing.T) {
	// GC completed and the backup finalizer is removed, but the informer has not observed the removal yet. The
	// completed job must keep its owner references until it does, otherwise a stale backup would be collected again.
	// The sync is requeued to finalize the job once the removal is observed.
	defaultDelay := gcFinalizeRequeueDelay
	gcFinalizeRequeueDelay = 100 * time.Millisecond
	t.Cleanup(func() { gcFinalizeRequeueDelay = defaultDelay })
	collected := testutils.FakeEtcdBackup("collected", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())
	completedJob := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "completed-backup-gc-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupGCAppName},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: collected.Name, UID: collected.UID,
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{collected},
		jobs:    []*batchv1.Job{completedJob},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), collected.Name, v1.GetOptions{})
			require.NoError(t, err)
			require.NotContains(t, backup.Finalizers, backuphelpers.FinalizerEtcdBackup)

			job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(t.Context(), completedJob.Name, v1.GetOptions{})
			require.NoError(t, err)
			require.Contains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
			require.Equal(t, completedJob.OwnerReferences, job.OwnerReferences)

			require.Eventually(t, func() bool { return syncCtx.Queue().Len() == 1 }, time.Second, 10*time.Millisecond,
				"expected sync to be requeued")
		},
	})
}

func TestBackupGarbageCollectionBackoffRetry(t *testing.T) {
	// Backups in a failed GC job back off, while new GC on the same storage backend proceeds without them
	storage := testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"},
	})
	completedGC := testutils.FakeEtcdBackup("completed-gc", storage, testutils.WithBackupCompleted(), testutils.WithBackupDeleted())
	failedGC := testutils.FakeEtcdBackup("failed-gc", storage, testutils.WithBackupCompleted(), testutils.WithBackupDeleted())
	newGC := testutils.FakeEtcdBackup("new-gc", storage, testutils.WithBackupCompleted(), testutils.WithBackupDeleted())
	completedJob := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "completed-backup-gc-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupGCAppName},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: completedGC.Name, UID: completedGC.UID,
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: v1.Now(),
		}}},
	}
	failedJob := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "failed-backup-gc-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupGCAppName},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: failedGC.Name, UID: failedGC.UID,
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: v1.Now(),
		}}},
	}
	retryQueue := newGCRetryQueue()
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups:    []*operatorv1alpha1.EtcdBackup{completedGC, failedGC, newGC},
		jobs:       []*batchv1.Job{completedJob, failedJob},
		pvcs:       []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		retryQueue: &retryQueue,
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			completedGC, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), completedGC.Name, v1.GetOptions{})
			require.NoError(t, err)
			require.NotContains(t, completedGC.Finalizers, backuphelpers.FinalizerEtcdBackup)

			failedGC, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), failedGC.Name, v1.GetOptions{})
			require.NoError(t, err)
			require.Contains(t, failedGC.Finalizers, backuphelpers.FinalizerEtcdBackup)

			newGC, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), newGC.Name, v1.GetOptions{})
			require.NoError(t, err)
			require.Contains(t, newGC.Finalizers, backuphelpers.FinalizerEtcdBackup)

			require.Zero(t, retryQueue.NumRequeues(completedGC.UID))
			require.Zero(t, retryQueue.NumRequeues(newGC.UID))
			require.Equal(t, 1, retryQueue.NumRequeues(failedGC.UID))
			require.Greater(t, retryQueue.backoffDuration(failedGC.UID), time.Duration(0))

			action, ok := testutils.GetAction[k8stesting.CreateActionImpl](client.Fake.Actions())
			require.True(t, ok, "expected create action")
			job, ok := action.Object.(*batchv1.Job)
			require.True(t, ok, "expected job object")
			require.Len(t, job.OwnerReferences, 1)
			require.Equal(t, "EtcdBackup", job.OwnerReferences[0].Kind)
			require.Equal(t, newGC.Name, job.OwnerReferences[0].Name)
		},
	})
}

func TestGCRetryQueueBackoff(t *testing.T) {
	c := &BackupGarbageCollectionController{retryQueue: newGCRetryQueue()}
	owners := func(uids ...types.UID) []v1.OwnerReference {
		var refs []v1.OwnerReference
		for _, uid := range uids {
			refs = append(refs, v1.OwnerReference{Kind: "EtcdBackup", Name: string(uid), UID: uid})
		}
		return refs
	}
	now := time.Now()

	require.Zero(t, c.retryQueue.backoffDuration("a"), "backups without failed GC do not back off")

	// A failed job backs off every backup it owns
	delay := c.backoffFailedBackupGC("job-1", now, owners("a", "b"))
	require.InDelta(t, gcBackoffBase, delay, float64(time.Second))
	require.Equal(t, 1, c.retryQueue.NumRequeues("a"))
	require.Equal(t, 1, c.retryQueue.NumRequeues("b"))
	require.InDelta(t, gcBackoffBase, c.retryQueue.backoffDuration("a"), float64(time.Second))

	// The same failed job observed again is only counted once
	require.Zero(t, c.backoffFailedBackupGC("job-1", now, owners("a", "b")))
	require.Equal(t, 1, c.retryQueue.NumRequeues("a"))

	// A new failed job escalates only the backups it owns
	delay = c.backoffFailedBackupGC("job-2", now, owners("a"))
	require.InDelta(t, 2*gcBackoffBase, delay, float64(time.Second))
	require.Equal(t, 2, c.retryQueue.NumRequeues("a"))
	require.Equal(t, 1, c.retryQueue.NumRequeues("b"))

	// Non-backup owner references are ignored
	c.backoffFailedBackupGC("job-3", now, []v1.OwnerReference{{Kind: "Other", UID: "other"}})
	require.NotContains(t, c.retryQueue.backoffs, types.UID("other"))

	// Backoff is measured from when the job failed
	c.backoffFailedBackupGC("job-4", now.Add(-time.Hour), owners("c"))
	require.Equal(t, 1, c.retryQueue.NumRequeues("c"))
	require.LessOrEqual(t, c.retryQueue.backoffDuration("c"), time.Duration(0))

	c.retryQueue.Forget("a")
	require.Zero(t, c.retryQueue.NumRequeues("a"))
	require.NotContains(t, c.retryQueue.backoffs, types.UID("a"))
	require.Contains(t, c.retryQueue.backoffs, types.UID("b"))
}

func TestFilterBackupsForGC(t *testing.T) {
	now := time.Now()
	backup := func(name string, deletedAgo time.Duration) *operatorv1alpha1.EtcdBackup {
		b := testutils.FakeEtcdBackup(name, testutils.WithBackupCompleted(), testutils.WithBackupDeleted())
		b.DeletionTimestamp = &v1.Time{Time: now.Add(-deletedAgo)}
		return b
	}
	// failures records previous failed GC per backup: number of failures, and whether the backoff has expired
	type failure struct {
		count   int
		expired bool
	}

	tests := []struct {
		name     string
		backups  []*operatorv1alpha1.EtcdBackup
		failures map[string]failure
		expected []string
	}{
		{
			name:     "new backups batched oldest deletion first, then by name",
			backups:  []*operatorv1alpha1.EtcdBackup{backup("c", time.Minute), backup("b", time.Hour), backup("a", time.Minute)},
			expected: []string{"b", "a", "c"},
		},
		{
			name:     "new backups batched without previously failed backups",
			backups:  []*operatorv1alpha1.EtcdBackup{backup("failed", time.Hour), backup("new-1", time.Minute), backup("new-2", time.Minute)},
			failures: map[string]failure{"failed": {count: 1, expired: true}},
			expected: []string{"new-1", "new-2"},
		},
		{
			name:     "previously failed backups retried individually",
			backups:  []*operatorv1alpha1.EtcdBackup{backup("failed-1", time.Minute), backup("failed-2", time.Hour)},
			failures: map[string]failure{"failed-1": {count: 1, expired: true}, "failed-2": {count: 1, expired: true}},
			expected: []string{"failed-2"},
		},
		{
			name:     "fewest failures retried first",
			backups:  []*operatorv1alpha1.EtcdBackup{backup("failed-twice", time.Hour), backup("failed-once", time.Minute)},
			failures: map[string]failure{"failed-twice": {count: 2, expired: true}, "failed-once": {count: 1, expired: true}},
			expected: []string{"failed-once"},
		},
		{
			name:     "backups in backoff excluded",
			backups:  []*operatorv1alpha1.EtcdBackup{backup("waiting", time.Hour), backup("ready", time.Minute)},
			failures: map[string]failure{"waiting": {count: 1}, "ready": {count: 3, expired: true}},
			expected: []string{"ready"},
		},
		{
			name:     "all backups in backoff",
			backups:  []*operatorv1alpha1.EtcdBackup{backup("waiting", time.Hour)},
			failures: map[string]failure{"waiting": {count: 1}},
			expected: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &BackupGarbageCollectionController{retryQueue: newGCRetryQueue()}
			for name, f := range tt.failures {
				uid := types.UID(name + "-uid")
				for range f.count {
					c.retryQueue.When(uid)
				}
				retryAt := now.Add(time.Hour)
				if f.expired {
					retryAt = now.Add(-time.Second)
				}
				c.retryQueue.backoffs[uid] = gcBackoff{retryAt: retryAt}
			}

			var names []string
			for _, b := range c.filterBackupsForGC(tt.backups) {
				names = append(names, b.Name)
			}
			require.Equal(t, tt.expected, names)
		})
	}

	t.Run("new backups capped per job", func(t *testing.T) {
		c := &BackupGarbageCollectionController{retryQueue: newGCRetryQueue()}
		var backups []*operatorv1alpha1.EtcdBackup
		for i := range gcMaxBackupsPerJob + 5 {
			backups = append(backups, backup(fmt.Sprintf("backup-%03d", i), time.Minute))
		}
		filtered := c.filterBackupsForGC(backups)
		require.Len(t, filtered, gcMaxBackupsPerJob)
		require.Equal(t, "backup-000", filtered[0].Name)
	})
}

func TestBackupGarbageCollectionDefaultHostPath(t *testing.T) {
	// Local backups without a hostPath are stored under the default hostPath, and GC must resolve the same location
	backup := testutils.FakeEtcdBackup("test-backup",
		testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{Type: operatorv1alpha1.EtcdBackupStorageTypeLocal}),
		testutils.WithBackupNodeName("test-node"), testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed), testutils.WithBackupDeleted())
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node")},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)
			podSpec := jobList.Items[0].Spec.Template.Spec

			volume := requireFind(t, podSpec.Volumes, func(v corev1.Volume) bool { return v.HostPath != nil })
			require.Equal(t, backuphelpers.BackupPathMount, volume.HostPath.Path)
			mount := requireFind(t, podSpec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volume.Name })
			require.Equal(t, filepath.Join(backuphelpers.BackupPathMount, "0"), mount.MountPath)
			require.Contains(t, podSpec.Containers[0].Env, corev1.EnvVar{
				Name:  backupGCFilesEnvName,
				Value: "0/test-backup",
			})
		},
	})
}

func TestBackupFileMounts(t *testing.T) {
	// Generate backups with files such that 5 backups will fill the max arg length
	numBackups := 10
	filePathLen := gcMaxFileArgBytes/5 - 20
	backups := make([]*operatorv1alpha1.EtcdBackup, numBackups)
	for i := range backups {
		backups[i] = testutils.FakeEtcdBackup(fmt.Sprintf("test-backup-%d", i), func(backup *operatorv1alpha1.EtcdBackup) {
			backup.Spec.Storage = operatorv1alpha1.EtcdBackupStorage{Type: operatorv1alpha1.EtcdBackupStorageTypeLocal, Local: operatorv1alpha1.EtcdBackupStorageLocal{
				HostPath: fmt.Sprintf("/hostpath-%d", i%2),
			}}
			backup.Status.NodeName = "test-node"
			backup.Status.Files = []operatorv1alpha1.EtcdBackupFile{
				{AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot, Path: strings.Repeat("a", filePathLen)},
			}
		})
	}
	backups, env, volumes, volumeMounts := backupFileMounts(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypeLocal, nodeName: "test-node"}, backups)
	require.Len(t, backups, 5)
	require.LessOrEqual(t, len(env.Value), gcMaxFileArgBytes)
	// Host paths are split into 2 groups, so there should be 2 volumes/volumeMounts
	require.Len(t, volumes, 2)
	require.Len(t, volumeMounts, 2)

	for i, backup := range backups {
		volumeIdx := i % 2
		require.Equal(t, backup.Spec.Storage.Local.HostPath, volumes[volumeIdx].HostPath.Path)
		require.Equal(t, fmt.Sprintf("etc-kubernetes-cluster-backup-%d", volumeIdx), volumes[volumeIdx].Name)
		require.Equal(t, fmt.Sprintf("etc-kubernetes-cluster-backup-%d", volumeIdx), volumeMounts[volumeIdx].Name)
		require.Equal(t, filepath.Join(backuphelpers.BackupPathMount, strconv.Itoa(volumeIdx)), volumeMounts[volumeIdx].MountPath)
	}
}

func requireFind[T any](t *testing.T, items []T, find func(T) bool) (result T) {
	for _, item := range items {
		if ok := find(item); ok {
			return item
		}
	}
	require.Fail(t, "Unable to find expected item")
	return
}
