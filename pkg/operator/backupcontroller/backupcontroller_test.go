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
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
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
	"k8s.io/apimachinery/pkg/util/validation/field"
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
	activeCache      *activeBackupCache
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

	activeCache := newActiveBackupCache()
	if tc.activeCache != nil {
		activeCache = *tc.activeCache
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
	unknownExecution := func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
		requireNoBackupJobCreated(t, client)
		requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
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
	t.Run("pod-history-incomplete", func(t *testing.T) {
		// Job accounted for more pods than are observable, a missing pod may have written files
		jobWithRetries := job.DeepCopy()
		jobWithRetries.Status.Failed = 2
		runBackupControllerTest(t, testCaseBackupController{
			backups: []*operatorv1alpha1.EtcdBackup{backup},
			jobs:    []*batchv1.Job{jobWithRetries},
			pods: []*corev1.Pod{testutils.FakePod("failed-backup-job-pod-init",
				testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
				testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
				testutils.WithCreationTimestamp(v1.Now()),
				testutils.WithPodStatus(corev1.PodFailed))},
			validate: unknownExecution,
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
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
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
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
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
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
					Message: "failed to verify storage",
					Status:  metav1.ConditionTrue,
				}}, nil)
			},
		})
	})
}

func TestBackupGracefulFailureAfterCrashRequiresGC(t *testing.T) {
	// The first attempt started and crashed without a parseable termination message, so it may have written partial
	// files. A later attempt failing gracefully without files must not cause GC to be skipped.
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "failed-backup-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, backuphelpers.LabelEtcdBackup: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeName: "test-node"}}},
		Status: batchv1.JobStatus{Failed: 2, Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue}}}}
	failedPod := func(name string, age time.Duration, message string) *corev1.Pod {
		return testutils.FakePod(name,
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Time{Time: time.Now().Add(-age)}),
			testutils.WithPodStatus(corev1.PodFailed),
			func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Started: new(true),
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1, Message: message,
					}}}}
			})
	}

	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup", testutils.WithBackupRunning(job))},
		jobs: []*batchv1.Job{job},
		pods: []*corev1.Pod{
			// FallbackToLogsOnError reports the log tail of a crashed container
			failedPod("failed-backup-job-pod-1", 2*time.Minute, "fatal error: runtime: out of memory"),
			failedPod("failed-backup-job-pod-2", time.Minute, `{"files": []}`),
		},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
				Type:   string(operatorv1alpha1.BackupFailed),
				Reason: string(operatorv1alpha1.BackupReasonJobFailed),
				// Not every attempt confirmed it created no files, so the message must not claim it
				Message: "backup job failed",
				Status:  metav1.ConditionTrue,
			}}, nil)
		},
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
			requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
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
				_, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.True(t, ok, "expected new backup to be started")
				backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.False(t, backuphelpers.IsBackupFinished(backup), "new backup must not inherit the old job's result")
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
			_, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
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
			Verb:     "patch",
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

func TestApplyBackupStatusRequiresResourceVersion(t *testing.T) {
	backup := testutils.FakeEtcdBackup("test-backup", func(backup *operatorv1alpha1.EtcdBackup) {
		backup.ResourceVersion = "42"
	})
	_, job := fakeRunningBackup("test-backup", "test-node", localStorage)

	appliedResourceVersion := func(t *testing.T, apply func(client operatorv1alpha1client.EtcdBackupInterface)) string {
		t.Helper()
		operatorFake := operatorfake.NewSimpleClientset(backup)
		apply(operatorFake.OperatorV1alpha1().EtcdBackups())
		action, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
		require.True(t, ok, "expected status patch, found %v", operatorFake.Actions())
		patch := map[string]any{}
		require.NoError(t, json.Unmarshal(action.GetPatch(), &patch))
		metadata, _ := patch["metadata"].(map[string]any)
		resourceVersion, _ := metadata["resourceVersion"].(string)
		return resourceVersion
	}

	require.Equal(t, "42", appliedResourceVersion(t, func(client operatorv1alpha1client.EtcdBackupInterface) {
		_, _ = applyBackupFinished(t.Context(), client, backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonJobFailed, "", nil)
	}))
	// Running must not replace a finished status applied since the backup was read
	require.Equal(t, "42", appliedResourceVersion(t, func(client operatorv1alpha1client.EtcdBackupInterface) {
		_, _ = applyBackupRunning(t.Context(), client, backup, job)
	}))
}

func TestInvalidBackupJobFailsBackup(t *testing.T) {
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
		Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
		Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/var/../backups"},
	}))
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
		operatorReactors: []k8stesting.SimpleReactor{{
			// Adding the finalizer updates the backup's resource version
			Verb:     "patch",
			Resource: "etcdbackups",
			Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "" {
					return false, nil, nil
				}
				patched := backup.DeepCopy()
				patched.Finalizers = append(patched.Finalizers, backuphelpers.FinalizerEtcdBackup)
				patched.ResourceVersion = "finalizer-added"
				return true, patched, nil
			},
		}},
		k8sReactors: []k8stesting.SimpleReactor{{
			Verb:     "create",
			Resource: "jobs",
			Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInvalid(batchv1.SchemeGroupVersion.WithKind("Job").GroupKind(), "backup-test-backup",
					field.ErrorList{field.Invalid(field.NewPath("spec", "template", "spec", "volumes").Index(0).Child("hostPath", "path"), "/var/../backups", "must not contain '..'")})
			},
		}},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			backup, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, backuphelpers.IsBackupFailed(backup), "expected backup to fail, got %v", backup.Status.Conditions)
			condition := backup.Status.Conditions[0]
			require.Equal(t, string(operatorv1alpha1.BackupReasonSetupFailed), condition.Reason)
			require.Contains(t, condition.Message, "must not contain '..'")

			// The failed status must be applied to the version of the backup with the finalizer, or it would conflict
			statusPatches := testutils.ListActions[k8stesting.PatchActionImpl](operatorFake.Actions(), func(a k8stesting.PatchActionImpl) bool {
				return a.GetSubresource() == "status"
			})
			require.NotEmpty(t, statusPatches)
			patch := map[string]any{}
			require.NoError(t, json.Unmarshal(statusPatches[len(statusPatches)-1].GetPatch(), &patch))
			metadata, _ := patch["metadata"].(map[string]any)
			require.Equal(t, "finalizer-added", metadata["resourceVersion"])
		},
	})
}

func TestActiveCacheStaleQueuedBackup(t *testing.T) {
	// The backup was started by an earlier sync, but its running status was never recorded and its job no longer
	// exists. Once the grace period has passed, the backup is failed and its reservation released. It is unknown
	// whether the job created any files, so the failure reason must not skip garbage collection.
	newCache := func(backup *operatorv1alpha1.EtcdBackup, age time.Duration) *activeBackupCache {
		activeCache := newActiveBackupCache()
		activeCache.add(backup)
		active := activeCache.backups[backup.Name]
		active.addedAt = time.Now().Add(-age)
		activeCache.backups[backup.Name] = active
		return &activeCache
	}
	queued := func() *operatorv1alpha1.EtcdBackup {
		return testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage), testutils.WithBackupNodeName("test-node-1"))
	}
	nodes := []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())}

	t.Run("within-grace-period", func(t *testing.T) {
		backup := queued()
		runBackupControllerTest(t, testCaseBackupController{
			backups:     []*operatorv1alpha1.EtcdBackup{backup},
			nodes:       nodes,
			activeCache: newCache(backup, time.Second),
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
			},
		})
	})
	t.Run("after-grace-period", func(t *testing.T) {
		backup := queued()
		activeCache := newCache(backup, 2*jobNotFoundGracePeriod)
		runBackupControllerTest(t, testCaseBackupController{
			backups:     []*operatorv1alpha1.EtcdBackup{backup},
			nodes:       nodes,
			activeCache: activeCache,
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
				requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
					Type:    string(operatorv1alpha1.BackupFailed),
					Reason:  string(operatorv1alpha1.BackupReasonJobFailed),
					Message: fmt.Sprintf("unable to find job %q", generateBackupJobName(backup)),
					Status:  metav1.ConditionTrue,
				}}, nil)
				require.False(t, activeCache.isActive(backup.Name), "expected reservation to be released")
				require.False(t, activeCache.local.inUse("test-node-1"))
			},
		})
	})
	t.Run("after-grace-period-job-exists", func(t *testing.T) {
		// Job exists but hasn't been observed by the informer, keep the reservation
		backup := queued()
		existingJob := &batchv1.Job{ObjectMeta: v1.ObjectMeta{Name: generateBackupJobName(backup), Namespace: operatorclient.TargetNamespace}}
		runBackupControllerTest(t, testCaseBackupController{
			backups:     []*operatorv1alpha1.EtcdBackup{backup},
			nodes:       nodes,
			activeCache: newCache(backup, 2*jobNotFoundGracePeriod),
			k8sReactors: []k8stesting.SimpleReactor{{
				Verb:     "get",
				Resource: "jobs",
				Reaction: func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, existingJob, nil
				},
			}},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				requireNoBackupJobCreated(t, client)
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
			require.True(t, strings.HasPrefix(jobs[0].Name, "backup-new-"), "expected job to have prefix %q, got %q", "backup-new-", jobs[0].Name)
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
			backups := map[string]string{}
			for _, job := range jobs {
				backups[job.Labels[backuphelpers.LabelEtcdBackup]] = job.Spec.Template.Spec.NodeName
			}
			require.Contains(t, backups, "test-backup-1")
			require.Contains(t, backups, "test-backup-2")
			require.NotEqual(t, backups["test-backup-1"], backups["test-backup-2"], "Expected backups to be started on different nodes")
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

func TestActiveCacheStaleLocalNodeName(t *testing.T) {
	// Backup was started on a node by a previous sync and its job is observed, but the informer still has the backup
	// without a node. The node reservation must be kept so a queued backup isn't started on the same node.
	running, runningJob := fakeRunningBackup("running", "test-node-1", localStorage)
	queued := testutils.FakeEtcdBackup("queued", testutils.WithBackupStorage(localStorage))
	staleRunning := testutils.FakeEtcdBackup("running", testutils.WithBackupStorage(localStorage), testutils.WithBackupRunning(runningJob), testutils.WithBackupNodeName(""))

	for name, cached := range map[string]bool{"cached": true, "not cached": false} {
		t.Run(name, func(t *testing.T) {
			activeCache := newActiveBackupCache()
			if cached {
				activeCache.add(running)
			}
			runBackupControllerTest(t, testCaseBackupController{
				backups:      []*operatorv1alpha1.EtcdBackup{running, queued},
				staleBackups: []*operatorv1alpha1.EtcdBackup{staleRunning, queued},
				jobs:         []*batchv1.Job{runningJob},
				nodes:        []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
				activeCache:  &activeCache,
				validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
					requireNoBackupJobCreated(t, client)
					require.True(t, activeCache.local.inUse("test-node-1"), "Expected node reservation to be kept")
					require.Equal(t, "test-node-1", activeCache.backups["running"].nodeName)
				},
			})
		})
	}
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

func TestApplyBackupStatusOmitsEmptyFileSize(t *testing.T) {
	// sizeBytes must be at least 1 when set, so empty (e.g. partial) files must omit it or the apply is rejected
	backup := testutils.FakeEtcdBackup("test-backup")
	operatorFake := operatorfake.NewSimpleClientset(backup)
	_, err := applyBackupFinished(t.Context(), operatorFake.OperatorV1alpha1().EtcdBackups(), backup, nil,
		operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonJobFailed, "", []operatorv1alpha1.EtcdBackupFile{
			{Path: "/backups/test-backup/snapshot.db", SizeBytes: 100},
			{Path: "/backups/test-backup/static_kuberesources.tar.gz", SizeBytes: 0},
		})
	require.NoError(t, err)

	action, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
	require.True(t, ok, "expected status patch, found %v", operatorFake.Actions())
	patch := struct {
		Status struct {
			Files []map[string]any `json:"files"`
		} `json:"status"`
	}{}
	require.NoError(t, json.Unmarshal(action.GetPatch(), &patch))
	require.Equal(t, []map[string]any{
		{"path": "/backups/test-backup/snapshot.db", "sizeBytes": float64(100)},
		{"path": "/backups/test-backup/static_kuberesources.tar.gz"},
	}, patch.Status.Files)
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
			requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
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
			requireBackupStatusApplied(t, operatorFake, []metav1.Condition{{
				Type:    string(operatorv1alpha1.BackupFailed),
				Reason:  string(operatorv1alpha1.BackupReasonSetupFailed),
				Message: `assigned node "removed-node" is no longer an eligible control plane node`,
				Status:  metav1.ConditionTrue,
			}}, nil)
		},
	})
}

func TestBackupJobExistsIgnoresPreviousOwner(t *testing.T) {
	backup := testutils.FakeEtcdBackup("test-backup")
	jobFor := func(ownerUID types.UID) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: v1.ObjectMeta{
			Name:            generateBackupJobName(backup),
			Namespace:       operatorclient.TargetNamespace,
			OwnerReferences: []v1.OwnerReference{{Kind: "EtcdBackup", Name: backup.Name, UID: ownerUID}},
		}}
	}
	for _, tc := range []struct {
		name     string
		ownerUID types.UID
		expected bool
	}{
		{name: "same-owner", ownerUID: backup.UID, expected: true},
		{name: "previous-owner", ownerUID: "previous-uid", expected: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &BackupController{kubeClient: k8sfakeclient.NewSimpleClientset(jobFor(tc.ownerUID))}
			exists, err := c.backupJobExists(t.Context(), backup)
			require.NoError(t, err)
			require.Equal(t, tc.expected, exists)
		})
	}
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
				_, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.False(t, ok, "expected no status patch while waiting for pod status, found %v", operatorFake.Actions())
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

func TestActiveCacheRecreatedBackup(t *testing.T) {
	// A backup was force removed while active and recreated with the same name. The stale cache entry of the deleted
	// backup must not cause the new backup to be failed as if its job went missing.
	backup := testutils.FakeEtcdBackup("recreated", testutils.WithBackupStorage(localStorage))
	activeCache := newActiveBackupCache()
	activeCache.add(testutils.FakeEtcdBackup("recreated", testutils.WithBackupStorage(localStorage), testutils.WithBackupNodeName("test-node-1"),
		func(b *operatorv1alpha1.EtcdBackup) { b.UID = "deleted-uid" }))
	active := activeCache.backups[backup.Name]
	active.addedAt = time.Now().Add(-2 * jobNotFoundGracePeriod)
	activeCache.backups[backup.Name] = active

	runBackupControllerTest(t, testCaseBackupController{
		backups:     []*operatorv1alpha1.EtcdBackup{backup},
		nodes:       []*corev1.Node{testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
		activeCache: &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			jobs := listCreatedJobs(client)
			require.Len(t, jobs, 1, "Expected recreated backup to be started")
			require.Equal(t, "test-node-1", jobs[0].Spec.Template.Spec.NodeName)

			updated, err := operatorFake.OperatorV1alpha1().EtcdBackups().Get(t.Context(), backup.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.False(t, backuphelpers.IsBackupFailed(updated), "Expected recreated backup to not be failed")
			require.Equal(t, backup.UID, activeCache.backups[backup.Name].uid)
		},
	})
}

func TestActiveCachePruneRecreatedBackup(t *testing.T) {
	// The cached backup is gone from the informer, and a backup with the same name but a different UID exists in the
	// API. The reservation of the deleted backup is released.
	activeCache := newActiveBackupCache()
	deleted, _ := fakeRunningBackup("recreated", "test-node-1", localStorage)
	deleted.UID = "deleted-uid"
	activeCache.add(deleted)

	recreated := testutils.FakeEtcdBackup("recreated", testutils.WithBackupStorage(localStorage))
	runBackupControllerTest(t, testCaseBackupController{
		backups:      []*operatorv1alpha1.EtcdBackup{recreated},
		staleBackups: []*operatorv1alpha1.EtcdBackup{},
		activeCache:  &activeCache,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			require.False(t, activeCache.isActive("recreated"))
			require.False(t, activeCache.local.inUse("test-node-1"))
		},
	})
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
				_, ok := testutils.GetStatusAction[k8stesting.PatchActionImpl](operatorFake.Actions())
				require.False(t, ok, "Expected backup to wait for an available node, not be failed")
			},
		})
	})
}

func TestApplyBackupFinalizerCoOwned(t *testing.T) {
	// Another field manager co-owns the finalizer, so releasing ownership with apply leaves it in place. The finalizer
	// must be removed with a JSON patch instead.
	backup := testutils.FakeEtcdBackup("test-backup", func(b *operatorv1alpha1.EtcdBackup) {
		b.Finalizers = []string{"other", backuphelpers.FinalizerEtcdBackup}
		b.ManagedFields = []metav1.ManagedFieldsEntry{{
			Manager:    backupFinalizerFieldManager,
			Operation:  metav1.ManagedFieldsOperationApply,
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:finalizers":{"v:\"` + backuphelpers.FinalizerEtcdBackup + `\"":{}}}}`)},
		}}
	})
	operatorFake := operatorfake.NewSimpleClientset(backup)
	var jsonPatch []byte
	operatorFake.PrependReactor("patch", "etcdbackups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		switch patch.GetPatchType() {
		case types.ApplyPatchType:
			// Co-owned finalizer is kept
			return true, backup.DeepCopy(), nil
		case types.JSONPatchType:
			jsonPatch = patch.GetPatch()
			updated := backup.DeepCopy()
			updated.Finalizers = []string{"other"}
			return true, updated, nil
		}
		return false, nil, nil
	})

	updated, err := applyBackupFinalizer(t.Context(), operatorFake.OperatorV1alpha1().EtcdBackups(), backup, false)
	require.NoError(t, err)
	require.NotContains(t, updated.Finalizers, backuphelpers.FinalizerEtcdBackup)
	require.JSONEq(t, `[{"op":"test","path":"/metadata/finalizers/1","value":"`+backuphelpers.FinalizerEtcdBackup+`"},{"op":"remove","path":"/metadata/finalizers/1"}]`, string(jsonPatch))
}
