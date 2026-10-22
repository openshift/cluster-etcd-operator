package jobs

/*
TEST COVERAGE SUMMARY - lifecycle_test.go
==========================================

This file tests TNF job controller restart logic and multi-node retry state machine.

WHAT'S TESTED
-------------

Job Controller Lifecycle:
├── TestRestartJobOrRunController - Job restart and cleanup logic
│   ├── Job does not exist - just runs controller
│   └── Job exists and stops successfully - deletes and runs controller
├── TestSyncMultiNodeJobState_RetryProgression - Multi-node retry state machine
│   ├── Job fails on node 0 -> advances to node 1
│   ├── All nodes fail in attempt 1 -> starts attempt 2
│   ├── Max attempts exhausted -> returns error with DegradedMessageMaxRetries, resets to attempt 1
│   └── Job succeeds -> returns no error
├── TestSyncMultiNodeJobState_DriftDetection - Infrastructure drift detection
│   ├── schedulableNodesFunc changes (node added) -> refresh placement, preserve job
│   ├── schedulableNodesFunc changes (node removed) -> refresh placement, preserve job
│   ├── jobConfigFunc changes (config updated) -> reset state, delete job
│   └── No drift (nodes and config unchanged) -> state progression continues
├── TestAdmitReadyNodes - Creation admission readiness check
│   ├── Affected nodes ready -> creation admitted
│   ├── Affected nodes not ready < 10min -> job blocked, no degraded condition
│   └── Affected nodes not ready >= 10min -> job blocked, returns error (triggers Degraded)
└── TestSyncMultiNodeJobState_NoSchedulableNodesDegradedCondition - Schedulable nodes availability
    ├── No schedulable nodes < 10min -> job blocked, no degraded condition
    └── No schedulable nodes >= 10min -> job blocked, returns error (triggers Degraded)
*/

import (
	"context"
	"strings"
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
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func TestRestartJobOrRunController(t *testing.T) {
	tests := []struct {
		name                    string
		jobType                 tools.JobType
		schedulableNodesFunc    SchedulableNodesFunc
		retries                 int
		setupClient             func() *fake.Clientset
		expectError             bool
		errorContains           string
		expectControllerStarted bool
	}{
		{
			name:                 "Job does not exist - just runs controller",
			jobType:              tools.JobTypeSetup,
			schedulableNodesFunc: nil,
			retries:              3,
			setupClient: func() *fake.Clientset {
				// No job exists
				return fake.NewClientset()
			},
			expectError:             false,
			expectControllerStarted: true,
		},
		{
			name:                 "Job exists and stops successfully - deletes and runs controller",
			jobType:              tools.JobTypeSetup,
			schedulableNodesFunc: nil,
			retries:              3,
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
				client := fake.NewClientset(job)

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
			expectControllerStarted: true,
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
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
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

			// Execute - only testing cluster job restart
			err := RestartClusterJobOrRunController(
				ctx,
				tt.jobType,
				tt.schedulableNodesFunc,
				nil, // createAdmissionHook
				nil, // jobConfigFunc
				tt.retries,
				controllerContext,
				fakeOperatorClient,
				client,
				kubeInformersForNamespaces,
				DefaultConditions,
				1*time.Second, // timeout
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

			// Cluster jobs don't have node-specific names
			jobName := tt.jobType.GetJobName(nil)

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

func TestSyncMultiNodeJobState_RetryProgression(t *testing.T) {
	// This test verifies the multi-node retry state machine:
	// - Job fails on node 0 -> retries on node 1
	// - Job fails on node 1 -> new attempt, back to node 0
	// - Max attempts exhausted -> returns error (not nil), resets to attempt 1
	// - Job succeeds -> returns no error

	ctx := context.Background()
	jobName := "tnf-update-setup-job"
	maxRetries := 2

	// Create two fake nodes
	node0 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-0", UID: "uid-0"}}
	node1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-1", UID: "uid-1"}}
	targetNodesFunc := func() ([]*corev1.Node, error) {
		return []*corev1.Node{node0, node1}, nil
	}

	// Setup fake clients
	fakeKubeClient := fake.NewClientset()
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		&operatorv1.StaticPodOperatorStatus{},
		nil,
		nil,
	)

	// Reset global state
	retryStateMutex.Lock()
	retryState = make(map[string]*JobRetryState)
	retryStateMutex.Unlock()

	// Helper to get current retry state
	getState := func() *JobRetryState {
		retryStateMutex.Lock()
		defer retryStateMutex.Unlock()
		state, exists := retryState[jobName]
		if !exists {
			return nil
		}
		state.Mu.Lock()
		defer state.Mu.Unlock()
		return &JobRetryState{
			AttemptNumber: state.AttemptNumber,
			NodeIndex:     state.NodeIndex,
			TargetNodes:   append([]string{}, state.TargetNodes...),
		}
	}

	// Step 1: Initialize - should create state at attempt 1, node 0
	err := syncMultiNodeJobState(ctx, jobName, targetNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)
	require.NoError(t, err)
	state := getState()
	require.NotNil(t, state)
	require.Equal(t, 1, state.AttemptNumber, "Should start at attempt 1")
	require.Equal(t, 0, state.NodeIndex, "Should start at node index 0")

	// Step 2: Job fails on node 0 -> should advance to node 1
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: operatorclient.TargetNamespace},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	_, err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(ctx, failedJob, metav1.CreateOptions{})
	require.NoError(t, err)

	err = syncMultiNodeJobState(ctx, jobName, targetNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)
	require.NoError(t, err)
	state = getState()
	require.Equal(t, 1, state.AttemptNumber, "Should still be attempt 1")
	require.Equal(t, 1, state.NodeIndex, "Should advance to node index 1")

	// Delete job to simulate ApplyJob detecting drift (NodeName changed) and recreating
	err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, jobName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Step 3: Job fails on node 1 -> should start attempt 2, back to node 0
	_, err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(ctx, failedJob.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	err = syncMultiNodeJobState(ctx, jobName, targetNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)
	require.NoError(t, err)
	state = getState()
	require.Equal(t, 2, state.AttemptNumber, "Should advance to attempt 2")
	require.Equal(t, 0, state.NodeIndex, "Should reset to node index 0")

	// Delete job to simulate ApplyJob detecting drift
	err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, jobName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Step 4: Exhaust attempt 2 (fail on both nodes) -> should set degraded and reset to attempt 1
	// Fail on node 0
	_, err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(ctx, failedJob.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	err = syncMultiNodeJobState(ctx, jobName, targetNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)
	require.NoError(t, err)
	// Delete and recreate on node 1
	err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, jobName, metav1.DeleteOptions{})
	require.NoError(t, err)
	_, err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(ctx, failedJob.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	err = syncMultiNodeJobState(ctx, jobName, targetNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)
	require.Error(t, err, "syncMultiNodeJobState returns error when max retries exceeded")
	require.Contains(t, err.Error(), DegradedMessageMaxRetries, "Error should contain MaxRetriesExceeded message")
	state = getState()
	require.Equal(t, 1, state.AttemptNumber, "Should reset to attempt 1 after exhausting max attempts")
	require.Equal(t, 0, state.NodeIndex, "Should reset to node index 0")

	// Delete job to simulate ApplyJob detecting drift
	err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, jobName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Step 5: Job succeeds -> should return no error
	successJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: operatorclient.TargetNamespace},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	_, err = fakeKubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(ctx, successJob, metav1.CreateOptions{})
	require.NoError(t, err)

	err = syncMultiNodeJobState(ctx, jobName, targetNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)
	require.NoError(t, err)
}

func TestSyncMultiNodeJobState_DriftDetection(t *testing.T) {
	ctx := context.Background()
	jobName := "tnf-fencing-job"
	node0 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-0", UID: "uid-0"}}
	node1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-1", UID: "uid-1"}}
	for _, tc := range []struct {
		name                            string
		next                            []*corev1.Node
		condition                       batchv1.JobConditionType
		configIncludesNodes, wantDelete bool
		failAfterPlacement              bool
	}{
		{name: "completed job survives executor removal", next: []*corev1.Node{node0}, condition: batchv1.JobComplete},
		{name: "node set explicitly included in config", next: []*corev1.Node{node0}, condition: batchv1.JobComplete, configIncludesNodes: true, wantDelete: true},
		{name: "executor disappears before the Job fails", next: []*corev1.Node{node0}, failAfterPlacement: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetJobRetryState(jobName)
			t.Cleanup(func() { resetJobRetryState(jobName) })
			client := fake.NewClientset()
			op := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{}, nil, nil)
			nodes := []*corev1.Node{node0, node1}
			nodesFunc := func() ([]*corev1.Node, error) { return nodes, nil }
			configFunc := func() (string, error) {
				config := "v1"
				if tc.configIncludesNodes {
					config += strings.Join(tools.GetNodeNames(nodes), ",")
				}
				return config, nil
			}
			err := syncMultiNodeJobState(ctx, jobName, nodesFunc, configFunc, 3, client, op)
			require.NoError(t, err)
			state := retryState[jobName]
			state.AttemptNumber, state.NodeIndex = 2, 1
			job := ReadJobV1OrDie(bindata.MustAsset("tnfdeployment/job.yaml"))
			job.Name = jobName
			require.NoError(t, configureMultiNodeJob(job, 3))
			recorder := events.NewInMemoryRecorder("placement-test", clock.RealClock{})
			job, _, err = ApplyJob(ctx, client.BatchV1(), recorder, job, -1)
			require.NoError(t, err)
			job.Status.Active = 1
			if tc.condition != "" {
				job.Status.Active = 0
				job.Status.Conditions = []batchv1.JobCondition{{Type: tc.condition, Status: corev1.ConditionTrue}}
			}
			_, err = client.BatchV1().Jobs(operatorclient.TargetNamespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
			require.NoError(t, err)
			nodes = tc.next
			err = syncMultiNodeJobState(ctx, jobName, nodesFunc, configFunc, 3, client, op)
			require.NoError(t, err)
			if tc.wantDelete {
				_, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err))
				require.Equal(t, 1, state.AttemptNumber)
				return
			}
			require.Equal(t, 2, state.AttemptNumber, "placement changes must not restart the retry budget")
			// Exercise actual application as well: executor-only spec changes
			// must preserve the running/completed cluster Job.
			required := ReadJobV1OrDie(bindata.MustAsset("tnfdeployment/job.yaml"))
			required.Name = jobName
			require.NoError(t, configureMultiNodeJob(required, 3))
			actual, changed, err := ApplyJob(ctx, client.BatchV1(), recorder, required, job.Generation)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, "master-1", actual.Spec.Template.Spec.NodeName)
			if tc.failAfterPlacement {
				actual.Status.Active = 0
				actual.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
				_, err = client.BatchV1().Jobs(operatorclient.TargetNamespace).UpdateStatus(ctx, actual, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, syncMultiNodeJobState(ctx, jobName, nodesFunc, configFunc, 3, client, op))
				require.Equal(t, 0, state.NodeIndex)
				require.Equal(t, 2, state.AttemptNumber, "refreshing while running must not consume a retry round")
			}
		})
	}
}

func TestAdmitReadyNodes(t *testing.T) {
	name := "tnf-fencing-job"
	for _, tc := range []struct {
		name                   string
		ready                  bool
		blocked                time.Duration
		wantAllowed, wantError bool
	}{
		{"ready", true, 0, true, false},
		{"not ready", false, 5 * time.Minute, false, false},
		{"blocked too long", false, 11 * time.Minute, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobBlockedMutex.Lock()
			jobBlockedSince = map[string]time.Time{name: time.Now().Add(-tc.blocked)}
			jobBlockedMutex.Unlock()
			t.Cleanup(func() {
				jobBlockedMutex.Lock()
				defer jobBlockedMutex.Unlock()
				delete(jobBlockedSince, name)
			})
			status := corev1.ConditionFalse
			if tc.ready {
				status = corev1.ConditionTrue
			}
			node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}}}
			allowed, err := AdmitReadyNodes([]*corev1.Node{node}, name)
			require.Equal(t, tc.wantAllowed, allowed)
			require.Equal(t, tc.wantError, err != nil)
		})
	}
}

func TestSyncMultiNodeJobState_NoSchedulableNodesDegradedCondition(t *testing.T) {
	// This test verifies degraded condition logic for schedulable nodes:
	// - schedulableNodesFunc returns empty array -> blocks job, returns error after 10min
	// - schedulableNodesFunc returns nodes -> clears degraded condition, allows job

	ctx := context.Background()
	jobName := "tnf-fencing-job"
	maxRetries := 3

	// Create a ready node
	readyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "master-0", UID: "uid-0"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	tests := []struct {
		name                  string
		schedulableNodes      []*corev1.Node
		blockedDuration       time.Duration
		expectRetryStateExist bool
		expectError           bool
	}{
		{
			name:                  "schedulable nodes available - job admitted",
			schedulableNodes:      []*corev1.Node{readyNode},
			blockedDuration:       0,
			expectRetryStateExist: true,
			expectError:           false,
		},
		{
			name:                  "no schedulable nodes < 10min - job blocked, no error",
			schedulableNodes:      []*corev1.Node{},
			blockedDuration:       5 * time.Minute,
			expectRetryStateExist: false,
			expectError:           false,
		},
		{
			name:                  "no schedulable nodes >= 10min - job blocked, error returned",
			schedulableNodes:      []*corev1.Node{},
			blockedDuration:       11 * time.Minute,
			expectRetryStateExist: false,
			expectError:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset global state
			retryStateMutex.Lock()
			retryState = make(map[string]*JobRetryState)
			retryStateMutex.Unlock()

			jobNoSchedulableNodesMutex.Lock()
			jobNoSchedulableNodesSince = make(map[string]time.Time)
			jobNoSchedulableNodesMutex.Unlock()

			// Setup fake clients
			fakeKubeClient := fake.NewClientset()
			// Add readyNode to fake client
			_, err := fakeKubeClient.CoreV1().Nodes().Create(ctx, readyNode, metav1.CreateOptions{})
			require.NoError(t, err)
			// Add schedulable nodes to fake client (if any beyond readyNode)
			for _, node := range tt.schedulableNodes {
				if node.Name != readyNode.Name {
					_, err := fakeKubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
					require.NoError(t, err)
				}
			}

			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				u.StaticPodOperatorStatus(),
				nil,
				nil,
			)

			schedulableNodesFunc := func() ([]*corev1.Node, error) {
				return tt.schedulableNodes, nil
			}

			// Simulate blocked time if needed
			if tt.blockedDuration > 0 {
				jobNoSchedulableNodesMutex.Lock()
				jobNoSchedulableNodesSince[jobName] = time.Now().Add(-tt.blockedDuration)
				jobNoSchedulableNodesMutex.Unlock()
			}

			// Sync state
			err = syncMultiNodeJobState(ctx, jobName, schedulableNodesFunc, nil, maxRetries, fakeKubeClient, fakeOperatorClient)

			// Verify error expectation
			if tt.expectError {
				require.Error(t, err, "Expected error when blocked >= 10min")
			} else {
				require.NoError(t, err, "Expected no error")
			}

			// Verify retry state existence
			retryStateMutex.Lock()
			_, stateExists := retryState[jobName]
			retryStateMutex.Unlock()
			require.Equal(t, tt.expectRetryStateExist, stateExists, "Retry state existence mismatch")

			// Verify degraded condition cleared when unblocked (if it exists)
			// Note: Condition might not exist if blocking resolved before 10min timeout
			if !tt.expectError && tt.blockedDuration > 0 {
				_, status, _, _ := fakeOperatorClient.GetStaticPodOperatorState()
				expectedConditionType := tools.ToPascalCase(jobName) + "Degraded"
				for _, cond := range status.Conditions {
					if cond.Type == expectedConditionType {
						require.Equal(t, operatorv1.ConditionFalse, cond.Status, "Degraded should be cleared when unblocked")
						break
					}
				}
			}
		})
	}
}

// testNodeInformer is a minimal mock of cache.SharedIndexInformer for testing
type testNodeInformer struct {
	indexer cache.Indexer
	synced  bool
}

func (m *testNodeInformer) GetIndexer() cache.Indexer {
	return m.indexer
}

func (m *testNodeInformer) HasSynced() bool {
	return m.synced
}

func (m *testNodeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *testNodeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *testNodeInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}

func (m *testNodeInformer) GetStore() cache.Store {
	return m.indexer
}

func (m *testNodeInformer) GetController() cache.Controller {
	return nil
}

func (m *testNodeInformer) Run(stopCh <-chan struct{}) {
}

func (m *testNodeInformer) HasStarted() bool {
	return true
}

func (m *testNodeInformer) LastSyncResourceVersion() string {
	return ""
}

func (m *testNodeInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}

func (m *testNodeInformer) SetTransform(f cache.TransformFunc) error {
	return nil
}

func (m *testNodeInformer) IsStopped() bool {
	return false
}

func (m *testNodeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *testNodeInformer) AddIndexers(indexers cache.Indexers) error {
	return nil
}

func (m *testNodeInformer) RunWithContext(ctx context.Context) {
}

func (m *testNodeInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}
