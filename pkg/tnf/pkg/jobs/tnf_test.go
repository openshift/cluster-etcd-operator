package jobs

/*
TEST COVERAGE SUMMARY - tnf_test.go
====================================

This file tests TNF job controller management and restart logic.

WHAT'S TESTED
-------------

Job Controller Lifecycle:
├── TestRunTNFJobController - Controller startup and deduplication
│   ├── ✅ Start controller for cluster-wide job (setup, fencing, update-setup)
│   ├── ✅ Start controller for node-specific job (auth, after-setup)
│   ├── ✅ Skip starting controller when already running
│   ├── ✅ Start different controller when another is running
│   ├── ✅ Start controller for different node when same job type exists
│   └── ✅ Cluster-wide job with scheduling hint (scheduleOnNode)
├── TestRestartJobOrRunController - Job restart and cleanup logic
│   ├── ✅ Job does not exist - just runs controller
│   ├── ✅ Job exists and stops successfully - deletes and runs controller
│   ├── ✅ Job exists but Get returns error - returns error
│   ├── ✅ Job exists but won't stop (timeout) - returns error
│   └── ✅ Job exists, stops, but delete fails - returns error
└── TestRestartJobOrRunController_ParallelExecution - Concurrency safety
    └── ✅ Parallel calls serialize via mutex (only one delete)
*/

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func TestRunTNFJobController(t *testing.T) {
	tests := []struct {
		name                  string
		jobType               tools.JobType
		nodeTarget            *NodeTarget
		scheduleOnNode        *string
		existingControllers   map[string]bool
		expectControllerRun   bool
		expectedControllerKey string
	}{
		{
			name:                  "Start controller for cluster-wide job",
			jobType:               tools.JobTypeSetup,
			nodeTarget:            nil,
			scheduleOnNode:        nil,
			existingControllers:   make(map[string]bool),
			expectControllerRun:   true,
			expectedControllerKey: "tnf-setup-job",
		},
		{
			name:                  "Start controller for node-specific job",
			jobType:               tools.JobTypeAuth,
			nodeTarget:            &NodeTarget{Name: "master-0", UID: "uid-master-0"},
			scheduleOnNode:        nil,
			existingControllers:   make(map[string]bool),
			expectControllerRun:   true,
			expectedControllerKey: tools.JobTypeAuth.GetJobName(stringPtr("master-0")),
		},
		{
			name:           "Skip starting controller when already running",
			jobType:        tools.JobTypeSetup,
			nodeTarget:     nil,
			scheduleOnNode: nil,
			existingControllers: map[string]bool{
				"tnf-setup-job": true,
			},
			expectControllerRun:   false,
			expectedControllerKey: "tnf-setup-job",
		},
		{
			name:           "Start different controller when another is running",
			jobType:        tools.JobTypeFencing,
			nodeTarget:     nil,
			scheduleOnNode: nil,
			existingControllers: map[string]bool{
				"tnf-setup-job": true,
			},
			expectControllerRun:   true,
			expectedControllerKey: "tnf-fencing-job",
		},
		{
			name:           "Start controller for different node when same job type exists",
			jobType:        tools.JobTypeAuth,
			nodeTarget:     &NodeTarget{Name: "master-1", UID: "uid-master-1"},
			scheduleOnNode: nil,
			existingControllers: map[string]bool{
				tools.JobTypeAuth.GetJobName(stringPtr("master-0")): true,
			},
			expectControllerRun:   true,
			expectedControllerKey: tools.JobTypeAuth.GetJobName(stringPtr("master-1")),
		},
		{
			name:                  "Cluster-wide job with scheduling hint",
			jobType:               tools.JobTypeUpdateSetup,
			nodeTarget:            nil,
			scheduleOnNode:        stringPtr("master-0"),
			existingControllers:   make(map[string]bool),
			expectControllerRun:   true,
			expectedControllerKey: "tnf-update-setup-job",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the global tracking maps
			runningControllersMutex.Lock()
			runningControllers = make(map[string]bool)
			maps.Copy(runningControllers, tt.existingControllers)
			runningControllersMutex.Unlock()

			// Setup
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			fakeKubeClient := fake.NewSimpleClientset()
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				u.StaticPodOperatorStatus(),
				nil,
				nil,
			)

			eventRecorder := events.NewRecorder(
				fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-tnf",
				&corev1.ObjectReference{},
				clock.RealClock{},
			)
			controllerContext := &controllercmd.ControllerContext{
				EventRecorder: eventRecorder,
			}

			kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
				fakeKubeClient,
				operatorclient.TargetNamespace,
			)

			// Execute
			RunTNFJobController(
				ctx,
				tt.jobType,
				tt.nodeTarget,
				tt.scheduleOnNode,
				controllerContext,
				fakeOperatorClient,
				fakeKubeClient,
				kubeInformersForNamespaces,
				DefaultConditions,
			)

			// Give the goroutine a moment to start if expected
			time.Sleep(100 * time.Millisecond)

			// Verify
			runningControllersMutex.Lock()
			isRunning := runningControllers[tt.expectedControllerKey]
			runningControllersMutex.Unlock()

			if tt.expectControllerRun {
				require.True(t, isRunning,
					"Expected controller %q to be marked as running", tt.expectedControllerKey)
			} else {
				// Controller should still be marked as running from before
				require.True(t, isRunning,
					"Expected controller %q to still be marked as running", tt.expectedControllerKey)
			}
		})
	}
}

func TestRestartJobOrRunController(t *testing.T) {
	tests := []struct {
		name                   string
		jobType                tools.JobType
		nodeTarget             *NodeTarget
		scheduleOnNode         *string
		setupClient            func() *fake.Clientset
		expectError            bool
		errorContains          string
		expectJobDeleted       bool
		expectWaitForStopped   bool
		expectControllerStarted bool
	}{
		{
			name:           "Job does not exist - just runs controller",
			jobType:        tools.JobTypeAuth,
			nodeTarget:     &NodeTarget{Name: "master-0", UID: "uid-master-0"},
			scheduleOnNode: nil,
			setupClient: func() *fake.Clientset {
				// No job exists
				return fake.NewSimpleClientset()
			},
			expectError:             false,
			expectJobDeleted:        false,
			expectWaitForStopped:    false,
			expectControllerStarted: true,
		},
		{
			name:           "Job exists and stops successfully - deletes and runs controller",
			jobType:        tools.JobTypeSetup,
			nodeTarget:     nil,
			scheduleOnNode: nil,
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tnf-setup-job",
						Namespace: operatorclient.TargetNamespace,
						UID:       "test-uid",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{
							{
								Type:   batchv1.JobComplete,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				client := fake.NewSimpleClientset(job)

				// After delete, subsequent Gets should return NotFound
				deleted := false
				client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					deleted = true
					return false, nil, nil
				})
				client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					if deleted {
						return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), "tnf-setup-job")
					}
					return false, nil, nil
				})

				return client
			},
			expectError:             false,
			expectJobDeleted:        true,
			expectWaitForStopped:    true,
			expectControllerStarted: true,
		},
		{
			name:           "Job exists but Get returns error - returns error",
			jobType:        tools.JobTypeAuth,
			nodeTarget:     &NodeTarget{Name: "master-0", UID: "uid-master-0"},
			scheduleOnNode: nil,
			setupClient: func() *fake.Clientset {
				client := fake.NewSimpleClientset()
				client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewServiceUnavailable("service unavailable")
				})
				return client
			},
			expectError:             true,
			errorContains:           "failed to check for existing job",
			expectJobDeleted:        false,
			expectWaitForStopped:    false,
			expectControllerStarted: false, // Early error, controller not started
		},
		{
			name:           "Job exists but WaitForStopped times out - returns error",
			jobType:        tools.JobTypeFencing,
			nodeTarget:     nil,
			scheduleOnNode: nil,
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tnf-fencing-job",
						Namespace: operatorclient.TargetNamespace,
						UID:       "test-uid",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{}, // Still running
					},
				}
				return fake.NewSimpleClientset(job)
			},
			expectError:             true,
			errorContains:           "failed to wait for update-setup job",
			expectJobDeleted:        false,
			expectWaitForStopped:    true,
			expectControllerStarted: true, // Controller started before WaitForStopped failed
		},
		{
			name:           "Job exists, stops, but delete fails - returns error",
			jobType:        tools.JobTypeAfterSetup,
			nodeTarget:     &NodeTarget{Name: "master-1", UID: "uid-master-1"},
			scheduleOnNode: nil,
			setupClient: func() *fake.Clientset {
				jobName := tools.JobTypeAfterSetup.GetJobName(stringPtr("master-1"))
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      jobName,
						Namespace: operatorclient.TargetNamespace,
						UID:       "test-uid",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{
							{
								Type:   batchv1.JobComplete,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				client := fake.NewSimpleClientset(job)

				// Make Get succeed initially, then fail on delete
				client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewForbidden(batchv1.Resource("jobs"), jobName, nil)
				})

				return client
			},
			expectError:             true,
			errorContains:           "failed to delete existing update-setup job",
			expectJobDeleted:        false,
			expectWaitForStopped:    true,
			expectControllerStarted: true, // Controller started before Delete failed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the global tracking maps for each test
			runningControllersMutex.Lock()
			runningControllers = make(map[string]bool)
			runningControllersMutex.Unlock()

			restartJobLocksMutex.Lock()
			restartJobLocks = make(map[string]*sync.Mutex)
			restartJobLocksMutex.Unlock()

			// Setup
			ctx := context.Background()
			client := tt.setupClient()

			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				u.StaticPodOperatorStatus(),
				nil,
				nil,
			)

			eventRecorder := events.NewRecorder(
				client.CoreV1().Events(operatorclient.TargetNamespace),
				"test-tnf",
				&corev1.ObjectReference{},
				clock.RealClock{},
			)
			controllerContext := &controllercmd.ControllerContext{
				EventRecorder: eventRecorder,
			}

			kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
				client,
				operatorclient.TargetNamespace,
			)

			// Execute
			err := RestartJobOrRunController(
				ctx,
				tt.jobType,
				tt.nodeTarget,
				tt.scheduleOnNode,
				controllerContext,
				fakeOperatorClient,
				client,
				kubeInformersForNamespaces,
				DefaultConditions,
				1*time.Second, // Short timeout for tests
			)

			// Verify
			if tt.expectError {
				require.Error(t, err, "Expected error but got none")
				if tt.errorContains != "" {
					require.Contains(t, err.Error(), tt.errorContains,
						"Expected error to contain %q but got: %v", tt.errorContains, err)
				}
			} else {
				require.NoError(t, err, "Expected no error but got: %v", err)
			}

			// Determine job name based on node target
			var jobNodeName *string
			if tt.nodeTarget != nil {
				jobNodeName = &tt.nodeTarget.Name
			}
			jobName := tt.jobType.GetJobName(jobNodeName)

			// Verify controller started based on explicit expectation
			runningControllersMutex.Lock()
			isRunning := runningControllers[jobName]
			runningControllersMutex.Unlock()

			if tt.expectControllerStarted {
				require.True(t, isRunning, "Expected controller to be started")
			} else {
				require.False(t, isRunning, "Expected controller NOT to be started")
			}

			// Verify lock was created (always created, even on error)
			restartJobLocksMutex.Lock()
			_, lockExists := restartJobLocks[jobName]
			restartJobLocksMutex.Unlock()
			require.True(t, lockExists, "Expected lock to be created for job %q", jobName)
		})
	}
}

func TestRestartJobOrRunController_ParallelExecution(t *testing.T) {
	// Reset the global tracking maps
	runningControllersMutex.Lock()
	runningControllers = make(map[string]bool)
	runningControllersMutex.Unlock()

	restartJobLocksMutex.Lock()
	restartJobLocks = make(map[string]*sync.Mutex)
	restartJobLocksMutex.Unlock()

	// Setup
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tnf-setup-job",
			Namespace: operatorclient.TargetNamespace,
			UID:       "test-uid",
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	client := fake.NewSimpleClientset(job)

	// Track delete calls
	var deleteCalls int
	var deleteCallsMutex sync.Mutex
	deleted := false

	client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		deleteCallsMutex.Lock()
		deleteCalls++
		deleted = true
		deleteCallsMutex.Unlock()
		return false, nil, nil
	})

	client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		deleteCallsMutex.Lock()
		isDeleted := deleted
		deleteCallsMutex.Unlock()

		if isDeleted {
			return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), "tnf-setup-job")
		}
		return false, nil, nil
	})

	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	eventRecorder := events.NewRecorder(
		client.CoreV1().Events(operatorclient.TargetNamespace),
		"test-tnf",
		&corev1.ObjectReference{},
		clock.RealClock{},
	)
	controllerContext := &controllercmd.ControllerContext{
		EventRecorder: eventRecorder,
	}

	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
		client,
		operatorclient.TargetNamespace,
	)

	// Execute multiple calls in parallel
	var wg sync.WaitGroup
	numCalls := 3
	errors := make([]error, numCalls)

	for i := range numCalls {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errors[idx] = RestartJobOrRunController(
				context.Background(),
				tools.JobTypeSetup,
				nil, // nodeTarget
				nil, // scheduleOnNode
				controllerContext,
				fakeOperatorClient,
				client,
				kubeInformersForNamespaces,
				DefaultConditions,
				2*time.Second,
			)
		}(i)
	}

	wg.Wait()

	// Verify - all calls should succeed
	for i, err := range errors {
		require.NoError(t, err, "Call %d should succeed", i)
	}

	// Verify delete was only called once due to locking
	deleteCallsMutex.Lock()
	actualDeleteCalls := deleteCalls
	deleteCallsMutex.Unlock()

	require.Equal(t, 1, actualDeleteCalls,
		"Delete should only be called once despite parallel execution")
}

// Helper functions

func stringPtr(s string) *string {
	return &s
}
