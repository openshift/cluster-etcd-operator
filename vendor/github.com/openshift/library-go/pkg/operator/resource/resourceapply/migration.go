package resourceapply

import (
	"context"
	"reflect"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kube-storage-version-migrator/pkg/apis/migration/v1alpha1"
	migrationv1alpha1 "sigs.k8s.io/kube-storage-version-migrator/pkg/apis/migration/v1alpha1"
	migrationclientv1alpha1 "sigs.k8s.io/kube-storage-version-migrator/pkg/clients/clientset"
)

// ApplyStorageVersionMigration merges objectmeta and required data.
func ApplyStorageVersionMigration(client migrationclientv1alpha1.Interface, shouldDelete bool, recorder events.Recorder, required *migrationv1alpha1.StorageVersionMigration) (*migrationv1alpha1.StorageVersionMigration, bool, error) {
	clientInterface := client.MigrationV1alpha1().StorageVersionMigrations()
	existing, err := clientInterface.Get(context.Background(), required.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && !shouldDelete {
		requiredCopy := required.DeepCopy()
		actual, err := clientInterface.Create(context.Background(), resourcemerge.WithCleanLabelsAndAnnotations(requiredCopy).(*v1alpha1.StorageVersionMigration), metav1.CreateOptions{})
		reportCreateEvent(recorder, requiredCopy, err)
		return actual, true, err
	} else if apierrors.IsNotFound(err) && shouldDelete {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	if shouldDelete {
		err := clientInterface.Delete(context.TODO(), existing.Name, metav1.DeleteOptions{})
		if err != nil {
			return nil, false, err
		}
		reportDeleteEvent(recorder, required, err)
		return nil, true, nil
	}

	modified := resourcemerge.BoolPtr(false)
	existingCopy := existing.DeepCopy()
	resourcemerge.EnsureObjectMeta(modified, &existingCopy.ObjectMeta, required.ObjectMeta)
	if !*modified && reflect.DeepEqual(existingCopy.Spec, required.Spec) {
		return existingCopy, false, nil
	}

	if klog.V(4).Enabled() {
		klog.Infof("StorageVersionMigration %q changes: %v", required.Name, JSONPatchNoError(existing, required))
	}

	required.Spec.Resource.DeepCopyInto(&existingCopy.Spec.Resource)
	actual, err := clientInterface.Update(context.Background(), existingCopy, metav1.UpdateOptions{})
	reportUpdateEvent(recorder, required, err)
	return actual, true, err
}
