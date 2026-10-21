package backupcontroller

import (
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
	validate    func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
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
				backupDir := backupPathMount + "/etcdbackups/test-backup/"
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
					Name:  backupGCFilesEnvName,
					Value: strings.Join([]string{backupDir + "snapshot.db", backupDir + "archive.tar.gz"}, " "),
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
				backupDir := backupPathMount + "/test-backup/"
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
					Name:  backupGCFilesEnvName,
					Value: strings.Join([]string{backupDir + "snapshot.db", backupDir + "archive.tar.gz"}, " "),
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
				Name:        "existing-backup-gc-job",
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
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
				Name:        generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
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
				Name:        jobName,
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
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

			backupDir1 := backupPathMount + "/test-backup-1/"
			backupDir2 := backupPathMount + "/test-backup-2/"
			backupDir3 := backupPathMount + "/test-backup-3/"
			require.Equal(t,
				corev1.EnvVar{Name: backupGCFilesEnvName, Value: strings.Join([]string{backupDir1 + "snapshot.db", backupDir1 + "archive.tar.gz", backupDir2 + "snapshot.db", backupDir2 + "archive.tar.gz"}, " ")},
				requireFind(t, job1.Spec.Template.Spec.Containers[0].Env, func(env corev1.EnvVar) bool {
					return env.Name == backupGCFilesEnvName
				}),
			)
			require.Equal(t,
				corev1.EnvVar{Name: backupGCFilesEnvName, Value: strings.Join([]string{backupDir3 + "snapshot.db", backupDir3 + "archive.tar.gz"}, " ")},
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
				Name:        generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
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
				Name:        generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
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
		},
	})
}

func TestBackupGarbageCollectionFailedJobBackoffEscalates(t *testing.T) {
	// Previous failed jobs on this storage backend
	retryQueue := newGCRetryQueue()
	for range 3 {
		retryQueue.When("pvc/test-backup-pvc")
	}
	retryQueue.backoffs["pvc/test-backup-pvc"] = gcBackoff{jobUID: "failed-gc-job-uid-1"}

	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupCompleted(), testutils.WithBackupDeleted())},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:        generateBackupGCJobName(storageBackend{storageType: operatorv1alpha1.EtcdBackupStorageTypePVC, pvcName: "test-backup-pvc"}),
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}},
				UID:               "failed-gc-job-uid-2",
				CreationTimestamp: v1.Time{Time: time.Now().Add(-2 * gcBackoffBase)},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue,
			}}},
		}},
		retryQueue: &retryQueue,
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 1)
			require.True(t, isJobFailed(&jobList.Items[0]), "Job should not have been replaced yet, expected to wait for backoff")
			require.Equal(t, 4, retryQueue.NumRequeues("pvc/test-backup-pvc"))
			require.Equal(t, types.UID("failed-gc-job-uid-2"), retryQueue.backoffs["pvc/test-backup-pvc"].jobUID)
		},
	})
}

func TestBackupGarbageCollectionCompletedJobsFinalized(t *testing.T) {
	// Finalize GC Jobs that have completed
	retryQueue := newGCRetryQueue()
	retryQueue.When("pvc/completed")
	retryQueue.backoffs["pvc/completed"] = gcBackoff{jobUID: "old-failed-job-uid"}
	retryQueue.When("pvc/missing")
	retryQueue.backoffs["pvc/missing"] = gcBackoff{jobUID: "old-failed-job-uid"}
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		jobs: []*batchv1.Job{
			{
				ObjectMeta: v1.ObjectMeta{
					Name:        "failed-backup-gc-job",
					Namespace:   operatorclient.TargetNamespace,
					Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
					Labels:      map[string]string{"app": backupGCAppName},
					Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
				},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				}}},
			},
			{
				ObjectMeta: v1.ObjectMeta{
					Name:        "completed-backup-gc-job",
					Namespace:   operatorclient.TargetNamespace,
					Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/completed"},
					Labels:      map[string]string{"app": backupGCAppName},
					Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
				},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				}}},
			},
		},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Backoff is reset for storage backends with a completed job, or without any job
			require.Zero(t, retryQueue.NumRequeues("pvc/completed"))
			require.NotContains(t, retryQueue.backoffs, "pvc/completed")
			require.Zero(t, retryQueue.NumRequeues("pvc/missing"))
			require.NotContains(t, retryQueue.backoffs, "pvc/missing")

			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 2)
			for _, job := range jobList.Items {
				if isJobCompleted(&job) {
					require.NotContains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
				} else {
					require.True(t, isJobFailed(&job), "Expected job to be failed")
					require.Contains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
				}
			}
		},
	})
}

func TestGCJobBackoff(t *testing.T) {
	c := &BackupGarbageCollectionController{retryQueue: newGCRetryQueue()}
	failedJob := func(uid types.UID, failedAt time.Time) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: v1.ObjectMeta{UID: uid},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: v1.Time{Time: failedAt},
			}}},
		}
	}
	completedJob := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}}}

	now := time.Now()
	require.Zero(t, c.retryQueue.backoffDuration("local/a", completedJob), "completed jobs do not back off")
	require.Zero(t, c.retryQueue.NumRequeues("local/a"))

	// Same failed job observed repeatedly is only counted once
	job1 := failedJob("job-1", now)
	first := c.retryQueue.backoffDuration("local/a", job1)
	require.InDelta(t, gcBackoffBase, first, float64(time.Second))
	require.LessOrEqual(t, c.retryQueue.backoffDuration("local/a", job1), first)
	require.Equal(t, 1, c.retryQueue.NumRequeues("local/a"))

	// A new failed job on the same storage backend escalates
	require.InDelta(t, 2*gcBackoffBase, c.retryQueue.backoffDuration("local/a", failedJob("job-2", now)), float64(time.Second))
	require.Equal(t, 2, c.retryQueue.NumRequeues("local/a"))

	// Storage backends back off independently
	require.InDelta(t, gcBackoffBase, c.retryQueue.backoffDuration("local/b", failedJob("job-3", now)), float64(time.Second))

	// Backoff is measured from when the job failed
	require.LessOrEqual(t, c.retryQueue.backoffDuration("local/c", failedJob("job-4", now.Add(-time.Hour))), time.Duration(0))

	c.retryQueue.Forget("local/a")
	require.Zero(t, c.retryQueue.NumRequeues("local/a"))
	require.NotContains(t, c.retryQueue.backoffs, "local/a")
	require.Contains(t, c.retryQueue.backoffs, "local/b")
}

func TestJobFailedTime(t *testing.T) {
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	failed := time.Now().Truncate(time.Second)

	job := &batchv1.Job{ObjectMeta: v1.ObjectMeta{CreationTimestamp: v1.Time{Time: created}}}
	require.Equal(t, created, jobFailedTime(job), "falls back to creation time without failed condition")

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	require.Equal(t, created, jobFailedTime(job), "falls back to creation time without transition time")

	job.Status.Conditions[0].LastTransitionTime = v1.Time{Time: failed}
	require.Equal(t, failed, jobFailedTime(job))
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
