package jobs

import (
	"context"
	"fmt"

	operatorsv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehelper"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	batchclientv1 "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/klog/v2"
)

// TODO move to github.com/openshift/library-go/pkg/operator/resource/resource[read,apply,merge]

const (
	// Label keys for TNF job metadata
	LabelJobType   = "tnf.etcd.openshift.io/job-type"
	LabelNodeIndex = "tnf.etcd.openshift.io/node-index"
	LabelAttempt   = "tnf.etcd.openshift.io/attempt"
)

var (
	batchScheme = runtime.NewScheme()
	batchCodecs = serializer.NewCodecFactory(batchScheme)
)

func init() {
	if err := batchv1.AddToScheme(batchScheme); err != nil {
		panic(err)
	}
}

func ReadJobV1OrDie(objBytes []byte) *batchv1.Job {
	requiredObj, err := runtime.Decode(batchCodecs.UniversalDecoder(batchv1.SchemeGroupVersion), objBytes)
	if err != nil {
		panic(err)
	}
	return requiredObj.(*batchv1.Job)
}

func ReadCronJobV1OrDie(objBytes []byte) *batchv1.CronJob {
	requiredObj, err := runtime.Decode(batchCodecs.UniversalDecoder(batchv1.SchemeGroupVersion), objBytes)
	if err != nil {
		panic(err)
	}
	return requiredObj.(*batchv1.CronJob)
}

// ApplyJob ensures the form of the specified job is present in the API. If it
// does not exist, it will be created. If it does exist and has drifted from the required
// spec, the existing job will be deleted and recreated. For all non-failed cluster jobs
// (labeled with job-type=cluster), drift in retry-related fields (NodeName, node-index,
// attempt labels) is ignored to prevent recreation on operator restart when retry state
// is reinitialized.
func ApplyJob(ctx context.Context, client batchclientv1.JobsGetter, recorder events.Recorder,
	requiredOriginal *batchv1.Job, expectedGeneration int64) (*batchv1.Job, bool, error) {

	required := requiredOriginal.DeepCopy()
	err := resourceapply.SetSpecHashAnnotation(&required.ObjectMeta, required.Spec)
	if err != nil {
		return nil, false, err
	}

	existing, err := client.Jobs(required.Namespace).Get(ctx, required.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		actual, err := client.Jobs(required.Namespace).Create(ctx, required, metav1.CreateOptions{})
		resourcehelper.ReportCreateEvent(recorder, required, err)
		return actual, true, nil
	}
	if err != nil {
		return nil, false, err
	}

	existingCopy := existing.DeepCopy()
	modified := false
	resourcemerge.EnsureObjectMeta(&modified, &existingCopy.ObjectMeta, required.ObjectMeta)

	// there was no change to metadata, and the generation was right
	if !modified && existingCopy.ObjectMeta.Generation == expectedGeneration {
		return existingCopy, false, nil
	}

	// Drift detected - for non-failed cluster jobs, check if drift is only in retry-related fields
	isClusterJob := required.Labels != nil && required.Labels[LabelJobType] == "cluster"
	if isClusterJob && !IsFailed(*existing) {
		// Check if drift is only in retry-related fields (NodeName, node-index, attempt labels)
		// If so, this is expected (operator restart, retry state reinitialized) and safe to ignore
		// This applies to both completed and running jobs - we only want to recreate failed jobs for round-robin retry
		if isDriftOnlyInRetryFields(existing, required) {
			klog.V(4).Infof("Job %s/%s is not failed with drift only in retry fields - ignoring", existing.Namespace, existing.Name)
			return existing, false, nil
		}
		// Real drift detected in non-failed job (image, command, config changed)
		klog.Warningf("Job %s/%s is not failed but has real config drift - recreating", existing.Namespace, existing.Name)
	}

	// We do not update jobs, we always recreate them, since significant parts are immutable.
	// Delete here, recreate on next sync.
	err = client.Jobs(required.Namespace).Delete(ctx, required.Name, metav1.DeleteOptions{})
	if err != nil {
		return nil, false, err
	}
	resourcehelper.ReportDeleteEvent(recorder, required, nil)
	return nil, false, fmt.Errorf("job spec was modified, old job is deleted")
}

func ExpectedJobGeneration(required *batchv1.Job, previousGenerations []operatorsv1.GenerationStatus) int64 {
	generation := resourcemerge.GenerationFor(previousGenerations, schema.GroupResource{Group: "batch", Resource: "jobs"}, required.Namespace, required.Name)
	if generation != nil {
		return generation.LastGeneration
	}
	return -1
}

// isDriftOnlyInRetryFields checks if the drift between existing and required jobs
// is only in retry-related fields (NodeName, node-index label, attempt label).
// Returns true if drift is only in these fields, false if there's drift in other fields.
func isDriftOnlyInRetryFields(existing, required *batchv1.Job) bool {
	// Create copies with retry fields stripped
	existingStripped := existing.DeepCopy()
	requiredStripped := required.DeepCopy()

	// Strip NodeName from spec
	existingStripped.Spec.Template.Spec.NodeName = ""
	requiredStripped.Spec.Template.Spec.NodeName = ""

	// Strip retry labels
	delete(existingStripped.Labels, LabelNodeIndex)
	delete(existingStripped.Labels, LabelAttempt)
	delete(requiredStripped.Labels, LabelNodeIndex)
	delete(requiredStripped.Labels, LabelAttempt)

	// Recompute spec hashes without retry fields
	existingErr := resourceapply.SetSpecHashAnnotation(&existingStripped.ObjectMeta, existingStripped.Spec)
	requiredErr := resourceapply.SetSpecHashAnnotation(&requiredStripped.ObjectMeta, requiredStripped.Spec)
	if existingErr != nil || requiredErr != nil {
		// If we can't compute hashes, assume there's real drift to be safe
		return false
	}

	// Compare metadata after stripping
	modified := false
	resourcemerge.EnsureObjectMeta(&modified, &existingStripped.ObjectMeta, requiredStripped.ObjectMeta)

	// If no drift after stripping, then original drift was only in retry fields
	return !modified
}
