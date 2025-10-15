package jobs

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	// Check if schedule has changed
	if existing.Spec.Schedule != desired.Spec.Schedule {
		klog.V(4).Infof("Schedule changed from %q to %q", existing.Spec.Schedule, desired.Spec.Schedule)
		return true
	}

	// Check if the job template has changed (containers, command, image, etc.)
	if len(existing.Spec.JobTemplate.Spec.Template.Spec.Containers) > 0 &&
		len(desired.Spec.JobTemplate.Spec.Template.Spec.Containers) > 0 {
		existingContainer := existing.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
		desiredContainer := desired.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

		// Check if command has changed
		if len(existingContainer.Command) != len(desiredContainer.Command) {
			klog.V(4).Infof("Command length changed")
			return true
		}
		for i := range desiredContainer.Command {
			if existingContainer.Command[i] != desiredContainer.Command[i] {
				klog.V(4).Infof("Command changed at index %d", i)
				return true
			}
		}

		// Check if image has changed
		if existingContainer.Image != desiredContainer.Image {
			klog.V(4).Infof("Image changed from %q to %q", existingContainer.Image, desiredContainer.Image)
			return true
		}
	}

	return false
}
