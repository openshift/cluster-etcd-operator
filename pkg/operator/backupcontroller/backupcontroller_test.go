package backupcontroller

import (
	"context"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	fake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var backupFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
	[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName},
	[]configv1.FeatureGateName{})

func TestSyncLoopHappyPath(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: v1.ObjectMeta{Name: "backup-happy-path-pvc", Namespace: operatorclient.TargetNamespace},
	}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&pvc}...)

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupJobCreated(t, client, backup)
}

func TestJobAlreadyRunning(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	// a job without conditions is supposed to be running/pending
	job := batchv1.Job{ObjectMeta: v1.ObjectMeta{
		Name:      "running-backup-job",
		Namespace: operatorclient.TargetNamespace,
		Labels:    map[string]string{"app": backupLabel},
	}}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&job}...)

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireNoBackupJobCreated(t, client)
}

func TestJobBackupJobFinished(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	job := batchv1.Job{ObjectMeta: v1.ObjectMeta{
		Name:      "completed-backup-job",
		Namespace: operatorclient.TargetNamespace,
		Labels:    map[string]string{"app": backupLabel, backupJobLabel: backup.Name},
	}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		},
	}}}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&job}...)

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireNoBackupJobCreated(t, client)
	requireBackupUpdated(t, operatorFake, string(operatorv1alpha1.BackupCompleted), string(batchv1.JobComplete))
	requireJobUpdated(t, client, backup.Name)
}

func TestJobWithoutBackupRemovesJob(t *testing.T) {
	operatorFake := fake.NewSimpleClientset()
	job := batchv1.Job{ObjectMeta: v1.ObjectMeta{
		Name:      "completed-backup-job",
		Namespace: operatorclient.TargetNamespace,
		Labels:    map[string]string{"app": backupLabel, backupJobLabel: "some-backup"},
	}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{
			Type:   batchv1.JobComplete,
			Status: corev1.ConditionTrue,
		},
	}}}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&job}...)

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireNoBackupJobCreated(t, client)
	requireJobDeleted(t, client, job)
}

func TestJobCreationHappyPath(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewSimpleClientset()

	err := createBackupJob(context.Background(),
		backup,
		"operator-pullspec-image",
		client.BatchV1().Jobs(operatorclient.TargetNamespace),
		operatorFake.OperatorV1alpha1().EtcdBackups(),
	)
	require.NoError(t, err)
	requireBackupJobCreated(t, client, backup)

	actions := operatorFake.Actions()
	require.Equal(t, 1, len(actions))
	updateAction := actions[0].(k8stesting.UpdateActionImpl)
	require.Equal(t, "update", updateAction.GetVerb())
	updatedStatus := updateAction.Object.(*operatorv1alpha1.EtcdBackup)

	require.Equal(t, 1, len(updatedStatus.Status.Conditions))
	require.Equal(t, string(operatorv1alpha1.BackupPending), updatedStatus.Status.Conditions[0].Type)
	require.Equal(t, string(operatorv1alpha1.BackupPending), updatedStatus.Status.Conditions[0].Reason)
	require.Contains(t, updatedStatus.Status.Conditions[0].Message, "cluster-backup-job-")
}

func TestMultipleBackupsAreSkipped(t *testing.T) {
	backup1 := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup-1"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	backup2 := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup-2"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	backup3 := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup-3"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup1, &backup2, &backup3}...)
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: v1.ObjectMeta{Name: "backup-happy-path-pvc", Namespace: operatorclient.TargetNamespace},
	}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&pvc}...)

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupJobCreated(t, client, backup1)
	requireBackupJobSkipped(t, operatorFake, backup2)
	requireBackupJobSkipped(t, operatorFake, backup3)
}

func TestIndexJobsByBackupLabelName(t *testing.T) {
	jobList := &batchv1.JobList{
		Items: []batchv1.Job{
			{ObjectMeta: v1.ObjectMeta{Name: "test-1", Labels: map[string]string{backupJobLabel: "test-1"}}},
			{ObjectMeta: v1.ObjectMeta{Name: "test-2", Labels: map[string]string{backupJobLabel: "test-2"}}},
			{ObjectMeta: v1.ObjectMeta{Name: "test-3", Labels: map[string]string{backupJobLabel: "test-3"}}},
			{ObjectMeta: v1.ObjectMeta{Name: "test-4", Labels: map[string]string{"some-other-label": "value"}}},
		},
	}
	expected := map[string]batchv1.Job{}
	expected["test-1"] = jobList.Items[0]
	expected["test-2"] = jobList.Items[1]
	expected["test-3"] = jobList.Items[2]

	m := indexJobsByBackupLabelName(jobList)
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

func TestPVCNotFound(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-pvc-that-doesnt-exist"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewSimpleClientset()

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireNoBackupJobCreated(t, client)
	requireBackupUpdated(t, operatorFake, string(operatorv1alpha1.BackupFailed), "unable to find PVC [backup-pvc-that-doesnt-exist]")
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

func TestOwnerRefsPropagate(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-backup",
			OwnerReferences: []v1.OwnerReference{{
				Name: "some-cronjob-we-inject",
				UID:  "123",
			}}},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: v1.ObjectMeta{Name: "backup-happy-path-pvc", Namespace: operatorclient.TargetNamespace},
	}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&pvc}...)

	controller := BackupController{
		backupsClient:         operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "operator-pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupJobCreated(t, client, backup)
}

func requireNoBackupJobCreated(t *testing.T, client *k8sfakeclient.Clientset) {
	createAction := findFirstCreateAction(client)
	require.Nilf(t, createAction, "expected to not find one createAction, but found %v", client.Fake.Actions())
}

func requireBackupJobCreated(t *testing.T, client *k8sfakeclient.Clientset, backup operatorv1alpha1.EtcdBackup) {
	createAction := findFirstCreateAction(client)
	require.NotNilf(t, createAction, "expected to find at least one createAction, but found %v", client.Fake.Actions())
	require.Equal(t, operatorclient.TargetNamespace, createAction.GetNamespace())
	require.Equal(t, "create", createAction.GetVerb())
	createdJob := createAction.Object.(*batchv1.Job)

	require.Truef(t, strings.HasPrefix(createdJob.Name, "cluster-backup-job-"), "expected job.name [%s] to have prefix [cluster-backup-job]", createdJob.Name)
	require.Equal(t, operatorclient.TargetNamespace, createdJob.Namespace)
	require.Equal(t, backup.Name, createdJob.Labels[backupJobLabel])
	require.Equal(t, "operator-pullspec-image", createdJob.Spec.Template.Spec.InitContainers[0].Image)
	require.Equal(t, "operator-pullspec-image", createdJob.Spec.Template.Spec.Containers[0].Image)

	foundVolume := false
	for _, volume := range createdJob.Spec.Template.Spec.Volumes {
		if volume.Name == "etc-kubernetes-cluster-backup" {
			foundVolume = true
			require.Equal(t, backup.Spec.PVCName, volume.PersistentVolumeClaim.ClaimName)
		}
	}

	require.Truef(t, foundVolume, "could not find injected PVC volume in %v", createdJob.Spec.Template.Spec.Volumes)
	require.Equal(t, len(backup.OwnerReferences)+1, len(createdJob.OwnerReferences))
	require.Equal(t, v1.OwnerReference{Name: backup.Name}, createdJob.OwnerReferences[0])
	for i := 0; i < len(backup.OwnerReferences); i++ {
		require.Equal(t, backup.OwnerReferences[i], createdJob.OwnerReferences[i+1])
	}
}

func requireBackupJobSkipped(t *testing.T, client *fake.Clientset, backup operatorv1alpha1.EtcdBackup) {
	var updateAction *k8stesting.UpdateActionImpl
	var updateStatusAction *k8stesting.UpdateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.UpdateActionImpl); ok && a.Subresource == "status" {
			if b, ok := a.Object.(*operatorv1alpha1.EtcdBackup); ok && b.Name == backup.Name {
				updateStatusAction = &a
			}
		} else if a, ok := action.(k8stesting.UpdateActionImpl); ok && a.Subresource == "" {
			if b, ok := a.Object.(*operatorv1alpha1.EtcdBackup); ok && b.Name == backup.Name {
				updateAction = &a
			}
		}
	}

	require.NotNilf(t, updateStatusAction, "expected to find at least one status updateAction matching the backup name, but found %v", client.Fake.Actions())
	require.NotNilf(t, updateAction, "expected to find at least one updateAction matching the backup name, but found %v", client.Fake.Actions())

	b := updateStatusAction.Object.(*operatorv1alpha1.EtcdBackup)
	require.Equal(t, []v1.Condition{
		{
			Type:    string(operatorv1alpha1.BackupSkipped),
			Reason:  string(operatorv1alpha1.BackupSkipped),
			Message: "skipped due too many simultaneous backups",
			Status:  v1.ConditionTrue,
		},
	}, removeTransitionTime(b.Status.Conditions))

	b = updateAction.Object.(*operatorv1alpha1.EtcdBackup)
	require.Equal(t, map[string]string{"state": "processed"}, b.Labels)
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

func requireBackupUpdated(t *testing.T, client *fake.Clientset, expectedConditionType string, expectedConditionMessage string) {
	var updateAction *k8stesting.UpdateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.UpdateActionImpl); ok && a.Subresource == "" {
			updateAction = &a
			break
		}
	}

	require.NotNilf(t, updateAction, "expected to find at least one updateAction, but found %v", client.Fake.Actions())
	b := updateAction.Object.(*operatorv1alpha1.EtcdBackup)
	require.Equal(t, map[string]string{"state": "processed"}, b.Labels)

	var updateStatusAction *k8stesting.UpdateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.UpdateActionImpl); ok && a.Subresource == "status" {
			updateStatusAction = &a
			break
		}
	}

	require.NotNilf(t, updateStatusAction, "expected to find at least one status updateAction, but found %v", client.Fake.Actions())
	b = updateStatusAction.Object.(*operatorv1alpha1.EtcdBackup)
	require.Equal(t, []v1.Condition{
		{
			Type:    expectedConditionType,
			Reason:  expectedConditionType,
			Message: expectedConditionMessage,
			Status:  v1.ConditionTrue,
		},
	}, removeTransitionTime(b.Status.Conditions))
}

func requireJobUpdated(t *testing.T, client *k8sfakeclient.Clientset, backupName string) {
	var updateAction *k8stesting.UpdateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.UpdateActionImpl); ok {
			updateAction = &a
			break
		}
	}

	require.NotNilf(t, updateAction, "expected to find at least one updateAction, but found %v", client.Fake.Actions())
	j := updateAction.Object.(*batchv1.Job)
	require.Equal(t, map[string]string{"app": "cluster-backup-job", "backup-name": backupName, "state": "processed"}, j.Labels)
}

func requireJobDeleted(t *testing.T, client *k8sfakeclient.Clientset, job batchv1.Job) {
	var deleteAction *k8stesting.DeleteActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.DeleteActionImpl); ok {
			deleteAction = &a
			break
		}
	}

	require.NotNilf(t, deleteAction, "expected to find at least one deleteAction, but found %v", client.Fake.Actions())
	require.Equal(t, job.Name, deleteAction.GetName())
	require.Equal(t, job.Namespace, deleteAction.GetNamespace())
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
