package jobs

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	configv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreinformers "k8s.io/client-go/informers"
	fakecore "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"
)

const (
	infraConfigName  = "cluster"
	jobName          = "testjob"
	defaultClusterID = "ID1234"
	controllerName   = "TestJobController"
	operandName      = "dummy-controller"
	operandNamespace = "openshift-test"
	// From github.com/openshift/library-go/pkg/operator/resource/resourceapply/batch.go
	specHashAnnotation = "operator.openshift.io/spec-hash"
)

var (
	conditionAvailable   = controllerName + opv1.OperatorStatusTypeAvailable
	conditionProgressing = controllerName + opv1.OperatorStatusTypeProgressing
	conditionDegraded    = controllerName + opv1.OperatorStatusTypeDegraded
	allConditions        = []string{conditionAvailable, conditionProgressing, conditionDegraded}
	availableCondition   = []string{conditionAvailable}
)

func TestSync(t *testing.T) {
	testCases := []testCase{
		{
			// No job should be created and conditions should remain unchanged
			name: "job not created without sync",
			initialObjects: testObjects{
				operator: makeFakeOperatorInstance(
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
			expectedObjects: testObjects{
				job: nil, // No job should be created
				operator: makeFakeOperatorInstance(
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
		},
		{
			// Only CR exists, everything else is created
			name: "initial sync will create job",
			initialObjects: testObjects{
				job: &batchv1.Job{},
				operator: makeFakeOperatorInstance(
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1)),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					// The progressing condition is set as soon as the job is created
					withTrueConditions(conditionAvailable, conditionProgressing),
					withFalseConditions(conditionDegraded),
				),
			},
		},
		{
			// Job is completed and its status is synced to CR
			name: "job completed",
			initialObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 1, 0),
					withJobComplete()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 1, 0),
					withJobComplete()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
		},
		{
			// Job is running
			name: "job running",
			initialObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(1, 0, 0)), // the Job has 1 active pod
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(1, 0, 0)), // no change to the Job
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable, conditionProgressing),
					withFalseConditions(conditionDegraded),
				),
			},
		},
		{
			// Job failed - this should trigger degraded status via error
			name: "job failed",
			initialObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 0, 1),
					withJobFailed()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
					withFalseConditions(conditionProgressing, conditionDegraded),
				),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 0, 1),
					withJobFailed()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable, conditionDegraded),
					withFalseConditions(conditionProgressing),
				),
			},
			expectErr: true, // Job failure should return an error
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			// Initialize
			management.SetOperatorNotRemovable()
			ctx := newTestContext(test, t, DefaultConditions)

			// Act
			var err error
			if test.initialObjects.job != nil {
				err = ctx.controller.Sync(context.TODO(), factory.NewSyncContext(controllerName, events.NewInMemoryRecorder("test-job-controller", clocktesting.NewFakePassiveClock(time.Now()))))
			}

			// If sync returned an error and we expect degraded condition, simulate WithSyncDegradedOnError
			if err != nil && test.expectErr {
				// Simulate the WithSyncDegradedOnError behavior
				degradedCondition := applyoperatorv1.OperatorCondition().
					WithType(conditionDegraded).
					WithStatus(opv1.ConditionTrue).
					WithReason("SyncError").
					WithMessage(err.Error())

				status := applyoperatorv1.OperatorStatus().WithConditions(degradedCondition)
				ctx.operatorClient.ApplyOperatorStatus(context.TODO(), controllerName+"-Job", status)
			}

			// Assert
			// Check error
			if err != nil && !test.expectErr {
				t.Errorf("sync() returned unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Error("sync() unexpectedly succeeded when error was expected")
			}

			// Check expectedObjects.job
			if test.expectedObjects.job != nil {
				actualJob, err := ctx.coreClient.BatchV1().Jobs(operandNamespace).Get(context.TODO(), jobName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get Job %s: %v", jobName, err)
				}
				sanitizeJob(actualJob)
				sanitizeJob(test.expectedObjects.job)
				if !equality.Semantic.DeepEqual(test.expectedObjects.job, actualJob) {
					t.Errorf("Unexpected Job %+v content:\n%s", operandName, cmp.Diff(test.expectedObjects.job, actualJob))
				}
			}
			if test.expectedObjects.job == nil && test.initialObjects.job != nil {
				actualJob, err := ctx.coreClient.BatchV1().Jobs(operandNamespace).Get(context.TODO(), jobName, metav1.GetOptions{})
				if err == nil {
					t.Errorf("Expected Job to be deleted, found generation %d", actualJob.Generation)
				}
				if !errors.IsNotFound(err) {
					t.Errorf("Expected error to be NotFound, got %s", err)
				}
			}

			// Check expectedObjects.operator.Status
			if test.expectedObjects.operator != nil {
				_, actualStatus, _, err := ctx.operatorClient.GetOperatorState()
				if err != nil {
					t.Errorf("Failed to get operator: %v", err)
				}
				sanitizeInstanceStatus(actualStatus, allConditions)
				sanitizeInstanceStatus(&test.expectedObjects.operator.Status, allConditions)
				if !equality.Semantic.DeepEqual(test.expectedObjects.operator.Status, *actualStatus) {
					t.Errorf("Unexpected operator %+v content:\n%s", operandName, cmp.Diff(test.expectedObjects.operator.Status, *actualStatus))
				}
			}

			// Check expected ObjectMeta - only check what's relevant for non-finalizer operations
			actualMeta, err := ctx.operatorClient.GetObjectMeta()
			if err != nil {
				t.Errorf("Failed to get operator: %v", err)
			}
			sanitizeObjectMeta(actualMeta)
			expectedMeta := &test.expectedObjects.operator.ObjectMeta
			sanitizeObjectMeta(expectedMeta)
			// For job controller, we only care about basic ObjectMeta fields, not finalizers
			if actualMeta.Name != expectedMeta.Name || actualMeta.Generation != expectedMeta.Generation {
				t.Errorf("Unexpected operator ObjectMeta basic fields: name=%s vs %s, generation=%d vs %d",
					actualMeta.Name, expectedMeta.Name, actualMeta.Generation, expectedMeta.Generation)
			}
		})
	}
}

// Test with Available condition explicitly enabled
func TestSyncWithAvailableCondition(t *testing.T) {
	testCases := []testCase{
		{
			name: "job completed with available condition",
			initialObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 1, 0),
					withJobComplete()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withFalseConditions(conditionAvailable)),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 1, 0),
					withJobComplete()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
				),
			},
		},
		{
			name: "job running with available condition",
			initialObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(1, 0, 0)),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
				),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(1, 0, 0)),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withFalseConditions(conditionAvailable),
				),
			},
		},
		{
			name: "job failed with available condition",
			initialObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 0, 1),
					withJobFailed()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withTrueConditions(conditionAvailable),
				),
			},
			expectedObjects: testObjects{
				job: makeJob(
					withJobGeneration(1),
					withJobStatus(0, 0, 1),
					withJobFailed()),
				operator: makeFakeOperatorInstance(
					withGenerations(1),
					withFalseConditions(conditionAvailable),
				),
			},
			expectErr: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			// Initialize
			management.SetOperatorNotRemovable()
			ctx := newTestContext(test, t, AllConditions)

			// Act
			err := ctx.controller.Sync(context.TODO(), factory.NewSyncContext(controllerName, events.NewInMemoryRecorder("test-job-controller", clocktesting.NewFakePassiveClock(time.Now()))))

			// Assert
			// Check error
			if err != nil && !test.expectErr {
				t.Errorf("sync() returned unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Error("sync() unexpectedly succeeded when error was expected")
			}

			// Check expectedObjects.operator.Status - verify all conditions
			if test.expectedObjects.operator != nil {
				_, actualStatus, _, err := ctx.operatorClient.GetOperatorState()
				if err != nil {
					t.Errorf("Failed to get operator: %v", err)
				}
				sanitizeInstanceStatus(actualStatus, availableCondition)
				sanitizeInstanceStatus(&test.expectedObjects.operator.Status, availableCondition)
				if !equality.Semantic.DeepEqual(test.expectedObjects.operator.Status, *actualStatus) {
					t.Errorf("Unexpected operator %+v content:\n%s", operandName, cmp.Diff(test.expectedObjects.operator.Status, *actualStatus))
				}
			}
		})
	}
}

func TestJobModificationRecreation(t *testing.T) {
	// Initialize
	management.SetOperatorNotRemovable()

	coreClient := fakecore.NewSimpleClientset()
	coreInformerFactory := coreinformers.NewSharedInformerFactory(coreClient, 0)

	// Create a modified job that differs from the expected manifest
	modifiedJob := makeJob(
		withJobGeneration(1),
		withModifiedJob(), // This makes it different from expected manifest
	)

	// Add the modified job to the fake client and informer
	coreClient.BatchV1().Jobs(operandNamespace).Create(context.TODO(), modifiedJob, metav1.CreateOptions{})
	coreInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(modifiedJob)

	// Add global reactors
	addGenerationReactor(coreClient)

	// Add infrastructure config
	initialInfras := []runtime.Object{makeInfra()}
	configClient := fakeconfig.NewSimpleClientset(initialInfras...)
	configInformerFactory := configinformers.NewSharedInformerFactory(configClient, 0)
	configInformer := configInformerFactory.Config().V1().Infrastructures().Informer()
	configInformer.GetIndexer().Add(initialInfras[0])

	// Create fake operator instance
	operatorInstance := makeFakeOperatorInstance(withGenerations(1))
	fakeOperatorClient := v1helpers.NewFakeOperatorClientWithObjectMeta(
		&operatorInstance.ObjectMeta,
		&operatorInstance.Spec,
		&operatorInstance.Status,
		nil,
	)

	// Create controller
	optionalInformers := []factory.Informer{configInformer}
	var optionalJobHooks []JobHookFunc
	controller := NewJobController(
		controllerName,
		makeFakeManifest(),
		events.NewInMemoryRecorder(operandName, clocktesting.NewFakePassiveClock(time.Now())),
		fakeOperatorClient,
		coreClient,
		coreInformerFactory.Batch().V1().Jobs(),
		DefaultConditions,
		optionalInformers,
		optionalJobHooks...,
	)

	// FIRST SYNC: Should detect the modified job and delete it
	err := controller.Sync(context.TODO(), factory.NewSyncContext(controllerName, events.NewInMemoryRecorder("test-job-controller", clocktesting.NewFakePassiveClock(time.Now()))))
	if err == nil {
		t.Fatalf("First sync should have returned an error when detecting modified job")
	}

	// Verify the error message indicates the job was modified and deleted
	expectedErrorMsg := "job spec was modified, old job is deleted"
	if err.Error() != expectedErrorMsg {
		t.Errorf("First sync returned unexpected error message: got %q, want %q", err.Error(), expectedErrorMsg)
	}

	// Verify the job was deleted
	_, err = coreClient.BatchV1().Jobs(operandNamespace).Get(context.TODO(), jobName, metav1.GetOptions{})
	if err == nil {
		t.Errorf("Job should have been deleted after first sync")
	}
	if !errors.IsNotFound(err) {
		t.Errorf("Expected NotFound error, got: %v", err)
	}

	// SECOND SYNC: Should succeed and ensure proper job is in place
	err = controller.Sync(context.TODO(), factory.NewSyncContext(controllerName, events.NewInMemoryRecorder("test-job-controller", clocktesting.NewFakePassiveClock(time.Now()))))
	if err != nil {
		t.Fatalf("Second sync returned unexpected error: %v", err)
	}

	// Verify the job now exists and matches expected manifest
	finalJob, err := coreClient.BatchV1().Jobs(operandNamespace).Get(context.TODO(), jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job should exist after second sync: %v", err)
	}

	// Verify the job has the expected values (not the modified ones)
	if finalJob.Labels != nil && finalJob.Labels["modified"] == "true" {
		t.Errorf("Second sync job still has modified label")
	}

	// Verify the command is correct (should be from the manifest)
	if len(finalJob.Spec.Template.Spec.Containers) > 0 {
		actualCommand := finalJob.Spec.Template.Spec.Containers[0].Command
		expectedCommand := []string{"test", "arg"} // From makeFakeManifest
		if !equality.Semantic.DeepEqual(actualCommand, expectedCommand) {
			t.Errorf("Second sync job has unexpected command: got %v, want %v", actualCommand, expectedCommand)
		}
	}

	t.Logf("Test completed successfully: job was modified, detected, and recreated properly")
}

// Infrastructure
func makeInfra() *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name:      infraConfigName,
			Namespace: v1.NamespaceAll,
		},
		Status: configv1.InfrastructureStatus{
			InfrastructureName: defaultClusterID,
		},
	}
}

type testCase struct {
	name            string
	initialObjects  testObjects
	expectedObjects testObjects
	expectErr       bool
}

type testObjects struct {
	job      *batchv1.Job
	operator *fakeOperatorInstance
}

type testContext struct {
	controller     factory.Controller
	operatorClient v1helpers.OperatorClient
	coreClient     *fakecore.Clientset
	coreInformers  coreinformers.SharedInformerFactory
}

func newTestContext(test testCase, t *testing.T, conditions []string) *testContext {
	// Add job to informer
	var initialObjects []runtime.Object
	if test.initialObjects.job != nil {
		resourceapply.SetSpecHashAnnotation(&test.initialObjects.job.ObjectMeta, test.initialObjects.job.Spec)
		initialObjects = append(initialObjects, test.initialObjects.job)
	}

	coreClient := fakecore.NewSimpleClientset(initialObjects...)
	coreInformerFactory := coreinformers.NewSharedInformerFactory(coreClient, 0 /*no resync */)

	// Fill the informer
	if test.initialObjects.job != nil {
		coreInformerFactory.Batch().V1().Jobs().Informer().GetIndexer().Add(test.initialObjects.job)
	}

	// Add global reactors
	addGenerationReactor(coreClient)

	// Add a fake Infrastructure object to informer. This is not
	// optional because it is always present in the cluster.
	initialInfras := []runtime.Object{makeInfra()}
	configClient := fakeconfig.NewSimpleClientset(initialInfras...)
	configInformerFactory := configinformers.NewSharedInformerFactory(configClient, 0)
	configInformer := configInformerFactory.Config().V1().Infrastructures().Informer()
	configInformer.GetIndexer().Add(initialInfras[0])

	// fakeOperatorInstance also fulfils the OperatorClient interface
	fakeOperatorClient := v1helpers.NewFakeOperatorClientWithObjectMeta(
		&test.initialObjects.operator.ObjectMeta,
		&test.initialObjects.operator.Spec,
		&test.initialObjects.operator.Status,
		nil, /*triggerErr func*/
	)
	optionalInformers := []factory.Informer{configInformer}
	var optionalJobHooks []JobHookFunc
	controller := NewJobController(
		controllerName,
		makeFakeManifest(),
		events.NewInMemoryRecorder(operandName, clocktesting.NewFakePassiveClock(time.Now())),
		fakeOperatorClient,
		coreClient,
		coreInformerFactory.Batch().V1().Jobs(),
		conditions,
		optionalInformers,
		optionalJobHooks...,
	)

	return &testContext{
		controller:     controller,
		operatorClient: fakeOperatorClient,
		coreClient:     coreClient,
		coreInformers:  coreInformerFactory,
	}
}

// This reactor is always enabled and bumps Job generation when it gets updated.
func addGenerationReactor(client *fakecore.Clientset) {
	client.PrependReactor("*", "jobs", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		switch a := action.(type) {
		case clientgotesting.CreateActionImpl:
			object := a.GetObject()
			job := object.(*batchv1.Job)
			job.Generation++
			return false, job, nil
		case clientgotesting.UpdateActionImpl:
			object := a.GetObject()
			job := object.(*batchv1.Job)
			job.Generation++
			return false, job, nil
		}
		return false, nil, nil
	})
}

type operatorModifier func(instance *fakeOperatorInstance) *fakeOperatorInstance

// fakeOperatorInstance is a fake Operator instance that fulfils the OperatorClient interface.
type fakeOperatorInstance struct {
	metav1.ObjectMeta
	Spec   opv1.OperatorSpec
	Status opv1.OperatorStatus
}

func makeFakeOperatorInstance(modifiers ...operatorModifier) *fakeOperatorInstance {
	instance := &fakeOperatorInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster",
			Generation: 0,
		},
		Spec: opv1.OperatorSpec{
			ManagementState: opv1.Managed,
		},
		Status: opv1.OperatorStatus{},
	}
	for _, modifier := range modifiers {
		instance = modifier(instance)
	}
	return instance
}

func withGenerations(job int64) operatorModifier {
	return func(i *fakeOperatorInstance) *fakeOperatorInstance {
		i.Status.Generations = []opv1.GenerationStatus{
			{
				Group:          batchv1.GroupName,
				LastGeneration: job,
				Name:           jobName,
				Namespace:      operandNamespace,
				Resource:       "jobs",
			},
		}
		return i
	}
}

func withTrueConditions(conditions ...string) operatorModifier {
	return func(i *fakeOperatorInstance) *fakeOperatorInstance {
		if i.Status.Conditions == nil {
			i.Status.Conditions = []opv1.OperatorCondition{}
		}
		for _, cond := range conditions {
			i.Status.Conditions = append(i.Status.Conditions, opv1.OperatorCondition{
				Type:   cond,
				Status: opv1.ConditionTrue,
			})
		}
		return i
	}
}

func withFalseConditions(conditions ...string) operatorModifier {
	return func(i *fakeOperatorInstance) *fakeOperatorInstance {
		if i.Status.Conditions == nil {
			i.Status.Conditions = []opv1.OperatorCondition{}
		}
		for _, c := range conditions {
			i.Status.Conditions = append(i.Status.Conditions, opv1.OperatorCondition{
				Type:   c,
				Status: opv1.ConditionFalse,
			})
		}
		return i
	}
}

func makeFakeManifest() []byte {
	return []byte(`
apiVersion: batch/v1
kind: Job
metadata:
  labels:
    app.kubernetes.io/name: testjob
  namespace: openshift-test
  name: testjob
spec:
  template:
    metadata:
      annotations:
        openshift.io/required-scc: "privileged"
    spec:
      containers:
        - name: testjob
          image: testimage
          imagePullPolicy: IfNotPresent
          command: [ "test", "arg" ]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 128Mi
          securityContext:
            privileged: true
            allowPrivilegeEscalation: true
      hostIPC: false
      hostNetwork: false
      hostPID: true
      serviceAccountName: test-manager
      terminationGracePeriodSeconds: 10
      restartPolicy: Never
    backoffLimit: 3`)
}

type jobModifier func(*batchv1.Job) *batchv1.Job

func makeJob(modifiers ...jobModifier) *batchv1.Job {
	manifest := makeFakeManifest()
	job := ReadJobV1OrDie(manifest)

	for _, modifier := range modifiers {
		job = modifier(job)
	}

	return job
}

func withJobGeneration(generation int64) jobModifier {
	return func(instance *batchv1.Job) *batchv1.Job {
		instance.Generation = generation
		return instance
	}
}

func withJobStatus(active, succeeded, failed int32) jobModifier {
	return func(instance *batchv1.Job) *batchv1.Job {
		instance.Status.Active = active
		instance.Status.Succeeded = succeeded
		instance.Status.Failed = failed
		return instance
	}
}

func withJobComplete() jobModifier {
	return func(instance *batchv1.Job) *batchv1.Job {
		instance.Status.Conditions = append(instance.Status.Conditions, batchv1.JobCondition{
			Type:   batchv1.JobComplete,
			Status: v1.ConditionTrue,
		})
		return instance
	}
}

func withJobFailed() jobModifier {
	return func(instance *batchv1.Job) *batchv1.Job {
		instance.Status.Conditions = append(instance.Status.Conditions, batchv1.JobCondition{
			Type:   batchv1.JobFailed,
			Status: v1.ConditionTrue,
		})
		return instance
	}
}

// withModifiedJob creates a job that differs from the expected manifest
func withModifiedJob() jobModifier {
	return func(instance *batchv1.Job) *batchv1.Job {
		// Modify the job to be different from the expected manifest
		// Change the command to make it different
		if len(instance.Spec.Template.Spec.Containers) > 0 {
			instance.Spec.Template.Spec.Containers[0].Command = []string{"modified", "command"}
		}
		// Add a label that shouldn't be there
		if instance.Labels == nil {
			instance.Labels = make(map[string]string)
		}
		instance.Labels["modified"] = "true"
		return instance
	}
}

func sanitizeJob(job *batchv1.Job) {
	// nil and empty array are the same
	if len(job.Labels) == 0 {
		job.Labels = nil
	}
	if len(job.Annotations) == 0 {
		job.Annotations = nil
	}
	// Remove random annotations set by ApplyJob
	delete(job.Annotations, specHashAnnotation)
}

func sanitizeInstanceStatus(status *opv1.OperatorStatus, testedConditions []string) {
	// Remove condition texts
	for i := range status.Conditions {
		status.Conditions[i].LastTransitionTime = metav1.Time{}
		status.Conditions[i].Message = ""
		status.Conditions[i].Reason = ""
	}
	// Sort the conditions by name to have consistent position in the array
	sort.Slice(status.Conditions, func(i, j int) bool {
		return strings.Compare(status.Conditions[i].Type, status.Conditions[j].Type) < 0
	})

	// Keep only testedConditions; preserve prior sort order
	filtered := status.Conditions[:0]
	for _, c := range status.Conditions {
		if slices.Contains(testedConditions, c.Type) {
			filtered = append(filtered, c)
		}
	}
	status.Conditions = filtered
}

func sanitizeObjectMeta(meta *metav1.ObjectMeta) {
	// Treat empty array as nil for easier comparison.
	if len(meta.Finalizers) == 0 {
		meta.Finalizers = nil
	}
}
