package jobs

import (
	"context"
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
		nodeName              *string
		existingControllers   map[string]bool
		expectControllerRun   bool
		expectedControllerKey string
	}{
		{
			name:                  "Start controller for auth job without node name",
			jobType:               tools.JobTypeAuth,
			nodeName:              nil,
			existingControllers:   make(map[string]bool),
			expectControllerRun:   true,
			expectedControllerKey: "tnf-auth-job",
		},
		{
			name:                  "Start controller for auth job with node name",
			jobType:               tools.JobTypeAuth,
			nodeName:              stringPtr("master-0"),
			existingControllers:   make(map[string]bool),
			expectControllerRun:   true,
			expectedControllerKey: "tnf-auth-job-master-0",
		},
		{
			name:     "Skip starting controller when already running",
			jobType:  tools.JobTypeSetup,
			nodeName: nil,
			existingControllers: map[string]bool{
				"tnf-setup-job": true,
			},
			expectControllerRun:   false,
			expectedControllerKey: "tnf-setup-job",
		},
		{
			name:     "Start different controller when another is running",
			jobType:  tools.JobTypeFencing,
			nodeName: nil,
			existingControllers: map[string]bool{
				"tnf-setup-job": true,
			},
			expectControllerRun:   true,
			expectedControllerKey: "tnf-fencing-job",
		},
		{
			name:     "Start controller for different node when same job type exists",
			jobType:  tools.JobTypeAuth,
			nodeName: stringPtr("master-1"),
			existingControllers: map[string]bool{
				"tnf-auth-job-master-0": true,
			},
			expectControllerRun:   true,
			expectedControllerKey: "tnf-auth-job-master-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the global tracking maps
			runningControllersMutex.Lock()
			runningControllers = make(map[string]bool)
			for k, v := range tt.existingControllers {
				runningControllers[k] = v
			}
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
				tt.nodeName,
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
		name                 string
		jobType              tools.JobType
		nodeName             *string
		setupClient          func() *fake.Clientset
		expectError          bool
		errorContains        string
		expectJobDeleted     bool
		expectWaitForStopped bool
	}{
		{
			name:     "Job does not exist - just runs controller",
			jobType:  tools.JobTypeAuth,
			nodeName: stringPtr("master-0"),
			setupClient: func() *fake.Clientset {
				// No job exists
				return fake.NewSimpleClientset()
			},
			expectError:          false,
			expectJobDeleted:     false,
			expectWaitForStopped: false,
		},
		{
			name:     "Job exists and stops successfully - deletes and runs controller",
			jobType:  tools.JobTypeSetup,
			nodeName: nil,
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
			expectError:          false,
			expectJobDeleted:     true,
			expectWaitForStopped: true,
		},
		{
			name:     "Job exists but Get returns error - returns error",
			jobType:  tools.JobTypeAuth,
			nodeName: stringPtr("master-0"),
			setupClient: func() *fake.Clientset {
				client := fake.NewSimpleClientset()
				client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewServiceUnavailable("service unavailable")
				})
				return client
			},
			expectError:          true,
			errorContains:        "failed to check for existing job",
			expectJobDeleted:     false,
			expectWaitForStopped: false,
		},
		{
			name:     "Job exists but WaitForStopped times out - returns error",
			jobType:  tools.JobTypeFencing,
			nodeName: nil,
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
			expectError:          true,
			errorContains:        "failed to wait for update-setup job",
			expectJobDeleted:     false,
			expectWaitForStopped: true,
		},
		{
			name:     "Job exists, stops, but delete fails - returns error",
			jobType:  tools.JobTypeAfterSetup,
			nodeName: stringPtr("master-1"),
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tnf-after-setup-job-master-1",
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
					return true, nil, apierrors.NewForbidden(batchv1.Resource("jobs"), "tnf-after-setup-job-master-1", nil)
				})

				return client
			},
			expectError:          true,
			errorContains:        "failed to delete existing update-setup job",
			expectJobDeleted:     false,
			expectWaitForStopped: true,
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
				tt.nodeName,
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

			// Verify controller was started (only if no early error)
			jobName := tt.jobType.GetJobName(tt.nodeName)

			// Only verify controller started if we didn't get an error before RunTNFJobController
			if !tt.expectError || tt.expectWaitForStopped {
				runningControllersMutex.Lock()
				isRunning := runningControllers[jobName]
				runningControllersMutex.Unlock()
				require.True(t, isRunning, "Expected controller to be started")
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

	for i := 0; i < numCalls; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errors[idx] = RestartJobOrRunController(
				context.Background(),
				tools.JobTypeSetup,
				nil,
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
