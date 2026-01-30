package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-cmp/cmp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	// Default resync interval for the controller
	defaultResyncInterval = 1 * time.Minute
)

// CronJobHookFunc is a hook function to modify the CronJob.
// Similar to JobHookFunc, this allows flexible modification of the CronJob object.
type CronJobHookFunc func(*operatorv1.OperatorSpec, *batchv1.CronJob) error

// CronJobController manages a Kubernetes CronJob resource
type CronJobController struct {
	operatorClient v1helpers.OperatorClient
	kubeClient     kubernetes.Interface
	eventRecorder  events.Recorder

	// Configuration for the CronJob
	manifest             []byte
	name                 string
	optionalCronJobHooks []CronJobHookFunc
}

// NewCronJobController creates a new controller for managing a CronJob.
// The name parameter is used for the controller instance name.
// Hook functions are called during sync to modify the CronJob before creation/update.
func NewCronJobController(
	name string,
	manifest []byte,
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	optionalCronJobHooks ...CronJobHookFunc,
) factory.Controller {
	c := &CronJobController{
		operatorClient:       operatorClient,
		kubeClient:           kubeClient,
		eventRecorder:        eventRecorder,
		manifest:             manifest,
		name:                 name,
		optionalCronJobHooks: optionalCronJobHooks,
	}

	controllerName := fmt.Sprintf("%sCronJobController", name)
	syncCtx := factory.NewSyncContext(controllerName, eventRecorder.WithComponentSuffix(fmt.Sprintf("%s-cronjob", name)))

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(defaultResyncInterval).
		WithSync(c.sync).
		WithInformers(
			operatorClient.Informer(),
		).ToController(controllerName, syncCtx.Recorder())
}

func (c *CronJobController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("%s sync started", c.name)
	defer klog.V(4).Infof("%s sync completed", c.name)

	// Check operator state
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	if opSpec.ManagementState != operatorv1.Managed {
		klog.V(4).Infof("Operator is not in Managed state, skipping CronJob creation")
		return nil
	}

	// Ensure the CronJob exists with the correct configuration
	if err := c.ensureCronJob(ctx); err != nil {
		return fmt.Errorf("failed to ensure CronJob: %w", err)
	}

	return nil
}

func (c *CronJobController) ensureCronJob(ctx context.Context) error {
	// Build the desired CronJob
	desired, err := c.buildCronJob(ctx)
	if err != nil {
		return fmt.Errorf("failed to build CronJob: %w", err)
	}

	// Get existing CronJob if it exists
	existing, err := c.kubeClient.BatchV1().CronJobs(operatorclient.TargetNamespace).Get(ctx, c.name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get CronJob: %w", err)
		}
		// CronJob doesn't exist, create it
		return c.createCronJob(ctx, desired)
	}

	// CronJob exists, check if it needs updating
	if c.needsUpdate(existing, desired) {
		klog.V(2).Infof("CronJob %s needs updating", c.name)
		return c.updateCronJob(ctx, existing, desired)
	}

	klog.V(4).Infof("CronJob %s is up to date", c.name)
	return nil
}

func (c *CronJobController) createCronJob(ctx context.Context, cronJob *batchv1.CronJob) error {
	_, err := c.kubeClient.BatchV1().CronJobs(operatorclient.TargetNamespace).Create(ctx, cronJob, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(4).Infof("CronJob %s already exists", c.name)
			return nil
		}
		return fmt.Errorf("failed to create CronJob: %w", err)
	}

	klog.V(2).Infof("Created CronJob: %s/%s with schedule %s", operatorclient.TargetNamespace, c.name, cronJob.Spec.Schedule)
	c.eventRecorder.Eventf("CronJobCreated", "Created CronJob: %s/%s", operatorclient.TargetNamespace, c.name)

	return nil
}

func (c *CronJobController) updateCronJob(ctx context.Context, existing, desired *batchv1.CronJob) error {
	// Preserve the resource version
	desired.ResourceVersion = existing.ResourceVersion

	_, err := c.kubeClient.BatchV1().CronJobs(operatorclient.TargetNamespace).Update(ctx, desired, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update CronJob: %w", err)
	}

	klog.V(2).Infof("Updated CronJob: %s/%s", operatorclient.TargetNamespace, c.name)
	c.eventRecorder.Eventf("CronJobUpdated", "Updated CronJob: %s/%s", operatorclient.TargetNamespace, c.name)

	return nil
}

func (c *CronJobController) buildCronJob(ctx context.Context) (*batchv1.CronJob, error) {
	// Parse the manifest
	cronJob := ReadCronJobV1OrDie(c.manifest)

	// Get operator spec for hooks - required to determine management state and pass to hooks
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return nil, fmt.Errorf("failed to get operator state for CronJob %s: %w", c.name, err)
	}

	// Enforce controller-managed identity and scope
	cronJob.Namespace = operatorclient.TargetNamespace
	cronJob.Name = c.name

	// Apply all hook functions to customize the CronJob (e.g., inject images, set schedules)
	for i, hook := range c.optionalCronJobHooks {
		if err := hook(opSpec, cronJob); err != nil {
			return nil, fmt.Errorf("CronJob %s hook function %d failed: %w", c.name, i, err)
		}
	}

	return cronJob, nil
}

func (c *CronJobController) needsUpdate(existing, desired *batchv1.CronJob) bool {
	// Normalize both specs to apply server defaults before comparison.
	// This prevents false positives from nil vs default value mismatches.
	normalizedExisting := existing.DeepCopy()
	normalizedDesired := desired.DeepCopy()
	normalizeCronJobSpec(normalizedExisting)
	normalizeCronJobSpec(normalizedDesired)

	// Compare normalized specs to detect actual configuration drift
	if !equality.Semantic.DeepEqual(normalizedExisting.Spec, normalizedDesired.Spec) {
		// Log drift using NORMALIZED specs so the output reflects actual differences
		c.logSpecDrift(normalizedExisting, normalizedDesired)
		return true
	}
	return false
}

// normalizeCronJobSpec applies Kubernetes API server default values to a CronJob spec.
// This ensures consistent comparison between desired specs (which may have nil optional fields)
// and existing specs (which have server-applied defaults).
func normalizeCronJobSpec(cronJob *batchv1.CronJob) {
	spec := &cronJob.Spec

	// CronJobSpec defaults
	if spec.Suspend == nil {
		spec.Suspend = ptrBool(false)
	}
	if spec.ConcurrencyPolicy == "" {
		spec.ConcurrencyPolicy = batchv1.AllowConcurrent
	}
	if spec.SuccessfulJobsHistoryLimit == nil {
		spec.SuccessfulJobsHistoryLimit = ptrInt32(3)
	}
	if spec.FailedJobsHistoryLimit == nil {
		spec.FailedJobsHistoryLimit = ptrInt32(1)
	}

	// JobSpec defaults
	jobSpec := &spec.JobTemplate.Spec
	if jobSpec.BackoffLimit == nil {
		jobSpec.BackoffLimit = ptrInt32(6)
	}
	if jobSpec.Completions == nil {
		jobSpec.Completions = ptrInt32(1)
	}
	if jobSpec.Parallelism == nil {
		jobSpec.Parallelism = ptrInt32(1)
	}
	if jobSpec.Suspend == nil {
		jobSpec.Suspend = ptrBool(false)
	}
	if jobSpec.CompletionMode == nil {
		mode := batchv1.NonIndexedCompletion
		jobSpec.CompletionMode = &mode
	}
	if jobSpec.PodFailurePolicy == nil {
		// PodReplacementPolicy default depends on PodFailurePolicy
		if jobSpec.PodReplacementPolicy == nil {
			policy := batchv1.TerminatingOrFailed
			jobSpec.PodReplacementPolicy = &policy
		}
	}

	// PodSpec defaults
	podSpec := &jobSpec.Template.Spec
	if podSpec.RestartPolicy == "" {
		podSpec.RestartPolicy = "Never"
	}
	if podSpec.TerminationGracePeriodSeconds == nil {
		podSpec.TerminationGracePeriodSeconds = ptrInt64(30)
	}
	if podSpec.DNSPolicy == "" {
		podSpec.DNSPolicy = "ClusterFirst"
	}
	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if podSpec.SchedulerName == "" {
		podSpec.SchedulerName = "default-scheduler"
	}
	// The deprecated ServiceAccount field is auto-populated by the API server
	// to match ServiceAccountName. Normalize it to prevent false drift detection.
	if podSpec.DeprecatedServiceAccount == "" && podSpec.ServiceAccountName != "" {
		podSpec.DeprecatedServiceAccount = podSpec.ServiceAccountName
	}

	// Container defaults
	for i := range podSpec.Containers {
		container := &podSpec.Containers[i]
		if container.TerminationMessagePath == "" {
			container.TerminationMessagePath = "/dev/termination-log"
		}
		if container.TerminationMessagePolicy == "" {
			container.TerminationMessagePolicy = "File"
		}
		if container.ImagePullPolicy == "" {
			// Match Kubernetes' tag-aware defaults:
			// - "Always" for images with :latest tag or no tag
			// - "IfNotPresent" for images with any other tag
			container.ImagePullPolicy = inferImagePullPolicy(container.Image)
		}
	}
}

// inferImagePullPolicy returns the Kubernetes default ImagePullPolicy based on the image tag.
// Per Kubernetes documentation:
// - If tag is :latest or omitted: Always
// - If tag is anything else: IfNotPresent
func inferImagePullPolicy(image string) corev1.PullPolicy {
	// Handle empty image (shouldn't happen, but be defensive)
	if image == "" {
		return corev1.PullAlways
	}

	// Parse the image reference to extract tag/digest information
	ref, err := reference.Parse(image)
	if err != nil {
		// If parsing fails, default to Always (safest option)
		klog.V(4).Infof("failed to parse image reference %q: %v, defaulting to Always", image, err)
		return corev1.PullAlways
	}

	// If there's a digest (ID field in DockerImageReference), use IfNotPresent
	// Digests are immutable, so there's no need to always pull
	if ref.ID != "" {
		return corev1.PullIfNotPresent
	}

	// If no tag or :latest tag, default is Always
	if ref.Tag == "" || ref.Tag == "latest" {
		return corev1.PullAlways
	}

	// Any other tag - default is IfNotPresent
	return corev1.PullIfNotPresent
}

// logSpecDrift logs the differences between existing and desired CronJob specs.
// NOTE: This function expects NORMALIZED specs (after normalizeCronJobSpec has been applied).
func (c *CronJobController) logSpecDrift(existing, desired *batchv1.CronJob) {
	diff := cmp.Diff(existing.Spec, desired.Spec)
	if diff != "" {
		klog.V(2).Infof("CronJob %s drift detected:\n%s", c.name, diff)
	}
}

func ptrBool(b bool) *bool {
	return &b
}

func ptrInt32(i int32) *int32 {
	return &i
}

func ptrInt64(i int64) *int64 {
	return &i
}
