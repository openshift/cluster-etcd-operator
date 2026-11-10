package backupcontroller

import (
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	staleBackups     []*operatorv1alpha1.EtcdBackup
	jobs             []*batchv1.Job
	pods             []*corev1.Pod
	nodes            []*corev1.Node
	pvcs             []*corev1.PersistentVolumeClaim
	pending          pendingStarts
	k8sReactors      []k8stesting.SimpleReactor
	operatorReactors []k8stesting.SimpleReactor
	expectError      bool
	validate         func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
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
	pvcsInformer := sharedFactory.Core().V1().PersistentVolumeClaims()
	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()

	podsInformerHasSynced := podsInformer.Informer().HasSynced
	jobsInformerHasSynced := jobsInformer.Informer().HasSynced
	nodesInformerHasSynced := nodesInformer.Informer().HasSynced
	pvcsInformerHasSynced := pvcsInformer.Informer().HasSynced
	backupsInformerHasSynced := backupsInformer.Informer().HasSynced

	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), backupsInformerHasSynced, podsInformerHasSynced, jobsInformerHasSynced, nodesInformerHasSynced, pvcsInformerHasSynced)

	for _, reactor := range tc.k8sReactors {
		client.PrependReactor(reactor.Verb, reactor.Resource, reactor.Reaction)
	}
	for _, reactor := range tc.operatorReactors {
		operatorFake.PrependReactor(reactor.Verb, reactor.Resource, reactor.Reaction)
	}

	pending := tc.pending
	if pending == nil {
		pending = pendingStarts{}
	}

	controller := BackupController{
		backupsLister:         backupsInformer.Lister(),
		podsLister:            podsInformer.Lister().Pods(operatorclient.TargetNamespace),
		nodesLister:           nodesInformer.Lister(),
		pvcsLister:            pvcsInformer.Lister().PersistentVolumeClaims(operatorclient.TargetNamespace),
		jobInformer:           jobsInformer.Informer(),
		operatorClient:        operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
		pending:               pending,
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

				actions := testutils.ListStatusActions[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.Len(t, actions, 2)
				updatedBackup, ok := actions[0].Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected etcdbackup object")
				require.Equal(t, "test-node", updatedBackup.Status.NodeName)
				require.False(t, backuphelpers.IsBackupRunning(updatedBackup), "expected backup node to be updated before setting status")

				updatedBackup, ok = actions[1].Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected etcdbackup object")
				require.True(t, backuphelpers.IsBackupRunning(updatedBackup), "expected backup to be running")
				requireBackupJob(t, updatedBackup, job)
			},
		})
	})

	t.Run("pvc", func(t *testing.T) {
		// PVC backups progress without a preassigned node; the scheduler places the job.
		backup := testutils.FakeEtcdBackup("test-backup")
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)
				require.Empty(t, backup.Status.NodeName)
				require.Empty(t, job.Spec.Template.Spec.NodeName)

				action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.True(t, ok, "expected status update action")
				updatedBackup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected etcdbackup object")

				require.True(t, backuphelpers.IsBackupRunning(updatedBackup), "Expected backup to be running")
				require.Empty(t, updatedBackup.Status.NodeName, "Expected no node to be assigned to a PVC backup")
				require.Equal(t, job.Name, updatedBackup.Status.JobName)
				requireBackupJob(t, updatedBackup, job)
			},
		})
	})
}

func TestBackupWithAssets(t *testing.T) {
	// Create a backup job for an EtcdBackup
	t.Run("snapshot", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage), testutils.WithBackupAssets(operatorv1alpha1.EtcdBackupAssetTypeSnapshot))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Args, "--assets=Snapshot")
			},
		})
	})

	t.Run("statickuberesources", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage), testutils.WithBackupAssets(operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Args, "--assets=StaticKubeResources")
			},
		})
	})

	t.Run("snapshot-and-statickuberesources", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage), testutils.WithBackupAssets(operatorv1alpha1.EtcdBackupAssetTypeSnapshot, operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)
				require.Contains(t, job.Spec.Template.Spec.Containers[0].Args, "--assets=Snapshot,StaticKubeResources")
			},
		})
	})

	t.Run("default-assets", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				job := requireBackupJobCreated(t, client, backup)
				for _, arg := range job.Spec.Template.Spec.Containers[0].Args {
					require.False(t, strings.HasPrefix(arg, "--assets="), "Expected no --assets arg")
				}
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
				_, ok := testutils.GetAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.False(t, ok, "Did not expect an update action")
			},
		})
	})
	t.Run("backup-missing-job-status", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup")},
			jobs:    []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)

				action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.True(t, ok, "expected status update action")
				updatedBackup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected etcdbackup object")

				require.Equal(t, job.Name, updatedBackup.Status.JobName)
			},
		})
	})
	t.Run("job-missing-backup-label", func(t *testing.T) {
		job := job.DeepCopy()
		delete(job.Labels, backuphelpers.LabelEtcdBackup)
		backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupAge(time.Hour), testutils.WithBackupRunning(job))
		job.OwnerReferences = append(job.OwnerReferences, metav1.OwnerReference{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "EtcdBackup",
			Name:       backup.Name,
			UID:        backup.UID,
		})
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)

				action, ok := testutils.GetAction(client.Actions(), func(a k8stesting.PatchActionImpl) bool {
					return a.GetResource().Resource == "jobs"
				})
				require.True(t, ok, "Expected job patch action")

				patchBody, err := json.Marshal(batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{backuphelpers.LabelEtcdBackup: "test-backup"}}})
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
			Failed:         1,
			Succeeded:      1,
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue}}}}
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupRunning(job))
	pods := []*corev1.Pod{
		testutils.FakePod("failed-backup-job-pod-1",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Time{Time: now.Add(-time.Minute)}),
			func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed }),
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
			requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
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
				_, ok := testutils.GetAction[k8stesting.UpdateAction](operatorFake.Actions())
				require.False(t, ok, "Expected no update action")
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
				_, ok := testutils.GetAction[k8stesting.UpdateAction](operatorFake.Actions())
				require.False(t, ok, "Expected no update action")
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

				action, ok := testutils.GetAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.True(t, ok, "Expected update action")
				updatedBackup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected backup object")

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
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
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
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonJobFailed),
					Message: "backup job failed",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
	unknownExecution := func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
		requireNoBackupJobCreated(t, client)
		requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
			Type:    string(operatorv1alpha1.BackupFailed),
			Reason:  string(operatorv1alpha1.BackupReasonJobFailed),
			Message: "unable to determine backup job outcome",
			Status:  metav1.ConditionTrue,
		}}, nil)
	}
	t.Run("pod-missing", func(t *testing.T) {
		// Job counted a failed pod that is absent from the cache, the backup container may have written files
		jobCountedPod := job.DeepCopy()
		jobCountedPod.Status.Failed = 1
		runBackupControllerTest(t, testCaseBackupController{
			backups:  []*operatorv1alpha1.EtcdBackup{backup},
			jobs:     []*batchv1.Job{jobCountedPod},
			validate: unknownExecution,
		})
	})
	t.Run("pod-uncounted-missing", func(t *testing.T) {
		// Pod terminated but is not yet counted and is absent from the cache
		jobUncountedPod := job.DeepCopy()
		jobUncountedPod.Status.UncountedTerminatedPods = &batchv1.UncountedTerminatedPods{Failed: []types.UID{"pod-uid"}}
		runBackupControllerTest(t, testCaseBackupController{
			backups:  []*operatorv1alpha1.EtcdBackup{backup},
			jobs:     []*batchv1.Job{jobUncountedPod},
			validate: unknownExecution,
		})
	})
	t.Run("pod-terminating", func(t *testing.T) {
		// Job still has a terminating pod that is absent from the cache
		jobTerminatingPod := job.DeepCopy()
		jobTerminatingPod.Status.Terminating = new(int32(1))
		runBackupControllerTest(t, testCaseBackupController{
			backups:  []*operatorv1alpha1.EtcdBackup{backup},
			jobs:     []*batchv1.Job{jobTerminatingPod},
			validate: unknownExecution,
		})
	})
	t.Run("pod-stale-pre-start", func(t *testing.T) {
		// Cached pod predates the backup container starting, even though the job has already failed
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			pods: []*corev1.Pod{testutils.FakePod("failed-backup-job-pod-stale",
				testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
				testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
				testutils.WithCreationTimestamp(v1.Now()),
				testutils.WithPodStatus(corev1.PodPending))},
			validate: unknownExecution,
		})
	})
	t.Run("job-deleted", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
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
		// Missing PVC must reach Failed with SetupFailed; PVC backups are not pinned to a node.
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{
				testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
					PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc-that-doesnt-exist"},
				}))},
			nodes: []*corev1.Node{testutils.FakeNode("test-node")},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
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
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
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
			Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
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
					testutils.WithPodStatus(corev1.PodFailed),
					func(pod *corev1.Pod) {
						pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
								ExitCode: 1, Message: `{"files": []}`,
							}}}}
					}),
			},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "backup job failed without creating files",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
	t.Run("job-failed-without-pods", func(t *testing.T) {
		// Job failed without ever accounting for a pod, so no backup container can have run
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
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "backup job failed without creating pods",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
	t.Run("backup-container-never-started", func(t *testing.T) {
		// All pods are terminal and none started the backup container, e.g. init container failed
		job := &batchv1.Job{
			ObjectMeta: v1.ObjectMeta{
				Name:       "failed-backup-job",
				Namespace:  operatorclient.TargetNamespace,
				Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
				Finalizers: []string{backuphelpers.FinalizerEtcdBackup}},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeName: "test-node"}}},
			Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
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
					testutils.WithPodStatus(corev1.PodFailed)),
			},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "failed to verify storage",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
}

func TestBackupOutcomeIgnoresPodsOfPreviousJob(t *testing.T) {
	// Job names are derived from the backup name, so a recreated backup reuses its job name. Pods left over from the
	// previous job carry the same job-name label and must not be used to determine the outcome of the current job.
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "failed-backup-job",
			Namespace:  operatorclient.TargetNamespace,
			UID:        "current-job-uid",
			Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeName: "test-node"}}},
		Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue}}}}
	jobPod := func(name string, ownerUID types.UID, phase corev1.PodPhase, message string) *corev1.Pod {
		return testutils.FakePod(name,
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: ownerUID}),
			testutils.WithCreationTimestamp(v1.Now()),
			testutils.WithPodStatus(phase),
			func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}}}}
			})
	}

	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup", testutils.WithBackupRunning(job))},
		jobs: []*batchv1.Job{job},
		pods: []*corev1.Pod{
			jobPod("failed-backup-job-pod", job.UID, corev1.PodFailed, `{"files": []}`),
			jobPod("previous-backup-job-pod", "previous-job-uid", corev1.PodSucceeded,
				`{"message": "previous backup", "files": [{"path": "/etc/kubernetes/cluster-backup/test-backup/snapshot.db", "sizeBytes": 10}]}`),
		},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
				Type:    string(operatorv1alpha1.BackupFailed),
				Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
				Message: "backup job failed without creating files",
				Status:  metav1.ConditionTrue,
			}}, nil)
		},
	})
}

func TestOrphanedJob(t *testing.T) {
	orphanedJob := func(nodeName string, configs ...func(*batchv1.Job)) *batchv1.Job {
		_, job := fakeRunningBackup("deleted-backup", nodeName, localStorage)
		for _, config := range configs {
			config(job)
		}
		return job
	}
	finished := func(job *batchv1.Job) {
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	deleting := func(job *batchv1.Job) {
		job.DeletionTimestamp = ptr.To(v1.Now())
	}
	requireJobFinalized := func(t *testing.T, client *k8sfakeclient.Clientset, jobName string) {
		t.Helper()
		job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(t.Context(), jobName, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotContains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
	}

	t.Run("finished", func(t *testing.T) {
		job := orphanedJob("test-node-1", finished)
		runBackupControllerTest(t, testCaseBackupController{
			jobs: []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireJobFinalized(t, client, job.Name)
			},
		})
	})
	t.Run("deleting", func(t *testing.T) {
		job := orphanedJob("test-node-1", deleting)
		runBackupControllerTest(t, testCaseBackupController{
			jobs: []*batchv1.Job{job},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireJobFinalized(t, client, job.Name)
			},
		})
	})
	t.Run("running", func(t *testing.T) {
		// The backup was force deleted while its job was running. Whoever deleted it is responsible for side effects,
		// so the job is released without holding its node.
		job := orphanedJob("test-node-1")
		queued := testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{queued},
			jobs:    []*batchv1.Job{job},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireJobFinalized(t, client, job.Name)
				createdJob := requireBackupJobCreated(t, client, queued)
				require.Equal(t, "test-node-1", createdJob.Spec.Template.Spec.NodeName)
			},
		})
	})
	t.Run("backup-recreated-with-same-name", func(t *testing.T) {
		// A job owned by a previous EtcdBackup with the same name is orphaned, and must not be reconciled as the
		// job of the new backup
		job := orphanedJob("test-node-1", finished)
		backup := testutils.FakeEtcdBackup("deleted-backup", testutils.WithBackupStorage(localStorage),
			func(backup *operatorv1alpha1.EtcdBackup) { backup.UID = "recreated-uid" })
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireJobFinalized(t, client, job.Name)
				action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.True(t, ok, "expected status update action")
				updatedBackup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected etcdbackup object")

				require.False(t, backuphelpers.IsBackupFinished(updatedBackup), "new backup must not inherit the old job's result")
			},
		})
	})
}

func TestJobNotFoundStaleBackup(t *testing.T) {
	// The informer still shows the backup running, but it already completed and its job is gone. The backup must not
	// be failed based on the stale cache.
	job := &batchv1.Job{ObjectMeta: v1.ObjectMeta{Name: "finalized-job", Namespace: operatorclient.TargetNamespace}}
	running := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupAge(time.Hour), testutils.WithBackupRunning(job))
	completed := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupAge(time.Hour), testutils.WithBackupRunning(job), testutils.WithBackupCompleted())

	runBackupControllerTest(t, testCaseBackupController{
		backups:      []*operatorv1alpha1.EtcdBackup{completed},
		staleBackups: []*operatorv1alpha1.EtcdBackup{running},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.False(t, ok, "expected no status update, but found %v", operatorFake.Actions())

			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), completed.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupCompleted(backup))
		},
	})
}

func TestFinishedStatusConflictRequeues(t *testing.T) {
	_, job := fakeRunningBackup("test-backup", "test-node", localStorage)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage), testutils.WithBackupRunning(job))

	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		jobs:    []*batchv1.Job{job},
		operatorReactors: []k8stesting.SimpleReactor{{
			Verb:     "update",
			Resource: "etcdbackups",
			Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "status" {
					return false, nil, nil
				}
				return true, nil, apierrors.NewConflict(operatorv1alpha1.Resource("etcdbackups"), backup.Name, fmt.Errorf("modified"))
			},
		}},
		// A conflict is expected when the cache is stale, so it is retried without reporting an error
		expectError: false,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// The job must keep its finalizer until the backup status is recorded
			job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(t.Context(), job.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Contains(t, job.Finalizers, backuphelpers.FinalizerEtcdBackup)
		},
	})
}

func TestPendingStart(t *testing.T) {
	// The backup was started on test-node-1 by an earlier sync, but the informer hasn't observed its job or running
	// status yet, so it still appears queued.
	newPending := func(backup *operatorv1alpha1.EtcdBackup, age time.Duration) pendingStarts {
		return pendingStarts{backup.UID: {
			backupName: backup.Name,
			resources:  backupResources{nodeName: "test-node-1"},
			startedAt:  time.Now().Add(-age),
		}}
	}
	started := func() *operatorv1alpha1.EtcdBackup {
		return testutils.FakeEtcdBackup("started", testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(time.Hour))
	}
	nodes := []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())}

	t.Run("within-ttl", func(t *testing.T) {
		// Neither the started backup nor another queued backup may be started on the held node
		backup := started()
		pending := newPending(backup, time.Second)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup, testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage))},
			nodes:   nodes,
			pending: pending,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				require.Contains(t, pending, backup.UID, "expected pending start to be kept")
				// The API is not consulted while waiting for the informer
				_, ok := testutils.GetAction[k8stesting.GetActionImpl](operatorFake.Actions())
				require.False(t, ok, "expected no get action, found %v", operatorFake.Actions())
			},
		})
	})
	t.Run("expired-job-missing", func(t *testing.T) {
		// The job was never observed, so the backup is restarted
		backup := started()
		pending := newPending(backup, 2*pendingStartTTL)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   nodes,
			pending: pending,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireBackupJobCreated(t, client, backup)
				require.Contains(t, pending, backup.UID, "expected restarted backup to be pending")
				require.WithinDuration(t, time.Now(), pending[backup.UID].startedAt, time.Minute)
			},
		})
	})
	t.Run("expired-job-exists", func(t *testing.T) {
		// The job exists, but was never observed by the informer. Creating it again adopts the existing job.
		backup := started()
		existingJob := &batchv1.Job{ObjectMeta: v1.ObjectMeta{
			Name:            generateBackupJobName(backup),
			Namespace:       operatorclient.TargetNamespace,
			OwnerReferences: []v1.OwnerReference{{Kind: "EtcdBackup", Name: backup.Name, UID: backup.UID}},
		}}
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   nodes,
			pending: newPending(backup, 2*pendingStartTTL),
			k8sReactors: []k8stesting.SimpleReactor{
				{Verb: "create", Resource: "jobs", Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewAlreadyExists(batchv1.Resource("jobs"), existingJob.Name)
				}},
				{Verb: "get", Resource: "jobs", Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, existingJob, nil
				}},
			},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				updated, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.True(t, backuphelpers.IsBackupRunning(updated), "expected backup to be running")
				require.Equal(t, existingJob.Name, updated.Status.JobName)
			},
		})
	})
	t.Run("expired-job-exists-other-owner", func(t *testing.T) {
		backup := started()
		existingJob := &batchv1.Job{ObjectMeta: v1.ObjectMeta{
			Name:            generateBackupJobName(backup),
			Namespace:       operatorclient.TargetNamespace,
			OwnerReferences: []v1.OwnerReference{{Kind: "EtcdBackup", Name: backup.Name, UID: "other-uid"}},
		}}
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   nodes,
			pending: newPending(backup, 2*pendingStartTTL),
			k8sReactors: []k8stesting.SimpleReactor{
				{Verb: "create", Resource: "jobs", Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewAlreadyExists(batchv1.Resource("jobs"), existingJob.Name)
				}},
				{Verb: "get", Resource: "jobs", Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, existingJob, nil
				}},
			},
			expectError: true,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				updated, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.False(t, backuphelpers.IsBackupRunning(updated), "expected backup to not be running")
			},
		})
	})
	t.Run("job-observed", func(t *testing.T) {
		// Once the informer observes the job, the job is the source of truth and the pending start is cleared
		running, runningJob := fakeRunningBackup("started", "test-node-1", localStorage)
		pending := newPending(running, time.Second)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{running, testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage))},
			jobs:    []*batchv1.Job{runningJob},
			nodes:   nodes,
			pending: pending,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				require.Empty(t, pending)
			},
		})
	})
	t.Run("backup-finished", func(t *testing.T) {
		backup := testutils.FakeEtcdBackup("started", testutils.WithBackupStorage(localStorage), testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupCompleted())
		pending := newPending(backup, time.Second)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			nodes:   nodes,
			pending: pending,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				require.Empty(t, pending)
			},
		})
	})
	t.Run("backup-deleted", func(t *testing.T) {
		// The started backup is gone, so its node is released for a queued backup
		pending := newPending(started(), time.Second)
		queued := testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{queued},
			nodes:   nodes,
			pending: pending,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireBackupJobCreated(t, client, queued)
				require.NotContains(t, pending, types.UID("started-uid"))
			},
		})
	})
	t.Run("backup-recreated", func(t *testing.T) {
		// The started backup was force removed and recreated with the same name. The pending start of the deleted backup
		// does not apply to the new one.
		deleted := started()
		deleted.UID = "deleted-uid"
		pending := newPending(deleted, time.Second)
		recreated := testutils.FakeEtcdBackup("started", testutils.WithBackupStorage(localStorage))
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{recreated},
			nodes:   nodes,
			pending: pending,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				jobs := listCreatedJobs(client)
				require.Len(t, jobs, 1, "Expected recreated backup to be started")
				require.Equal(t, "test-node-1", jobs[0].Spec.Template.Spec.NodeName)
				require.NotContains(t, pending, deleted.UID)
				require.Contains(t, pending, recreated.UID)
			},
		})
	})
}

func TestStartedBackupHoldsResourcesInSync(t *testing.T) {
	// A backup started in this sync is recorded as pending, and holds its node for the rest of the sync
	pending := pendingStarts{}
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("first", testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(time.Hour)),
			testutils.FakeEtcdBackup("second", testutils.WithBackupStorage(localStorage))},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
		pending: pending,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 1)
			require.Equal(t, "first", jobs[0].Labels[backuphelpers.LabelEtcdBackup])
			require.Len(t, pending, 1)
			require.Contains(t, pending, types.UID("first-uid"))
			require.Equal(t, backupResources{nodeName: "test-node-1"}, pending["first-uid"].resources)
		},
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
			require.True(t, strings.HasPrefix(jobs[0].Name, "backup-new-"), "expected job to have prefix %q, got %q", "backup-new-", jobs[0].Name)
			require.Equal(t, "test-node-2", jobs[0].Spec.Template.Spec.NodeName, "Expected backup to be assigned to an available node")

			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "new", metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupRunning(backup), "Expected backup to be running")
			require.Equal(t, "test-node-2", backup.Status.NodeName)
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
			require.True(t, strings.HasPrefix(jobs[0].Name, "backup-new-"), "expected job to have prefix %q, got %q", "backup-new-", jobs[0].Name)
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
			require.True(t, strings.HasPrefix(jobs[0].Name, "backup-new-"), "expected job to have prefix %q, got %q", "backup-new-", jobs[0].Name)
			require.Equal(t, "test-node", jobs[0].Spec.Template.Spec.NodeName)

			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), "finished", metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupCompleted(backup), "Expected backup to be completed")
		},
	})
}

func TestConcurrencyOrderByAge(t *testing.T) {
	// Oldest backups are started first, one per node with at most maxConcurrentBackups at once even when additional nodes are available
	backups := make([]*operatorv1alpha1.EtcdBackup, maxConcurrentBackups+1)
	nodes := make([]*corev1.Node, maxConcurrentBackups+1)
	for i := range maxConcurrentBackups + 1 {
		backups[i] = testutils.FakeEtcdBackup(fmt.Sprintf("test-backup-%d", i), testutils.WithBackupStorage(localStorage), testutils.WithBackupAge(time.Duration(i)*time.Hour))
		nodes[i] = testutils.FakeNode(fmt.Sprintf("test-node-%d", i), testutils.WithMasterLabel())
	}

	runBackupControllerTest(t, testCaseBackupController{
		backups: backups,
		nodes:   nodes,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 3)
			backups := map[string]string{}
			nodes := map[string]struct{}{}
			for _, job := range jobs {
				backups[job.Labels[backuphelpers.LabelEtcdBackup]] = job.Spec.Template.Spec.NodeName
				nodes[job.Spec.Template.Spec.NodeName] = struct{}{}
			}
			require.Len(t, nodes, 3, "expected backups to be started on different nodes")
			require.Contains(t, backups, "test-backup-3")
			require.Contains(t, backups, "test-backup-2")
			require.Contains(t, backups, "test-backup-1")
			require.NotContains(t, backups, "test-backup-0")
		},
	})
}

func TestStaleLocalNodeName(t *testing.T) {
	// Backup was started on a node by a previous sync and its job is observed, but the informer still has the backup
	// without a node. The node of the job is held so a queued backup isn't started on the same node.
	running, runningJob := fakeRunningBackup("running", "test-node-1", localStorage)
	queued := testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage))
	staleRunning := testutils.FakeEtcdBackup("running", testutils.WithBackupStorage(localStorage), testutils.WithBackupRunning(runningJob), testutils.WithBackupNodeName(""))

	runBackupControllerTest(t, testCaseBackupController{
		backups:      []*operatorv1alpha1.EtcdBackup{running, queued},
		staleBackups: []*operatorv1alpha1.EtcdBackup{staleRunning, queued},
		jobs:         []*batchv1.Job{runningJob},
		nodes:        []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
		},
	})
}

func TestBackupJobMissingFinalizerReconciled(t *testing.T) {
	// A job without the finalizer is not indexed, so its status must be reconciled from the API rather than waiting for
	// the job to be deleted and failing the backup.
	backup, job := fakeRunningBackup("test-backup", "test-node", localStorage)
	backup.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * jobNotFoundGracePeriod))
	job.Finalizers = nil
	job.Status = batchv1.JobStatus{
		CompletionTime: ptr.To(metav1.Now()),
		Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
	}
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		jobs:    []*batchv1.Job{job},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			updated, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupCompleted(updated), "expected backup to be completed, got %v", updated.Status.Conditions)
			require.False(t, backuphelpers.IsBackupFailed(updated))
		},
	})
}

func TestBackupJobCompletedWithoutFiles(t *testing.T) {
	// A completed job that reports no files keeps completed semantics rather than a failure message
	backup, job := fakeRunningBackup("test-backup", "test-node", localStorage)
	job.Status = batchv1.JobStatus{
		Succeeded:      1,
		CompletionTime: ptr.To(metav1.Now()),
		Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
	}
	pod := testutils.FakePod("backup-pod",
		testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
		testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
		func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  "backup",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"files": []}`}},
			}}
		})
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		jobs:    []*batchv1.Job{job},
		pods:    []*corev1.Pod{pod},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
				Type:    string(operatorv1alpha1.BackupCompleted),
				Reason:  string(operatorv1alpha1.BackupReasonJobCompleted),
				Message: "backup job completed",
				Status:  metav1.ConditionTrue,
			}}, nil)
		},
	})
}

func TestLocalBackupAssignedNodeIneligible(t *testing.T) {
	// A queued backup whose assigned node no longer exists fails. No job ran, so no GC is required.
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(localStorage), testutils.WithBackupNodeName("removed-node"))
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			requireBackupStatusUpdated(t, operatorFake, []metav1.Condition{{
				Type:    string(operatorv1alpha1.BackupFailed),
				Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
				Message: `assigned node "removed-node" is no longer an eligible control plane node`,
				Status:  metav1.ConditionTrue,
			}}, nil)
		},
	})
}

func TestGenerateBackupJobNameIncludesUID(t *testing.T) {
	backup := testutils.FakeEtcdBackup("test-backup")
	recreated := backup.DeepCopy()
	recreated.UID = "recreated-uid"
	require.Equal(t, generateBackupJobName(backup), generateBackupJobName(backup.DeepCopy()), "expected job name to be deterministic")
	require.NotEqual(t, generateBackupJobName(backup), generateBackupJobName(recreated), "expected recreated backup to get a new job name")
	require.True(t, strings.HasPrefix(generateBackupJobName(backup), "backup-test-backup-"))
}

func TestBackupJobFinishedWaitsForPodStatus(t *testing.T) {
	// The job is complete, but the cached pod is still running without its termination message. Recording the outcome
	// now would lose the reported files, so wait for the pod status until the grace period expires.
	newCase := func(completedAgo time.Duration) (*operatorv1alpha1.EtcdBackup, *batchv1.Job, *corev1.Pod) {
		backup, job := fakeRunningBackup("test-backup", "test-node", localStorage)
		job.Status = batchv1.JobStatus{
			Succeeded:      1,
			CompletionTime: ptr.To(metav1.NewTime(time.Now().Add(-completedAgo))),
			Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		}
		pod := testutils.FakePod("backup-pod",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:    "backup",
					Started: ptr.To(true),
					State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}}
			})
		return backup, job, pod
	}

	t.Run("within-grace-period", func(t *testing.T) {
		backup, job, pod := newCase(0)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			pods:    []*corev1.Pod{pod},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.False(t, ok, "expected no status update while waiting for pod status, found %v", operatorFake.Actions())
				updatedJob, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(t.Context(), job.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Contains(t, updatedJob.Finalizers, backuphelpers.FinalizerEtcdBackup, "job must not be finalized")
			},
		})
	})

	t.Run("holds-resources", func(t *testing.T) {
		// The waiting backup still holds its node
		backup, job, pod := newCase(0)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup, testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage))},
			jobs:    []*batchv1.Job{job},
			pods:    []*corev1.Pod{pod},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
			},
		})
	})

	t.Run("grace-period-expired", func(t *testing.T) {
		backup, job, pod := newCase(2 * podStatusGracePeriod)
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{job},
			pods:    []*corev1.Pod{pod},
			nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				updated, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.True(t, backuphelpers.IsBackupCompleted(updated))
				updatedJob, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(t.Context(), job.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NotContains(t, updatedJob.Finalizers, backuphelpers.FinalizerEtcdBackup)
			},
		})
	})
}

func outcomeTestJob(condition batchv1.JobConditionType, succeeded, failed int32) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-job"},
		Status: batchv1.JobStatus{
			Succeeded:  succeeded,
			Failed:     failed,
			Conditions: []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}},
		},
	}
}

// outcomeTestPod returns a terminal pod. started reports whether the backup container started, and message is its
// termination message.
func outcomeTestPod(name string, age time.Duration, phase corev1.PodPhase, started bool, message string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Now().Add(-age))},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if started {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}
	} else {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}}
	}
	return pod
}

func TestDetermineBackupOutcome(t *testing.T) {
	const filesMessage = `{"message": "backup done", "files": [{"path": "/etc/kubernetes/cluster-backup/b/snapshot.db", "sizeBytes": 10}]}`
	const noFilesMessage = `{"message": "etcd unavailable", "files": []}`

	tests := []struct {
		name     string
		job      *batchv1.Job
		pods     []*corev1.Pod
		expected backupOutcome
	}{
		{
			name: "completed with files",
			job:  outcomeTestJob(batchv1.JobComplete, 1, 0),
			pods: []*corev1.Pod{outcomeTestPod("p1", time.Minute, corev1.PodSucceeded, true, filesMessage)},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupCompleted,
				reason:    operatorv1alpha1.BackupReasonJobCompleted,
				message:   "backup done",
				files:     []operatorv1alpha1.EtcdBackupFile{{Path: "/b/snapshot.db", SizeBytes: 10}},
			},
		},
		{
			name: "failed with partial files requires GC",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 1),
			pods: []*corev1.Pod{outcomeTestPod("p1", time.Minute, corev1.PodFailed, true, filesMessage)},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonJobFailed,
				message:   "backup done",
				files:     []operatorv1alpha1.EtcdBackupFile{{Path: "/b/snapshot.db", SizeBytes: 10}},
			},
		},
		{
			name: "failed gracefully on every attempt skips GC",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 2),
			pods: []*corev1.Pod{
				outcomeTestPod("p1", 2*time.Minute, corev1.PodFailed, true, noFilesMessage),
				outcomeTestPod("p2", time.Minute, corev1.PodFailed, true, `{"files": []}`),
			},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonSetupFailed,
				message:   "backup job failed without creating files",
				files:     []operatorv1alpha1.EtcdBackupFile{},
			},
		},
		{
			name: "crash then graceful failure requires GC",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 2),
			pods: []*corev1.Pod{
				outcomeTestPod("p1", 2*time.Minute, corev1.PodFailed, true, ""),
				outcomeTestPod("p2", time.Minute, corev1.PodFailed, true, noFilesMessage),
			},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonJobFailed,
				message:   "etcd unavailable",
				files:     []operatorv1alpha1.EtcdBackupFile{},
			},
		},
		{
			name: "backup container never started skips GC",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 1),
			pods: []*corev1.Pod{outcomeTestPod("p1", time.Minute, corev1.PodFailed, false, "")},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonSetupFailed,
				message:   "failed to verify storage",
			},
		},
		{
			name: "no pods created skips GC",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 0),
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonSetupFailed,
				message:   "backup job failed without creating pods",
			},
		},
		{
			name: "missing pod history requires GC",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 2),
			pods: []*corev1.Pod{outcomeTestPod("p2", time.Minute, corev1.PodFailed, false, "")},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonJobFailed,
				message:   "unable to determine backup job outcome",
			},
		},
		{
			name: "completed without observable pods keeps completed message",
			job:  outcomeTestJob(batchv1.JobComplete, 1, 0),
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupCompleted,
				reason:    operatorv1alpha1.BackupReasonJobCompleted,
			},
		},
		{
			name: "unparseable termination message keeps job reason",
			job:  outcomeTestJob(batchv1.JobFailed, 0, 1),
			pods: []*corev1.Pod{outcomeTestPod("p1", time.Minute, corev1.PodFailed, true, "not json")},
			expected: backupOutcome{
				condition: operatorv1alpha1.BackupFailed,
				reason:    operatorv1alpha1.BackupReasonJobFailed,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, determineBackupOutcome("test-backup", tc.job, tc.pods))
		})
	}
}

func TestConcurrencyLocalSkipsUnavailableNodes(t *testing.T) {
	notReady := func(node *corev1.Node) {
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}
	}
	ready := func(node *corev1.Node) {
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	}
	cordoned := func(node *corev1.Node) { node.Spec.Unschedulable = true }

	t.Run("available-node-selected", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage))},
			nodes: []*corev1.Node{
				testutils.FakeNode("test-node-1", testutils.WithMasterLabel(), notReady),
				testutils.FakeNode("test-node-2", testutils.WithMasterLabel(), ready, cordoned),
				testutils.FakeNode("test-node-3", testutils.WithMasterLabel(), ready)},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				jobs := listCreatedJobs(client)
				require.Len(t, jobs, 1)
				require.Equal(t, "test-node-3", jobs[0].Spec.Template.Spec.NodeName, "Expected backup to skip unavailable nodes")
			},
		})
	})
	t.Run("no-available-node-waits", func(t *testing.T) {
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("new", testutils.WithBackupStorage(localStorage))},
			nodes: []*corev1.Node{
				testutils.FakeNode("test-node-1", testutils.WithMasterLabel(), notReady),
				testutils.FakeNode("test-node-2", testutils.WithMasterLabel(), ready, cordoned)},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				_, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
				require.False(t, ok, "Expected backup to wait for an available node, not be failed")
			},
		})
	})
}

func TestStartDeletedBackup(t *testing.T) {
	// The informer still has the queued backup, but it was deleted. Adding the finalizer fails with NotFound, so no job
	// is created and the backup is not failed.
	backup := testutils.FakeEtcdBackup("deleted")
	runBackupControllerTest(t, testCaseBackupController{
		backups:      []*operatorv1alpha1.EtcdBackup{},
		staleBackups: []*operatorv1alpha1.EtcdBackup{backup},
		nodes:        []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		pvcs:         []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "deleted-pvc")},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
		},
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

	require.Truef(t, strings.HasPrefix(createdJob.Name, "backup-"+backup.Name), "expected job.name [%s] to have prefix [%s]", createdJob.Name, "backup-"+backup.Name)
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

func requireBackupStatusUpdated(
	t *testing.T,
	operatorFake *operatorfake.Clientset,
	expectedConditions []metav1.Condition,
	expectedFiles []operatorv1alpha1.EtcdBackupFile) {
	t.Helper()
	action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](operatorFake.Fake.Actions())
	require.Truef(t, ok, "expected to find at least one status update, but found %v", operatorFake.Fake.Actions())
	backup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
	require.True(t, ok, "expected etcdbackup object")

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
			MountPath: filepath.Join(backuphelpers.BackupPathMount, backup.Spec.Storage.Local.HostPath),
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
			MountPath: backuphelpers.BackupPathMount,
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
