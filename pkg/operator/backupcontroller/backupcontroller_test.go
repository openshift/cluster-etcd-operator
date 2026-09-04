package backupcontroller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

var backupFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
	[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName},
	[]configv1.FeatureGateName{})

type testCaseBackupController struct {
	backups     []*operatorv1alpha1.EtcdBackup
	jobs        []*batchv1.Job
	pods        []*corev1.Pod
	nodes       []*corev1.Node
	pvcs        []*corev1.PersistentVolumeClaim
	expectError bool
	validate    func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
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
	operatorSharedFactory := operatorinformers.NewSharedInformerFactory(operatorFake, 0)

	podsInformer := sharedFactory.Core().V1().Pods()
	jobsInformer := sharedFactory.Batch().V1().Jobs()
	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()

	podsInformerHasSynced := podsInformer.Informer().HasSynced
	jobsInformerHasSynced := jobsInformer.Informer().HasSynced
	backupsInformerHasSynced := backupsInformer.Informer().HasSynced

	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), backupsInformerHasSynced, podsInformerHasSynced, jobsInformerHasSynced)

	controller := BackupController{
		backupsLister:         backupsInformer.Lister(),
		podsLister:            podsInformer.Lister().Pods(operatorclient.TargetNamespace),
		jobsLister:            jobsInformer.Lister().Jobs(operatorclient.TargetNamespace),
		operatorClient:        operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
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

func TestSyncLoopHappyPath(t *testing.T) {
	// Create a backup job for an EtcdBackup
	backup := testutils.FakeEtcdBackup("test-backup", testutils.WithBackupPending("test-node"))
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node")},
		pvcs:    []*corev1.PersistentVolumeClaim{testutils.FakePVC(operatorclient.TargetNamespace, "test-backup-pvc")},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			job := requireBackupJobCreated(t, client, backup)

			action, ok := testutils.GetAction[k8stesting.UpdateActionImpl](operatorFake.Actions())
			require.True(t, ok, "Expected update action")
			require.Equal(t, "update", action.GetVerb())
			updatedBackup := action.Object.(*operatorv1alpha1.EtcdBackup)

			require.Equal(t, updatedBackup.Status.Job, &operatorv1alpha1.EtcdBackupJobReference{
				Name:      job.Name,
				Namespace: job.Namespace,
				UID:       string(job.UID),
			})
		},
	})
}

func TestJobAlreadyRunning(t *testing.T) {
	// Don't create a new backup job when one already exists
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup")},
		jobs: []*batchv1.Job{
			{ObjectMeta: v1.ObjectMeta{
				Name:      "running-backup-job",
				Namespace: operatorclient.TargetNamespace,
				Labels:    map[string]string{"app": backupAppName},
			}}},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
		},
	})
}

func TestJobBackupJobCompleted(t *testing.T) {
	// Completed backup job is processed and does not start a new job
	backup := testutils.FakeEtcdBackup("test-backup")
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "completed-backup-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, labelBackupName: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
		}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		}}},
	}
	pods := []*corev1.Pod{
		testutils.FakePod("failed-backup-job-pod-1",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Time{Time: time.Now().Add(-time.Minute)})),
		testutils.FakePod("failed-backup-job-pod-2",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Now()),
			func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodSucceeded
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "backup",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
							Message:  `{"files": [{"path": "/my/successful/backup.db", "size": "100Mi"}, {"path": "/my/successful/static_kuberesources.tar.gz", "size": "4321"}]}`,
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
			requireBackupUpdated(t, operatorFake, operatorv1alpha1.BackupCompleted, operatorv1alpha1.BackupReasonJobCompleted, fmt.Sprintf("backup job status %s", batchv1.JobComplete), []operatorv1alpha1.EtcdBackupFile{
				{Path: "/my/successful/backup.db", Size: *resource.NewQuantity(100*1024*1024, resource.BinarySI)},
				{Path: "/my/successful/static_kuberesources.tar.gz", Size: *resource.NewQuantity(4321, resource.BinarySI)}})
			requireJobUpdated(t, client, "test-backup")
		},
	})
}

func TestBackupJobFailed(t *testing.T) {
	// Failed job with partially created files is reported in EtcdBackup.Status
	backup := testutils.FakeEtcdBackup("test-backup")
	job := &batchv1.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:       "failed-backup-job",
			Namespace:  operatorclient.TargetNamespace,
			Labels:     map[string]string{"app": backupAppName, labelBackupName: "test-backup"},
			Finalizers: []string{backuphelpers.FinalizerEtcdBackup},
		}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
		}}},
	}
	pods := []*corev1.Pod{
		testutils.FakePod("failed-backup-job-pod-1",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Time{Time: time.Now().Add(-time.Minute)})),
		testutils.FakePod("failed-backup-job-pod-2",
			testutils.WithPodLabels(map[string]string{labelJobName: job.Name}),
			testutils.WithPodOwner(v1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID}),
			testutils.WithCreationTimestamp(v1.Now()),
			func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: "backup",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 1,
							Message:  `{"files": [{"path": "/my/broken/backup.db.part", "size": "12345"}]}`,
						},
					},
				}}
			}),
	}

	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{backup},
		jobs:    []*batchv1.Job{job},
		pods:    pods,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			requireBackupUpdated(t, operatorFake, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonJobFailed, fmt.Sprintf("backup job status %s", batchv1.JobFailed), []operatorv1alpha1.EtcdBackupFile{{
				Path: "/my/broken/backup.db.part",
				Size: *resource.NewQuantity(12345, resource.BinarySI),
			}})
		},
	})
}

func TestPVCNotFound(t *testing.T) {
	// EtcdBackup with missing PVC is handled gracefully
	runBackupControllerTest(t, testCaseBackupController{
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup", testutils.WithBackupPending("test-node"), testutils.WithBackupStorage(operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC:  &operatorv1alpha1.EtcdBackupStoragePvc{Name: "backup-pvc-that-doesnt-exist"},
			}))},
		nodes: []*corev1.Node{testutils.FakeNode("test-node")},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			requireNoBackupJobCreated(t, client)
			requireBackupUpdated(t, operatorFake, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonPVCNotFound, "unable to find PVC [backup-pvc-that-doesnt-exist]", nil)
		},
	})
}

func TestIndexJobsByBackupLabelName(t *testing.T) {
	jobs := []*batchv1.Job{
		{ObjectMeta: v1.ObjectMeta{Name: "test-1", Labels: map[string]string{labelBackupName: "test-1"}, Finalizers: []string{backuphelpers.FinalizerEtcdBackup}}},
		{ObjectMeta: v1.ObjectMeta{Name: "test-2", Labels: map[string]string{labelBackupName: "test-2"}, Finalizers: []string{backuphelpers.FinalizerEtcdBackup}}},
		{ObjectMeta: v1.ObjectMeta{Name: "test-3", Labels: map[string]string{labelBackupName: "test-3"}, Finalizers: []string{backuphelpers.FinalizerEtcdBackup}}},
		{ObjectMeta: v1.ObjectMeta{Name: "test-4", Labels: map[string]string{"some-other-label": "value"}, Finalizers: []string{backuphelpers.FinalizerEtcdBackup}}},
	}
	expected := map[string]*batchv1.Job{}
	expected["test-1"] = jobs[0]
	expected["test-2"] = jobs[1]
	expected["test-3"] = jobs[2]

	m := indexJobsByBackupLabelName(jobs)
	require.Equal(t, expected, m)
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
	require.Equal(t, backup.Name, createdJob.Labels[labelBackupName])
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

func requireBackupUpdated(
	t *testing.T,
	client *operatorfake.Clientset,
	expectedConditionType operatorv1alpha1.BackupConditionType,
	expectedConditionReason operatorv1alpha1.BackupConditionReason,
	expectedConditionMessage string,
	expectedFiles []operatorv1alpha1.EtcdBackupFile) {
	t.Helper()
	action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](client.Fake.Actions())
	require.Truef(t, ok, "expected to find at least one status updateAction, but found %v", client.Fake.Actions())
	b := action.Object.(*operatorv1alpha1.EtcdBackup)
	require.Contains(t, removeTransitionTime(b.Status.Conditions), v1.Condition{
		Type:    string(expectedConditionType),
		Reason:  string(expectedConditionReason),
		Message: expectedConditionMessage,
		Status:  v1.ConditionTrue,
	})
	if expectedFiles != nil {
		require.Len(t, b.Status.Files, len(expectedFiles))
		for i, file := range expectedFiles {
			require.Equal(t, file.Path, b.Status.Files[i].Path)
			require.True(t, file.Size.Equal(b.Status.Files[i].Size))
		}
	}
}

func requireJobUpdated(t *testing.T, client *k8sfakeclient.Clientset, backupName string) {
	t.Helper()
	action, ok := testutils.GetStatusAction[k8stesting.UpdateActionImpl](client.Fake.Actions())
	require.Truef(t, ok, "expected to find at least one status updateAction, but found %v", client.Fake.Actions())
	j := action.Object.(*batchv1.Job)
	require.Equal(t, map[string]string{"app": "cluster-backup-job", labelBackupName: backupName}, j.Labels)
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
