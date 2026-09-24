package backupcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

var backupFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
	[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName},
	[]configv1.FeatureGateName{})

var localStorage = operatorv1alpha1.EtcdBackupStorage{
	Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
	Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/backups"},
}

type testCaseBackupController struct {
	backups []*operatorv1alpha1.EtcdBackup
	// staleBackups, if set, populates the backup informer instead of backups to simulate informer lag
	staleBackups []*operatorv1alpha1.EtcdBackup
	jobs         []*batchv1.Job
	pods         []*corev1.Pod
	nodes        []*corev1.Node
	pvcs         []*corev1.PersistentVolumeClaim
	activeCache  *activeBackupCache
	expectError  bool
	validate     func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
}

func runBackupControllerTest(t *testing.T, tc testCaseBackupController) {
	t.Helper()
	operatorObjs := make([]runtime.Object, 0, len(tc.backups))
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backups)

	k8sObjs := make([]runtime.Object, 0, len(tc.jobs)+len(tc.pods)+len(tc.nodes)+len(tc.pvcs))
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.jobs)
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.pods)
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.nodes)
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.pvcs)

	client := k8sfakeclient.NewSimpleClientset(k8sObjs...)
	operatorFake := operatorfake.NewSimpleClientset(operatorObjs...)

	sharedFactory := informers.NewSharedInformerFactory(client, 0)
	operatorInformerClient := operatorFake
	if tc.staleBackups != nil {
		staleOperatorObjs := make([]runtime.Object, 0, len(tc.staleBackups))
		staleOperatorObjs = testutils.AppendRuntimeObjects(staleOperatorObjs, tc.staleBackups)
		operatorInformerClient = operatorfake.NewSimpleClientset(staleOperatorObjs...)
	}
	operatorSharedFactory := operatorinformers.NewSharedInformerFactory(operatorInformerClient, 0)

	podsInformer := sharedFactory.Core().V1().Pods()
	jobsInformer := sharedFactory.Batch().V1().Jobs()
	nodesInformer := sharedFactory.Core().V1().Nodes()
	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()

	podsInformerHasSynced := podsInformer.Informer().HasSynced
	jobsInformerHasSynced := jobsInformer.Informer().HasSynced
	nodesInformerHasSynced := nodesInformer.Informer().HasSynced
	backupsInformerHasSynced := backupsInformer.Informer().HasSynced

	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), backupsInformerHasSynced, podsInformerHasSynced, jobsInformerHasSynced, nodesInformerHasSynced)

	activeCache := newActiveBackupCache()
	if tc.activeCache != nil {
		activeCache = *tc.activeCache
	}

	controller := BackupController{
		backupsLister:         backupsInformer.Lister(),
		podsLister:            podsInformer.Lister().Pods(operatorclient.TargetNamespace),
		nodesLister:           nodesInformer.Lister(),
		jobInformer:           jobsInformer.Informer(),
		operatorClient:        operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
		activeCache:           activeCache,
	}
	err := controller.addIndexers()
	require.NoError(t, err)

	err = controller.sync(ctx, testutils.FakeSyncContext(t, factory.DefaultQueueKey))
	if tc.expectError {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	if tc.validate != nil {
		tc.validate(t, client, operatorFake)
	}
}

func TestSyncLoopHappyPath(t *testing.T) {
	// Create a backup job for an EtcdBackup
	t.Run("local", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)

				action, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.True(t, ok, "Expected status patch action")
				updatedBackup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), action.Name, metav1.GetOptions{})
				require.NoError(t, err)

				require.True(t, backuphelpers.IsBackupRunning(updatedBackup), "Expected backup to be running")
				require.Equal(t, "test-node", updatedBackup.Status.NodeName)
				requireBackupJob(t, updatedBackup, job)
			},
		})
	})

	t.Run("pvc", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("test-backup")
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)

				action, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.True(t, ok, "Expected status patch action")
				updatedBackup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), action.Name, metav1.GetOptions{})
				require.NoError(t, err)

				require.True(t, backuphelpers.IsBackupRunning(updatedBackup), "Expected backup to be running")
				require.Empty(t, updatedBackup.Status.NodeName, "Expected no node to be assigned to a PVC backup")
				requireBackupJob(t, updatedBackup, job)
			},
		})
	})
}

func TestJobAlreadyRunning(t *testing.T) {
	// Don't create a new backup job when one already exists
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "test-backup",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
			OwnerReferences: []v1.OwnerReference{{
				Kind: "EtcdBackup",
				Name: "test-backup",
				UID:  "test-backup-uid",
			}}}}

	t.Run("correct-labels-and-status", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupRunning(job))},
			jobs:    []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				_, ok := testutils.GetAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.False(t, ok, "Did not expect a patch action")
			},
		})
	})
	t.Run("backup-missing-job-status", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup")},
			jobs:    []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)

				action, ok := testutils.GetAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.True(t, ok, "Expected patch action")

				updatedBackup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), action.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, job.Name, updatedBackup.Status.JobName)
			},
		})
	})
	t.Run("job-missing-backup-label", func(t *testing.T) {
		job = job.DeepCopy()
		delete(job.Labels, backuphelpers.LabelEtcdBackup)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupAge(time.Hour), testutils.WithBackupRunning(job))},
			jobs:    []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)

				action, ok := testutils.GetAction(client.Actions(), func(a k8stesting.PatchActionImpl) bool {
					return a.GetResource().Resource == "jobs"
				})
				require.True(t, ok, "Expected job patch action")

				patchBody, err := json.Marshal(metav1.ObjectMeta{Labels: map[string]string{backuphelpers.LabelEtcdBackup: "test-backup"}})
				require.NoError(t, err)
				require.Equal(t, patchBody, action.Patch)
			},
		})
	})
}

func TestBackupJobCompleted(t *testing.T) {
	// Completed backup job is processed and does not start a new job
	now := metav1.Now()
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "completed-backup-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeName: "test-node"}}},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue}}}}
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupRunning(job))
	pods := []*corev1.Pod{
		testutils.FakePod("failed-backup-job-pod-1",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Time{Time: now.Add(-time.Minute)})),
		testutils.FakePod("failed-backup-job-pod-2",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(now),
			func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodSucceeded
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "backup",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
							Message:  `{"files": [{"path": "/my/successful/backup.db", "sizeBytes": 104857600}, {"path": "/my/successful/static_kuberesources.tar.gz", "sizeBytes": 4321}]}`,
						},
					},
				}}
			})}

	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		jobs:    []*batchv1.Job{job},
		pods:    pods,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
				Type:    string(operatorv1alpha1.BackupCompleted),
				Reason:  string(operatorv1alpha1.BackupReasonJobCompleted),
				Message: "backup job completed",
				Status:  metav1.ConditionTrue,
			}}, []operatorv1alpha1.EtcdBackupFile{
				{Path: "/my/successful/backup.db", SizeBytes: 100 * 1024 * 1024},
				{Path: "/my/successful/static_kuberesources.tar.gz", SizeBytes: 4321}})
			requireJobPatched(t, client, "test-backup")
		},
	})
}

func TestQueuedBackupDeleted(t *testing.T) {
	t.Run("no-finalizer", func(t *testing.T) {
		// Backup deleted before it was started, nothing to clean up
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupDeleted())},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				_, ok := testutils.GetAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.False(t, ok, "Expected no patch action")
			},
		})
	})
	t.Run("before-grace-period", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", func(backup *operatorv1alpha1.EtcdBackup) {
				now := v1.Now()
				backup.DeletionTimestamp = &now
				backup.Finalizers = append(backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
			})},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				_, ok := testutils.GetAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.False(t, ok, "Expected no patch action")
			},
		})
	})
	t.Run("after-grace-period", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", func(backup *operatorv1alpha1.EtcdBackup) {
				backup.DeletionTimestamp = &v1.Time{Time: time.Now().Add(-(backupDeletedGracePeriod + time.Second))}
				backup.Finalizers = append(backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
			})},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)

				action, ok := testutils.GetAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.True(t, ok, "Expected patch action")
				updatedBackup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), action.Name, metav1.GetOptions{})
				require.NoError(t, err)

				require.NotContains(t, updatedBackup.Finalizers, backuphelpers.FinalizerEtcdBackup)
			},
		})
	})
}

func TestBackupFailedRequiresGC(t *testing.T) {
	// EtcdBackups that failed after creating some files, or failure state is unknown and files could have been created
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "failed-backup-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeName: "test-node"}}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue}}}}
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupAge(time.Hour), testutils.WithBackupRunning(job))
	podFailedSilently := testutils.FakePod("failed-backup-job-pod-1",
		testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
		testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
		testutils.WithCreationTimestamp(v1.Time{Time: time.Now().Add(-time.Minute)}),
		func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
				}}}}
		})
	podFailedGracefully := testutils.FakePod("failed-backup-job-pod-2",
		testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
		testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
		testutils.WithCreationTimestamp(v1.Now()),
		func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Started: new(true),
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Message:  `{"message": "snapshot failed: no disk space", "files": [{"path": "/my/broken/backup.db.part", "sizeBytes": 12345}]}`,
				}}}}
		})

	t.Run("graceful-failure", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			// There may be more than one failed pod for the job.
			pods: []*corev1.Pod{podFailedSilently, podFailedGracefully},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonJobFailed),
					Message: "snapshot failed: no disk space",
					Status:  metav1.ConditionTrue,
				}}, []operatorv1alpha1.EtcdBackupFile{{
					Path:      "/my/broken/backup.db.part",
					SizeBytes: 12345,
				}})
			},
		})
	})
	t.Run("silent-failure", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			pods:    []*corev1.Pod{podFailedSilently},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonJobFailed),
					Message: "backup job failed",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
	t.Run("job-deleted", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonJobFailed),
					Message: fmt.Sprintf("unable to find job %q", job.Name),
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
}

func TestBackupFailedNoGC(t *testing.T) {
	// EtcdBackups that failed gracefully without creating any files
	t.Run("pvc-not-found", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
					PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc-that-doesnt-exist"},
				}))},
			nodes: []*corev1.Node{testutils.FakeNode("test-node")},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "unable to find PVC \"backup-pvc-that-doesnt-exist\"",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
	t.Run("node-not-found", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage), func(backup *operatorv1alpha1.EtcdBackup) {
					backup.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "test-node-that-doesnt-exist"}
				})},
			nodes: []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "unable to find a control plane Node matching the selector",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
	t.Run("job-failed-gracefully", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: v1.ObjectMeta{
				Name:       "failed-backup-job",
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup}},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeName: "test-node"}}},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: corev1.ConditionTrue}}}}
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup", testutils.WithBackupRunning(job))},
			jobs: []*batchv1.Job{job},
			pods: []*corev1.Pod{
				testutils.FakePod("failed-backup-job-pod",
					testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
					testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
					testutils.WithCreationTimestamp(v1.Now()),
					func(pod *corev1.Pod) {
						pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
								ExitCode: 1, Message: `{"files": []}`,
							}}}}
					}),
			},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "backup job failed without creating files",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
}

func TestConcurrencyLocalSelectAvailableNode(t *testing.T) {
	// Node selection only applies to Local backups; PVC backups are placed by the scheduler.
	running, runningJob := fakeRunningBackup("running", "test-node-1", localStorage)
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{running, testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage))},
		jobs:    []*batchv1.Job{runningJob},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 1)
			require.Equal(t, "new", jobs[0].Name)
			require.Equal(t, "test-node-2", jobs[0].Spec.Template.Spec.NodeName, "Expected backup to be assigned to an available node")

			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "new", metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupRunning(backup), "Expected backup to be running")
			require.Equal(t, "test-node-2", backup.Status.NodeName)
		},
	})
}

func TestConcurrencyLocalAllNodesInUse(t *testing.T) {
	running1, runningJob1 := fakeRunningBackup("running-1", "test-node-1", localStorage)
	running2, runningJob2 := fakeRunningBackup("running-2", "test-node-2", localStorage)
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{running1, running2, testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage))},
		jobs:    []*batchv1.Job{runningJob1, runningJob2},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			_, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no status patch action")
		},
	})
}

func TestConcurrencyLocalNodeSelector(t *testing.T) {
	// Backup waits for the node matching its selector, even when other nodes are available
	running, runningJob := fakeRunningBackup("running", "test-node-1", localStorage)
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{running, testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage), func(backup *operatorv1alpha1.EtcdBackup) {
			backup.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "test-node-1"}
		})},
		jobs: []*batchv1.Job{runningJob},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel(), func(node *corev1.Node) { node.Labels["kubernetes.io/hostname"] = "test-node-1" }),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel(), func(node *corev1.Node) { node.Labels["kubernetes.io/hostname"] = "test-node-2" })},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			_, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
			require.False(t, ok, "Expected no status patch action")
		},
	})
}

func TestConcurrencyPVCSamePVC(t *testing.T) {
	storage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "test-backup-pvc"}}
	running, runningJob := fakeRunningBackup("running", "", storage)
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{running, testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(storage))},
		jobs:    []*batchv1.Job{runningJob},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
		},
	})
}

func TestConcurrencyPVCDifferentPVC(t *testing.T) {
	running, runningJob := fakeRunningBackup("running", "", operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "running-pvc"}})
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{running, testutils.FakeEtcdBackup("new")},
		jobs:    []*batchv1.Job{runningJob},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		pvcs: []*corev1.PersistentVolumeClaim{
			testutils.FakePVC(operatorclient.TargetNamespace, "running-pvc"),
			testutils.FakePVC(operatorclient.TargetNamespace, "new-pvc")},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 1)
			require.Equal(t, "new", jobs[0].Name)
		},
	})
}

func TestConcurrencyFinishedJobReleasesNode(t *testing.T) {
	// A backup finishing in the same sync frees its node for a queued backup
	now := metav1.Now()
	finished, finishedJob := fakeRunningBackup("finished", "test-node", localStorage)
	finishedJob.Status = batchv1.JobStatus{
		CompletionTime: &now,
		Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{finished, testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage))},
		jobs:    []*batchv1.Job{finishedJob},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 1)
			require.Equal(t, "new", jobs[0].Name)
			require.Equal(t, "test-node", jobs[0].Spec.Template.Spec.NodeName)

			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "finished", metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupCompleted(backup), "Expected backup to be completed")
		},
	})
}

func TestConcurrencyOrderByAge(t *testing.T) {
	// Oldest backups are started first, one per node
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-4", testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(0)),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(2*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(3*time.Hour)),
			testutils.FakeEtcdBackup("test-backup-3", testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(time.Hour))},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 2)
			nodes := map[string]string{}
			for _, job := range jobs {
				nodes[job.Name] = job.Spec.Template.Spec.NodeName
			}
			require.Contains(t, nodes, "test-backup-1")
			require.Contains(t, nodes, "test-backup-2")
			require.NotEqual(t, nodes["test-backup-1"], nodes["test-backup-2"], "Expected backups to be started on different nodes")
		},
	})
}

func TestActiveCacheStaleInformer(t *testing.T) {
	// Backup was started by a previous sync, but the informer hasn't observed the update yet
	running, _ := fakeRunningBackup("running", "test-node-1", localStorage)
	activeCache := newActiveBackupCache()
	activeCache.add(running)
	runBackupControllerTest(t, testCaseBackupController{
		backups:      []*operatorv1alpha1.EtcdBackup{running},
		staleBackups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("running", testutils.WithBackupStorage(localStorage))},
		nodes:        []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
		activeCache:  &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)

			// Get request sent to verify mismatch between active cache and observed active backups from informer
			getAction, ok := testutils.GetAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected get action")
			require.Equal(t, "running", getAction.Name)
			require.True(t, activeCache.isActive("running"))
		},
	})
}

func TestActiveCacheSyncedInformer(t *testing.T) {
	running, runningJob := fakeRunningBackup("running", "test-node-1", localStorage)
	activeCache := newActiveBackupCache()
	activeCache.add(running)
	runBackupControllerTest(t, testCaseBackupController{
		backups:     []*operatorv1alpha1.EtcdBackup{running},
		jobs:        []*batchv1.Job{runningJob},
		activeCache: &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// No get request needed since active cache matched active backups observed from informer
			getAction, ok := testutils.GetAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.Falsef(t, ok, "Expected no get action, found %+v", getAction)
			require.True(t, activeCache.isActive("running"))
		},
	})
}

func TestActiveCacheRemoveFinished(t *testing.T) {
	backups := []*operatorv1alpha1.EtcdBackup{
		testutils.FakeEtcdBackup("completed", testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupCompleted()),
		testutils.FakeEtcdBackup("failed", testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed)),
	}
	activeCache := newActiveBackupCache()
	for _, backup := range backups {
		activeCache.add(backup)
	}
	require.Len(t, activeCache.backups, 2, "Expected 2 active backups")
	require.Len(t, activeCache.local, 0, "Expected 0 nodes with active Local backups")
	require.Len(t, activeCache.pvc, 2, "Expected 2 pvcs with active PVC backups")

	runBackupControllerTest(t, testCaseBackupController{
		backups:     backups,
		activeCache: &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// No get request needed to remove inactive backups from the cache
			getAction, ok := testutils.GetAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.Falsef(t, ok, "Expected no get action, found %+v", getAction)

			require.Len(t, activeCache.backups, 0)
			require.Len(t, activeCache.local, 0)
			require.Len(t, activeCache.pvc, 0)
		},
	})
}

func TestActiveCacheRemoveDeleted(t *testing.T) {
	backup, _ := fakeRunningBackup("deleted", "test-node-1", localStorage)
	activeCache := newActiveBackupCache()
	activeCache.add(backup)

	runBackupControllerTest(t, testCaseBackupController{
		backups:     []*operatorv1alpha1.EtcdBackup{},
		activeCache: &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Expect get request to verify backup is deleted before removing from backup cache
			action, ok := testutils.GetAction[k8stesting.GetActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected get action")
			require.Equal(t, backup.Name, action.Name)

			require.Len(t, activeCache.backups, 0)
			require.Len(t, activeCache.local, 0)
			require.Len(t, activeCache.pvc, 0)
		},
	})
}

func TestIsJobComplete(t *testing.T) {
	tests := map[string]struct {
		condition batchv1.JobConditionType
		complete  bool
	}{
		"no condition": {condition: "", complete: false},
		"suspended":    {condition: batchv1.JobSuspended, complete: false},
		"complete":     {condition: batchv1.JobComplete, complete: true},
		"failed":       {condition: batchv1.JobFailed, complete: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			j := &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{Type: test.condition, Status: corev1.ConditionTrue},
					},
				},
			}
			finished := isJobFinished(j)
			require.Equal(t, test.complete, finished)
		})
	}
}

func TestNoFeatureGateDisablesController(t *testing.T) {
	controller := BackupController{
		featureGateAccessor: featuregates.NewHardcodedFeatureGateAccess(
			[]configv1.FeatureGateName{},
			[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName}),
	}

	err := controller.sync(context.TODO(), nil)
	// TODO(thomas): that doesn't _really_ tell whether it's not running, we would assume a panic otherwise
	require.NoError(t, err)

	// invariant test for when the feature gate isn't defined at all
	require.Panics(t, func() {
		controller := BackupController{
			featureGateAccessor: featuregates.NewHardcodedFeatureGateAccess(
				[]configv1.FeatureGateName{},
				[]configv1.FeatureGateName{}),
		}

		_ = controller.sync(context.TODO(), nil)
	})
}

// fakeRunningBackup returns a running backup and its job. Local backup jobs are pinned to nodeName.
func fakeRunningBackup(name, nodeName string, storage operatorv1alpha1.EtcdBackupStorage) (*operatorv1alpha1.EtcdBackup, *batchv1.Job) {
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       name,
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: name},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
			OwnerReferences: []v1.OwnerReference{{
				Kind: "EtcdBackup",
				Name: name,
				UID:  types.UID(name + "-uid"),
			}}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: nodeName}}}}
	backup := testutils.FakeEtcdBackup(name, testutils.WithBackupStorage(storage), testutils.WithBackupRunning(job), testutils.WithBackupNodeName(nodeName))
	return backup, job
}

func listCreatedJobs(client *k8sfakeclient.Clientset) []*batchv1.Job {
	var jobs []*batchv1.Job
	for _, action := range testutils.ListActions[k8stesting.CreateActionImpl](client.Actions()) {
		if job, ok := action.Object.(*batchv1.Job); ok {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func requireNoBackupJobCreated(t *testing.T, client *k8sfakeclient.Clientset) {
	t.Helper()
	createAction := findFirstCreateAction(client)
	require.Nilf(t, createAction, "expected to not find one createAction, but found %v", client.Fake.Actions())
}

func requireBackupJobCreated(t *testing.T, client *k8sfakeclient.Clientset, backup *operatorv1alpha1.EtcdBackup) *batchv1.Job {
	t.Helper()
	createAction := findFirstCreateAction(client)
	require.NotNilf(t, createAction, "expected to find at least one createAction, but found %v", client.Fake.Actions())
	require.Equal(t, operatorclient.TargetNamespace, createAction.GetNamespace())
	require.Equal(t, "create", createAction.GetVerb())
	createdJob := createAction.Object.(*batchv1.Job)

	require.Truef(t, strings.HasPrefix(createdJob.Name, backup.Name), "expected job.name [%s] to have prefix [%s]", createdJob.Name, backup.Name)
	require.Equal(t, operatorclient.TargetNamespace, createdJob.Namespace)
	require.Equal(t, backup.Name, createdJob.Labels[backuphelpers.LabelEtcdBackup])
	require.Equal(t, "operator-pullspec-image", createdJob.Spec.Template.Spec.InitContainers[0].Image)
	require.Equal(t, "operator-pullspec-image", createdJob.Spec.Template.Spec.Containers[0].Image)

	foundVolume := false
	for _, volume := range createdJob.Spec.Template.Spec.Volumes {
		if volume.Name == "etc-kubernetes-cluster-backup" {
			foundVolume = true
			switch backup.Spec.Storage.Type {
			case operatorv1alpha1.EtcdBackupStorageTypeLocal:
				require.Equal(t, backup.Spec.Storage.Local.HostPath, volume.HostPath.Path)
			case operatorv1alpha1.EtcdBackupStorageTypePVC:
				require.Equal(t, backup.Spec.Storage.PVC.Name, volume.PersistentVolumeClaim.ClaimName)
			default:
				require.Fail(t, "Unrecognized backup storage type: %s", string(backup.Spec.Storage.Type))
			}
		}
	}

	require.Truef(t, foundVolume, "could not find injected PVC volume in %v", createdJob.Spec.Template.Spec.Volumes)
	require.Equal(t, len(backup.OwnerReferences)+1, len(createdJob.OwnerReferences))
	require.Equal(t, backup.Name, createdJob.OwnerReferences[0].Name)
	for i := 0; i < len(backup.OwnerReferences); i++ {
		require.Equal(t, backup.OwnerReferences[i], createdJob.OwnerReferences[i+1])
	}
	return createdJob
}

func findFirstCreateAction(client *k8sfakeclient.Clientset) *k8stesting.CreateActionImpl {
	var createAction *k8stesting.CreateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.CreateActionImpl); ok {
			createAction = &a
			break
		}
	}
	return createAction
}

func requireBackupStatusApplied(
	t *testing.T,
	client *operatorfake.Clientset,
	expectedConditions []metav1.Condition,
	expectedFiles []operatorv1alpha1.EtcdBackupFile) {
	t.Helper()
	action, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](client.Fake.Actions())
	require.Truef(t, ok, "expected to find at least one status patchAction, but found %v", client.Fake.Actions())

	backup, err := client.OperatorV1alpha1().EtcdBackups().Get(t.Context(), action.Name, metav1.GetOptions{})
	require.NoError(t, err)

	require.ElementsMatch(t, expectedConditions, removeTransitionTime(backup.Status.Conditions))
	require.ElementsMatch(t, expectedFiles, backup.Status.Files)
}

func requireJobPatched(t *testing.T, client *k8sfakeclient.Clientset, backupName string) {
	t.Helper()
	action, ok := testutils.GetAction(client.Fake.Actions(), func(a k8stesting.PatchActionImpl) bool {
		return a.GetResource().Resource == "jobs"
	})
	require.Truef(t, ok, "expected to find at least one job patchAction, but found %v", client.Fake.Actions())

	job, err := client.BatchV1().Jobs(action.Namespace).Get(t.Context(), action.Name, metav1.GetOptions{})
	require.NoError(t, err)

	require.Equal(t, map[string]string{"app": "cluster-backup-job", backuphelpers.LabelEtcdBackup: backupName}, job.Labels)
	require.NotContains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
}

func requireBackupJob(t *testing.T, backup *operatorv1alpha1.EtcdBackup, job *batchv1.Job) {
	t.Helper()
	require.Equal(t, job.Name, backup.Status.JobName)
	require.Contains(t, job.OwnerReferences, metav1.OwnerReference{
		APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "EtcdBackup", Name: backup.Name, UID: backup.UID,
	})
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		// Local backups are pinned to the node selected by the controller.
		require.Equal(t, backup.Status.NodeName, job.Spec.Template.Spec.NodeName)
		require.Empty(t, job.Spec.Template.Spec.NodeSelector)
		require.Nil(t, job.Spec.Template.Spec.Affinity)
		require.Contains(t, job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "etc-kubernetes-cluster-backup",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: backup.Spec.Storage.Local.HostPath,
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		})
		require.Contains(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      "etc-kubernetes-cluster-backup",
			MountPath: filepath.Join(backupPathMount, backup.Spec.Storage.Local.HostPath),
		})
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		// PVC backups are not pinned to a node; they are constrained to master nodes and placed by the scheduler.
		require.Empty(t, job.Spec.Template.Spec.NodeName)
		require.Equal(t, "", job.Spec.Template.Spec.NodeSelector[backuphelpers.ControlPlaneNodeLabelSelector])
		require.Contains(t, job.Spec.Template.Spec.NodeSelector, backuphelpers.ControlPlaneNodeLabelSelector)
		require.NotNil(t, job.Spec.Template.Spec.Affinity)
		require.NotNil(t, job.Spec.Template.Spec.Affinity.PodAntiAffinity)
		require.Contains(t, job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "etc-kubernetes-cluster-backup",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: backup.Spec.Storage.PVC.Name,
				},
			},
		})
		require.Contains(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      "etc-kubernetes-cluster-backup",
			MountPath: backupPathMount,
		})

	}
}

// removeTransitionTime will create a new list of conditions without the LastTransitionTime.
// We need to remove the time component to be able to match the structs in require.ElementsMatch
func removeTransitionTime(conditions []v1.Condition) []v1.Condition {
	var timelessConditions []v1.Condition
	for _, c := range conditions {
		timelessConditions = append(timelessConditions, v1.Condition{
			Type:    c.Type,
			Status:  c.Status,
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return timelessConditions
}
