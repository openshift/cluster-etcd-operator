package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
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

	// Get operator spec for hooks
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return nil, fmt.Errorf("failed to get operator state: %w", err)
	}

	// Enforce controller-managed identity and scope
	cronJob.Namespace = operatorclient.TargetNamespace
	cronJob.Name = c.name

	// Apply all hook functions
	for i, hook := range c.optionalCronJobHooks {
		if err := hook(opSpec, cronJob); err != nil {
			return nil, fmt.Errorf("hook function %d failed: %w", i, err)
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

	// Find the last colon that's not part of a port (after last /)
	lastSlash := strings.LastIndex(image, "/")
	tagPart := image
	if lastSlash != -1 {
		tagPart = image[lastSlash:]
	}

	// Check if there's a tag (colon after the last slash)
	colonIdx := strings.LastIndex(tagPart, ":")
	if colonIdx == -1 {
		// No tag specified - default is Always
		return corev1.PullAlways
	}

	// Extract the tag
	tag := tagPart[colonIdx+1:]

	// Check for digest (sha256:...) - these should use IfNotPresent
	if strings.HasPrefix(tag, "sha256:") || strings.Contains(image, "@sha256:") {
		return corev1.PullIfNotPresent
	}

	// :latest tag - default is Always
	if tag == "latest" {
		return corev1.PullAlways
	}

	// Any other tag - default is IfNotPresent
	return corev1.PullIfNotPresent
}

// logSpecDrift logs specific fields that have drifted between existing and desired CronJob specs.
// NOTE: This function expects NORMALIZED specs (after normalizeCronJobSpec has been applied).
func (c *CronJobController) logSpecDrift(existing, desired *batchv1.CronJob) {
	var drifts []string

	// CronJobSpec level
	if existing.Spec.Schedule != desired.Spec.Schedule {
		drifts = append(drifts, fmt.Sprintf("schedule: %q -> %q", existing.Spec.Schedule, desired.Spec.Schedule))
	}
	if !equality.Semantic.DeepEqual(existing.Spec.Suspend, desired.Spec.Suspend) {
		drifts = append(drifts, fmt.Sprintf("suspend: %v -> %v", ptrBoolValue(existing.Spec.Suspend), ptrBoolValue(desired.Spec.Suspend)))
	}
	if existing.Spec.ConcurrencyPolicy != desired.Spec.ConcurrencyPolicy {
		drifts = append(drifts, fmt.Sprintf("concurrencyPolicy: %q -> %q", existing.Spec.ConcurrencyPolicy, desired.Spec.ConcurrencyPolicy))
	}
	if !equality.Semantic.DeepEqual(existing.Spec.SuccessfulJobsHistoryLimit, desired.Spec.SuccessfulJobsHistoryLimit) {
		drifts = append(drifts, fmt.Sprintf("successfulJobsHistoryLimit: %v -> %v", ptrInt32Value(existing.Spec.SuccessfulJobsHistoryLimit), ptrInt32Value(desired.Spec.SuccessfulJobsHistoryLimit)))
	}
	if !equality.Semantic.DeepEqual(existing.Spec.FailedJobsHistoryLimit, desired.Spec.FailedJobsHistoryLimit) {
		drifts = append(drifts, fmt.Sprintf("failedJobsHistoryLimit: %v -> %v", ptrInt32Value(existing.Spec.FailedJobsHistoryLimit), ptrInt32Value(desired.Spec.FailedJobsHistoryLimit)))
	}

	// JobTemplate.ObjectMeta level - check if labels or annotations differ
	if !equality.Semantic.DeepEqual(existing.Spec.JobTemplate.Labels, desired.Spec.JobTemplate.Labels) {
		drifts = append(drifts, fmt.Sprintf("jobTemplate.labels: %v -> %v", existing.Spec.JobTemplate.Labels, desired.Spec.JobTemplate.Labels))
	}
	if !equality.Semantic.DeepEqual(existing.Spec.JobTemplate.Annotations, desired.Spec.JobTemplate.Annotations) {
		drifts = append(drifts, fmt.Sprintf("jobTemplate.annotations: %v -> %v", existing.Spec.JobTemplate.Annotations, desired.Spec.JobTemplate.Annotations))
	}

	// JobSpec level
	existingJobSpec := &existing.Spec.JobTemplate.Spec
	desiredJobSpec := &desired.Spec.JobTemplate.Spec
	if !equality.Semantic.DeepEqual(existingJobSpec.BackoffLimit, desiredJobSpec.BackoffLimit) {
		drifts = append(drifts, fmt.Sprintf("jobSpec.backoffLimit: %v -> %v", ptrInt32Value(existingJobSpec.BackoffLimit), ptrInt32Value(desiredJobSpec.BackoffLimit)))
	}
	if !equality.Semantic.DeepEqual(existingJobSpec.TTLSecondsAfterFinished, desiredJobSpec.TTLSecondsAfterFinished) {
		drifts = append(drifts, fmt.Sprintf("jobSpec.ttlSecondsAfterFinished: %v -> %v", ptrInt32Value(existingJobSpec.TTLSecondsAfterFinished), ptrInt32Value(desiredJobSpec.TTLSecondsAfterFinished)))
	}

	// PodSpec level
	existingPodSpec := &existingJobSpec.Template.Spec
	desiredPodSpec := &desiredJobSpec.Template.Spec
	if existingPodSpec.ServiceAccountName != desiredPodSpec.ServiceAccountName {
		drifts = append(drifts, fmt.Sprintf("podSpec.serviceAccountName: %q -> %q", existingPodSpec.ServiceAccountName, desiredPodSpec.ServiceAccountName))
	}
	if existingPodSpec.RestartPolicy != desiredPodSpec.RestartPolicy {
		drifts = append(drifts, fmt.Sprintf("podSpec.restartPolicy: %q -> %q", existingPodSpec.RestartPolicy, desiredPodSpec.RestartPolicy))
	}
	if !equality.Semantic.DeepEqual(existingPodSpec.NodeSelector, desiredPodSpec.NodeSelector) {
		drifts = append(drifts, fmt.Sprintf("podSpec.nodeSelector: %v -> %v", existingPodSpec.NodeSelector, desiredPodSpec.NodeSelector))
	}
	if !equality.Semantic.DeepEqual(existingPodSpec.Tolerations, desiredPodSpec.Tolerations) {
		drifts = append(drifts, fmt.Sprintf("podSpec.tolerations: %d items -> %d items", len(existingPodSpec.Tolerations), len(desiredPodSpec.Tolerations)))
	}

	// Container level
	existingContainers := existingPodSpec.Containers
	desiredContainers := desiredPodSpec.Containers

	if len(existingContainers) != len(desiredContainers) {
		drifts = append(drifts, fmt.Sprintf("container count: %d -> %d", len(existingContainers), len(desiredContainers)))
	} else {
		for i := range existingContainers {
			if existingContainers[i].Image != desiredContainers[i].Image {
				drifts = append(drifts, fmt.Sprintf("container[%d].image: %q -> %q", i, existingContainers[i].Image, desiredContainers[i].Image))
			}
			if !equality.Semantic.DeepEqual(existingContainers[i].Command, desiredContainers[i].Command) {
				drifts = append(drifts, fmt.Sprintf("container[%d].command: %v -> %v", i, existingContainers[i].Command, desiredContainers[i].Command))
			}
			if !equality.Semantic.DeepEqual(existingContainers[i].Args, desiredContainers[i].Args) {
				drifts = append(drifts, fmt.Sprintf("container[%d].args: %v -> %v", i, existingContainers[i].Args, desiredContainers[i].Args))
			}
			if existingContainers[i].ImagePullPolicy != desiredContainers[i].ImagePullPolicy {
				drifts = append(drifts, fmt.Sprintf("container[%d].imagePullPolicy: %q -> %q", i, existingContainers[i].ImagePullPolicy, desiredContainers[i].ImagePullPolicy))
			}
			if !equality.Semantic.DeepEqual(existingContainers[i].Env, desiredContainers[i].Env) {
				drifts = append(drifts, fmt.Sprintf("container[%d].env: %d vars -> %d vars", i, len(existingContainers[i].Env), len(desiredContainers[i].Env)))
			}
			if !equality.Semantic.DeepEqual(existingContainers[i].VolumeMounts, desiredContainers[i].VolumeMounts) {
				drifts = append(drifts, fmt.Sprintf("container[%d].volumeMounts: %d -> %d", i, len(existingContainers[i].VolumeMounts), len(desiredContainers[i].VolumeMounts)))
			}
			if !equality.Semantic.DeepEqual(existingContainers[i].SecurityContext, desiredContainers[i].SecurityContext) {
				drifts = append(drifts, fmt.Sprintf("container[%d].securityContext: differs", i))
			}
		}
	}

	if len(drifts) > 0 {
		klog.V(2).Infof("CronJob %s drift detected: %v", c.name, drifts)
	} else {
		// Drift in fields we don't explicitly check - use reflection to find the exact difference
		klog.Warningf("CronJob %s drift detected in untracked fields", c.name)
		c.logUnknownDrift(existing, desired)
	}
}

// logUnknownDrift uses reflection to identify exactly which fields differ between two CronJobs.
// This is called when equality.Semantic.DeepEqual returns false but our explicit checks don't find differences.
func (c *CronJobController) logUnknownDrift(existing, desired *batchv1.CronJob) {
	// Compare CronJobSpec fields using reflection
	existingSpec := reflect.ValueOf(existing.Spec)
	desiredSpec := reflect.ValueOf(desired.Spec)
	specType := existingSpec.Type()

	var fieldDiffs []string
	for i := 0; i < specType.NumField(); i++ {
		fieldName := specType.Field(i).Name
		existingField := existingSpec.Field(i).Interface()
		desiredField := desiredSpec.Field(i).Interface()

		if !equality.Semantic.DeepEqual(existingField, desiredField) {
			fieldDiffs = append(fieldDiffs, fieldName)
			// Log the actual values for this field
			existingJSON, _ := json.Marshal(existingField)
			desiredJSON, _ := json.Marshal(desiredField)
			klog.V(2).Infof("CronJob %s field %s differs:\n  existing: %s\n  desired:  %s",
				c.name, fieldName, string(existingJSON), string(desiredJSON))
		}
	}

	if len(fieldDiffs) > 0 {
		klog.Infof("CronJob %s untracked drift in CronJobSpec fields: %v", c.name, fieldDiffs)
	} else {
		// The difference must be in metadata or status - check JobTemplate more deeply
		c.logJobTemplateDrift(existing, desired)
	}
}

// logJobTemplateDrift drills into JobTemplate to find differences
func (c *CronJobController) logJobTemplateDrift(existing, desired *batchv1.CronJob) {
	existingJT := existing.Spec.JobTemplate
	desiredJT := desired.Spec.JobTemplate

	// Check JobTemplate.ObjectMeta
	if !equality.Semantic.DeepEqual(existingJT.ObjectMeta, desiredJT.ObjectMeta) {
		existingJSON, _ := json.Marshal(existingJT.ObjectMeta)
		desiredJSON, _ := json.Marshal(desiredJT.ObjectMeta)
		klog.Infof("CronJob %s JobTemplate.ObjectMeta differs:\n  existing: %s\n  desired:  %s",
			c.name, string(existingJSON), string(desiredJSON))
		return
	}

	// Check JobSpec fields
	existingJobSpec := reflect.ValueOf(existingJT.Spec)
	desiredJobSpec := reflect.ValueOf(desiredJT.Spec)
	jobSpecType := existingJobSpec.Type()

	for i := 0; i < jobSpecType.NumField(); i++ {
		fieldName := jobSpecType.Field(i).Name
		existingField := existingJobSpec.Field(i).Interface()
		desiredField := desiredJobSpec.Field(i).Interface()

		if !equality.Semantic.DeepEqual(existingField, desiredField) {
			existingJSON, _ := json.Marshal(existingField)
			desiredJSON, _ := json.Marshal(desiredField)
			klog.Infof("CronJob %s JobSpec.%s differs:\n  existing: %s\n  desired:  %s",
				c.name, fieldName, string(existingJSON), string(desiredJSON))
		}
	}
}

func ptrBoolValue(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

func ptrInt32Value(i *int32) int32 {
	if i == nil {
		return 0
	}
	return *i
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
