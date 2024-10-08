package backupcontroller

import (
	"context"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	fake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
)

func TestDeletesBackupWithoutJob(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{Name: "test-backup", OwnerReferences: []v1.OwnerReference{
			{Name: "some-job", UID: "123", APIVersion: "batch/v1", Kind: "Job"},
		}},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{}...)

	controller := BackupRemovalController{
		backupsClient:       operatorFake.OperatorV1alpha1(),
		kubeClient:          client,
		featureGateAccessor: backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)

	requireBackupDeleted(t, operatorFake, backup)
}

func TestDoesNotDeleteWhenOwningJobExists(t *testing.T) {
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: v1.ObjectMeta{Name: "test-backup", OwnerReferences: []v1.OwnerReference{
			{Name: "some-job", UID: "123", APIVersion: "batch/v1", Kind: "Job"},
		}},
		Spec: operatorv1alpha1.EtcdBackupSpec{PVCName: "backup-happy-path-pvc"}}
	operatorFake := fake.NewSimpleClientset([]runtime.Object{&backup}...)
	job := batchv1.Job{ObjectMeta: v1.ObjectMeta{
		Name:      "some-job",
		Namespace: operatorclient.TargetNamespace,
		Labels:    map[string]string{"app": cronJobBackupAppLabel},
		UID:       types.UID("123"),
	}}
	client := k8sfakeclient.NewSimpleClientset([]runtime.Object{&job}...)

	controller := BackupRemovalController{
		backupsClient:       operatorFake.OperatorV1alpha1(),
		kubeClient:          client,
		featureGateAccessor: backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)

	requireNothingDeleted(t, operatorFake)
}

func requireNothingDeleted(t *testing.T, client *fake.Clientset) {
	for _, action := range client.Fake.Actions() {
		if _, ok := action.(k8stesting.DeleteActionImpl); ok {
			t.Fatalf("expected to find no deleteAction, but found %v", client.Fake.Actions())

		}
	}
}

func requireBackupDeleted(t *testing.T, client *fake.Clientset, backup operatorv1alpha1.EtcdBackup) {
	var deleteAction *k8stesting.DeleteActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.DeleteActionImpl); ok {
			deleteAction = &a
			break
		}
	}

	require.NotNilf(t, deleteAction, "expected to find at least one deleteAction, but found %v", client.Fake.Actions())
	require.Equal(t, backup.Name, deleteAction.GetName())
	require.Equal(t, backup.Namespace, deleteAction.GetNamespace())
}
