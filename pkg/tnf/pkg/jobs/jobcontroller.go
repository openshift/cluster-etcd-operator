package jobs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	opv1 "github.com/openshift/api/operator/v1"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	batchinformersv1 "k8s.io/client-go/informers/batch/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// By default, we only set the progressing and degraded conditions for TNF setup.
// This is because the operator is only unavailable during the transition of etcd
// ownership between CEO and pacemaker.
var DefaultConditions = []string{opv1.OperatorStatusTypeProgressing, opv1.OperatorStatusTypeDegraded}
var AllConditions = []string{opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeProgressing, opv1.OperatorStatusTypeDegraded}

// TODO This based on DeploymentController in openshift/library-go
// TODO should be moved there once it proved to be useful

// JobHookFunc is a hook function to modify the Job.
type JobHookFunc func(*opv1.OperatorSpec, *batchv1.Job) error

// JobController is a generic controller that manages a job.
//
// This controller optionally produces the following conditions:
// <name>Available: indicates that the Job was successfully deployed and completed.
// <name>Progressing: indicates that the Job is active.
// <name>Degraded: produced when the Job failed.
type JobController struct {
	// instanceName is the name to identify what instance this belongs too: FooDriver for instance
	instanceName string
	// controllerInstanceName is the name to identify this instance of this particular control loop: FooDriver-CSIDriverNodeService for instance.
	controllerInstanceName string

	manifest          []byte
	operatorClient    v1helpers.OperatorClient
	kubeClient        kubernetes.Interface
	jobInformer       batchinformersv1.JobInformer
	optionalInformers []factory.Informer
	recorder          events.Recorder
	conditions        []string
	// Optional hook functions to modify the Job.
	// If one of these functions returns an error, the sync
	// fails indicating the ordinal position of the failed function.
	// Also, in that scenario the Degraded status is set to True.
	optionalJobHooks []JobHookFunc
	// errors contains any errors that occur during the configuration
	// and setup of the JobController.
	errors []error
}

// NewJobController creates a new instance of JobController,
// returning it as a factory.Controller interface. Under the hood it uses
// the NewJobControllerBuilder to construct the controller.
func NewJobController(
	name string,
	manifest []byte,
	recorder events.Recorder,
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	jobInformer batchinformersv1.JobInformer,
	conditions []string,
	optionalInformers []factory.Informer,
	optionalJobHooks ...JobHookFunc,
) factory.Controller {
	c := NewJobControllerBuilder(
		name,
		manifest,
		recorder,
		operatorClient,
		kubeClient,
		jobInformer,
	).WithConditions(
		conditions...,
	).WithExtraInformers(
		optionalInformers...,
	).WithJobHooks(
		optionalJobHooks...,
	)

	controller, err := c.ToController()
	if err != nil {
		panic(err)
	}
	return controller
}

// NewJobControllerBuilder initializes and returns a pointer to a
// minimal JobController.
func NewJobControllerBuilder(
	instanceName string,
	manifest []byte,
	recorder events.Recorder,
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	jobInformer batchinformersv1.JobInformer,
) *JobController {
	return &JobController{
		instanceName:           instanceName,
		controllerInstanceName: factory.ControllerInstanceName(instanceName, "Job"),
		manifest:               manifest,
		operatorClient:         operatorClient,
		kubeClient:             kubeClient,
		jobInformer:            jobInformer,
		recorder:               recorder,
	}
}

// WithExtraInformers appends additional informers to the JobController.
// These informers are used to watch for additional resources that might affect the Jobs's state.
func (c *JobController) WithExtraInformers(informers ...factory.Informer) *JobController {
	c.optionalInformers = informers
	return c
}

// WithJobHooks adds custom hook functions that are called during the sync.
// These hooks can perform operations or modifications at specific points in the Job.
func (c *JobController) WithJobHooks(hooks ...JobHookFunc) *JobController {
	c.optionalJobHooks = hooks
	return c
}

// WithConditions sets the operational conditions under which the JobController will operate.
// Only 'Available', 'Progressing' and 'Degraded' are valid conditions; other values are ignored.
func (c *JobController) WithConditions(conditions ...string) *JobController {
	validConditions := sets.New[string]()
	validConditions.Insert(
		opv1.OperatorStatusTypeAvailable,
		opv1.OperatorStatusTypeProgressing,
		opv1.OperatorStatusTypeDegraded,
	)
	for _, condition := range conditions {
		if validConditions.Has(condition) {
			if !slices.Contains(c.conditions, condition) {
				c.conditions = append(c.conditions, condition)
			}
		} else {
			err := fmt.Errorf("invalid condition %q. Valid conditions include %v", condition, validConditions.UnsortedList())
			c.errors = append(c.errors, err)
		}
	}
	return c
}

// ToController converts the JobController into a factory.Controller.
// It aggregates and returns all errors reported during the builder phase.
func (c *JobController) ToController() (factory.Controller, error) {
	informers := append(
		c.optionalInformers,
		c.operatorClient.Informer(),
		c.jobInformer.Informer(),
	)
	controller := factory.New().WithControllerInstanceName(c.controllerInstanceName).WithInformers(
		informers...,
	).WithSync(
		c.sync,
	).ResyncEvery(
		time.Minute,
	)
	if slices.Contains(c.conditions, opv1.OperatorStatusTypeDegraded) {
		controller = controller.WithSyncDegradedOnError(c.operatorClient)
	}
	return controller.ToController(
		c.instanceName, // don't change what is passed here unless you also remove the old FooDegraded condition
		c.recorder.WithComponentSuffix(strings.ToLower(c.instanceName)+"-job-controller-"),
	), errors.NewAggregate(c.errors)
}

// Name returns the name of the JobController.
func (c *JobController) Name() string {
	return c.instanceName
}

func (c *JobController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	opSpec, opStatus, _, err := c.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) && management.IsOperatorRemovable() {
		return nil
	}
	if err != nil {
		return err
	}

	if opSpec.ManagementState == opv1.Removed && management.IsOperatorRemovable() {
		return c.syncDeleting(ctx, opSpec, opStatus, syncContext)
	}

	if opSpec.ManagementState != opv1.Managed {
		return nil
	}

	meta, err := c.operatorClient.GetObjectMeta()
	if err != nil {
		return err
	}
	if management.IsOperatorRemovable() && meta.DeletionTimestamp != nil {
		return c.syncDeleting(ctx, opSpec, opStatus, syncContext)
	}
	return c.syncManaged(ctx, opSpec, opStatus, syncContext)
}

func (c *JobController) syncManaged(ctx context.Context, opSpec *opv1.OperatorSpec, opStatus *opv1.OperatorStatus, syncContext factory.SyncContext) error {
	klog.V(4).Infof("syncManaged")

	required, err := c.getJob(opSpec)
	if err != nil {
		return err
	}

	job, _, err := ApplyJob(
		ctx,
		c.kubeClient.BatchV1(),
		syncContext.Recorder(),
		required,
		ExpectedJobGeneration(required, opStatus.Generations),
	)
	if err != nil {
		return err
	}

	// Create an OperatorStatusApplyConfiguration with generations
	status := applyoperatorv1.OperatorStatus().
		WithGenerations(&applyoperatorv1.GenerationStatusApplyConfiguration{
			Group:          ptr.To("batch"),
			Resource:       ptr.To("jobs"),
			Namespace:      ptr.To(job.Namespace),
			Name:           ptr.To(job.Name),
			LastGeneration: ptr.To(job.Generation),
		})

	// Set Available condition
	if slices.Contains(c.conditions, opv1.OperatorStatusTypeAvailable) {
		availableCondition := applyoperatorv1.
			OperatorCondition().WithType(c.instanceName + opv1.OperatorStatusTypeAvailable)

		if isComplete(job) {
			availableCondition = availableCondition.
				WithStatus(opv1.ConditionTrue).
				WithReason("JobComplete").
				WithMessage("Job completed")
		} else if isFailed(job) {
			availableCondition = availableCondition.
				WithStatus(opv1.ConditionFalse).
				WithReason("JobFailed").
				WithMessage("Job failed")
		} else {
			availableCondition = availableCondition.
				WithStatus(opv1.ConditionFalse).
				WithReason("JobRunning").
				WithMessage("Job is running")
		}

		status = status.WithConditions(availableCondition)
	}

	// Set Progressing condition
	if slices.Contains(c.conditions, opv1.OperatorStatusTypeProgressing) {
		progressingCondition := applyoperatorv1.OperatorCondition().
			WithType(c.instanceName + opv1.OperatorStatusTypeProgressing)

		if isComplete(job) {
			progressingCondition = progressingCondition.
				WithStatus(opv1.ConditionFalse).
				WithReason("JobComplete").
				WithMessage("Job completed")
		} else if isFailed(job) {
			progressingCondition = progressingCondition.
				WithStatus(opv1.ConditionFalse).
				WithReason("JobFailed").
				WithMessage("Job failed")
		} else {
			progressingCondition = progressingCondition.
				WithStatus(opv1.ConditionTrue).
				WithReason("JobRunning").
				WithMessage("Job is running")
		}

		status = status.WithConditions(progressingCondition)
	}

	err = c.operatorClient.ApplyOperatorStatus(
		ctx,
		c.controllerInstanceName,
		status,
	)
	if err != nil {
		return err
	}

	// return an error for reporting degraded status!
	// setting a condition manually, similar to available and progressing, doesn't work
	if isFailed(job) {
		return fmt.Errorf("Job failed")
	}
	return nil
}

func (c *JobController) syncDeleting(ctx context.Context, opSpec *opv1.OperatorSpec, opStatus *opv1.OperatorStatus, syncContext factory.SyncContext) error {
	klog.V(4).Infof("syncDeleting")
	required, err := c.getJob(opSpec)
	if err != nil {
		return err
	}

	err = c.kubeClient.BatchV1().Jobs(required.Namespace).Delete(ctx, required.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	} else {
		klog.V(2).Infof("Deleted Job %s/%s", required.Namespace, required.Name)
	}
	return nil
}

func (c *JobController) getJob(opSpec *opv1.OperatorSpec) (*batchv1.Job, error) {
	manifest := c.manifest
	required := ReadJobV1OrDie(manifest)
	for i := range c.optionalJobHooks {
		err := c.optionalJobHooks[i](opSpec, required)
		if err != nil {
			return nil, fmt.Errorf("error running hook function (index=%d): %w", i, err)
		}
	}
	return required, nil
}

func isComplete(job *batchv1.Job) bool {
	return tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete)
}

func isFailed(job *batchv1.Job) bool {
	return tools.IsConditionTrue(job.Status.Conditions, batchv1.JobFailed)
}
