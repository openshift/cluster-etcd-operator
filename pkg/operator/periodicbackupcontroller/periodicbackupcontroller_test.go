package periodicbackupcontroller

import (
	"context"
	configv1 "github.com/openshift/api/config/v1"
	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"testing"

	fake "github.com/openshift/client-go/config/clientset/versioned/fake"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var backupFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
	[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName},
	[]configv1.FeatureGateName{})

func TestSyncLoopHappyPath(t *testing.T) {
	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewSimpleClientset()

	controller := PeriodicBackupController{
		backupsClient:       operatorFake.ConfigV1alpha1(),
		kubeClient:          client,
		operatorImage:       "pullspec-image",
		featureGateAccessor: backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupCronJobCreated(t, client, backup)
}

func TestSyncLoopExistingCronJob(t *testing.T) {
	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	cronJob := batchv1.CronJob{ObjectMeta: v1.ObjectMeta{Name: "test-backup", Namespace: operatorclient.TargetNamespace}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&cronJob}...)

	controller := PeriodicBackupController{
		backupsClient:       operatorFake.ConfigV1alpha1(),
		kubeClient:          client,
		operatorImage:       "pullspec-image",
		featureGateAccessor: backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupCronJobUpdated(t, client, backup)
}

func requireBackupCronJobCreated(t *testing.T, client *k8sfakeclient.Clientset, backup backupv1alpha1.Backup) {
	createAction := findFirstCreateAction(client)
	require.NotNilf(t, createAction, "expected to find at least one createAction, but found %v", client.Fake.Actions())
	require.Equal(t, operatorclient.TargetNamespace, createAction.GetNamespace())
	require.Equal(t, "create", createAction.GetVerb())
	createdCronJob := createAction.Object.(*batchv1.CronJob)

	require.Equal(t, backup.Name, createdCronJob.Name)
	require.Equal(t, backup.Spec.EtcdBackupSpec.Schedule, createdCronJob.Spec.Schedule)
	require.Equal(t, backup.Spec.EtcdBackupSpec.TimeZone, *createdCronJob.Spec.TimeZone)
	require.Equal(t, operatorclient.TargetNamespace, createdCronJob.Namespace)
	require.Equal(t, "pullspec-image", createdCronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image)
	require.Equal(t, 1, len(createdCronJob.OwnerReferences))
	require.Equal(t, v1.OwnerReference{Name: backup.Name}, createdCronJob.OwnerReferences[0])
}

func requireBackupCronJobUpdated(t *testing.T, client *k8sfakeclient.Clientset, backup backupv1alpha1.Backup) {
	var updateAction *k8stesting.UpdateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.UpdateActionImpl); ok {
			updateAction = &a
			break
		}
	}

	require.NotNilf(t, updateAction, "expected to find at least one updateAction, but found %v", client.Fake.Actions())
	j := updateAction.Object.(*batchv1.CronJob)
	require.Equal(t, backup.Name, j.Name)
	require.Equal(t, backup.Spec.EtcdBackupSpec.Schedule, j.Spec.Schedule)
	require.Equal(t, backup.Spec.EtcdBackupSpec.TimeZone, *j.Spec.TimeZone)
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
