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
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	clocktesting "k8s.io/utils/clock/testing"
)

type testCaseBackupGarbageCollectionController struct {
	backups     []*operatorv1alpha1.EtcdBackup
	jobs        []*batchv1.Job
	nodes       []*corev1.Node
	pvcs        []*corev1.PersistentVolumeClaim
	expectError bool
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

	controller := BackupGarbageCollectionController{
		backupsLister:         backupsInformer.Lister(),
		jobsLister:            jobInformer.Lister().Jobs(operatorclient.TargetNamespace),
		nodesLister:           nodesInformer.Lister(),
		pvcsLister:            pvcsInformer.Lister().PersistentVolumeClaims(operatorclient.TargetNamespace),
		operatorClient:        operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
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
						Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etcdbackups"},
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
					Name:  backupGcFilesEnvName,
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
					Name:  backupGcFilesEnvName,
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

func TestBackupGarbageCollectionNewAndExistingJob(t *testing.T) {
	// Create a new GC Job for the newly deleted EtcdBackup, but not for the one with an existing GC Job
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"},
	}
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(storage)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupCompleted(), testutils.WithBackupDeleted(), testutils.WithBackupStorage(storage)),
		},
		pvcs: []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		jobs: []*batchv1.Job{{
			ObjectMeta: v1.ObjectMeta{
				Name:        "existing-backup-gc-job",
				Namespace:   operatorclient.TargetNamespace,
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-1", UID: "test-backup-1-uid",
				}}},
		}},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 2)
			require.ElementsMatch(t,
				[][]v1.OwnerReference{
					{{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-1", UID: "test-backup-1-uid"}},
					{{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup-2", UID: "test-backup-2-uid"}},
				},
				[][]v1.OwnerReference{jobList.Items[0].OwnerReferences, jobList.Items[1].OwnerReferences})
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
						Local: &operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etcdbackups"},
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
		PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
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
					PVC: &operatorv1alpha1.EtcdBackupStoragePvc{
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
				corev1.EnvVar{Name: backupGcFilesEnvName, Value: strings.Join([]string{backupDir1 + "snapshot.db", backupDir1 + "archive.tar.gz", backupDir2 + "snapshot.db", backupDir2 + "archive.tar.gz"}, " ")},
				requireFind(t, job1.Spec.Template.Spec.Containers[0].Env, func(env corev1.EnvVar) bool {
					return env.Name == backupGcFilesEnvName
				}),
			)
			require.Equal(t,
				corev1.EnvVar{Name: backupGcFilesEnvName, Value: strings.Join([]string{backupDir3 + "snapshot.db", backupDir3 + "archive.tar.gz"}, " ")},
				requireFind(t, job2.Spec.Template.Spec.Containers[0].Env, func(env corev1.EnvVar) bool {
					return env.Name == backupGcFilesEnvName
				}),
			)
		},
	})
}

func TestBackupGarbageCollectionFailedBackupFinalized(t *testing.T) {
	// Finalize a failed EtcdBackup without creating a GC Job
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupFailed(), testutils.WithBackupDeleted())},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 0)

			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "test-backup", v1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, backup.Finalizers)
		},
	})
}

func TestBackupGarbageCollectionActiveBackupsIgnored(t *testing.T) {
	// Backups that haven't been deleted are ignored by the garbage collection controller
	runBackupGarbageCollectionControllerTest(t, testCaseBackupGarbageCollectionController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-pending-backup"),
			testutils.FakeEtcdBackup("test-completed-backup", testutils.WithBackupCompleted()),
			testutils.FakeEtcdBackup("test-failed-backup", testutils.WithBackupFailed()),
		},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobList, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).List(t.Context(), v1.ListOptions{
				LabelSelector: labels.Set{"app": backupGCAppName}.String(),
			})
			require.NoError(t, err)
			require.Len(t, jobList.Items, 0)

			backupList, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(t.Context(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backupList.Items, 3)
			for _, backup := range backupList.Items {
				require.Contains(t, backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
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
				Annotations: map[string]string{backuphelpers.AnnotationBackupStorage: "pvc/test-backup-pvc", backuphelpers.AnnotationBackupGCRetry: "5"},
				Labels:      map[string]string{"app": backupGCAppName},
				Finalizers:  []string{backuphelpers.FinalizerEtcdBackup},
				OwnerReferences: []v1.OwnerReference{{
					APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: "test-backup", UID: "test-backup-uid",
				}},
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

func TestBackupGarbageCollectionCompletedJobsFinalized(t *testing.T) {
	// Finalize GC Jobs that have completed
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
					Name:       "completed-backup-gc-job",
					Namespace:  operatorclient.TargetNamespace,
					Labels:     map[string]string{"app": backupGCAppName},
					Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
				},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				}}},
			},
		},
		validate: func(t *testing.T, syncCtx factory.SyncContext, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
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

func requireFind[T any](t *testing.T, items []T, find func(T) bool) (result T) {
	for _, item := range items {
		if ok := find(item); ok {
			return item
		}
	}
	require.Fail(t, "Unable to find expected item")
	return
}
