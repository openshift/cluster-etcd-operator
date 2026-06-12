package operator

/*
TEST COVERAGE SUMMARY - lifecycle_job_controllers_test.go
==========================================================

This file tests TNF job controller startup logic for Two-Node Fencing clusters.

WHAT'S TESTED
-------------

Job Controller Startup:
├── TestRetryInitialTransitionOrDegrade - Bootstrap retry and degraded condition
│   ├── ✅ Success on first attempt (condition cleared)
│   ├── ✅ Success after retries (condition cleared)
│   └── ✅ Failure after all retries (condition set to degraded)
└── TestStartJobControllers - Entry point logic and path selection
    ├── Before transition (bootstrap path):
    │   ├── ✅ 2 ready nodes → bootstrap with retry
    │   ├── ✅ 1 ready node → wait
    │   └── ✅ 3 nodes → skip (pacemaker only supports 2)
    └── After transition (post-transition path):
        ├── ✅ 2 ready nodes → ensure running
        ├── ✅ 1 ready node → ensure running (single-node case)
        └── ✅ 0 ready nodes → skip
*/

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclientfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	extinfops "github.com/openshift/client-go/operator/informers/externalversions"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// TestRetryInitialTransitionOrDegrade tests the exponential backoff retry logic
// and TNFJobControllersDegraded condition management during initial bootstrap.
func TestRetryInitialTransitionOrDegrade(t *testing.T) {
	tests := []struct {
		name                    string
		setupMockStartFunc      func() func(context.Context, []*corev1.Node, *controllercmd.ControllerContext, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error
		expectDegradedCondition bool
		expectDegradedStatus    operatorv1.ConditionStatus
		expectRetries           bool
	}{
		{
			name: "Success on first attempt",
			setupMockStartFunc: func() func(context.Context, []*corev1.Node, *controllercmd.ControllerContext, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error {
				return func(_ context.Context, _ []*corev1.Node, _ *controllercmd.ControllerContext, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
					return nil
				}
			},
			expectDegradedCondition: true,
			expectDegradedStatus:    operatorv1.ConditionFalse,
			expectRetries:           false,
		},
		{
			name: "Success after retries",
			setupMockStartFunc: func() func(context.Context, []*corev1.Node, *controllercmd.ControllerContext, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error {
				attemptCount := 0
				return func(_ context.Context, _ []*corev1.Node, _ *controllercmd.ControllerContext, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
					attemptCount++
					if attemptCount < 3 {
						return &retryableError{msg: "temporary failure"}
					}
					return nil
				}
			},
			expectDegradedCondition: true,
			expectDegradedStatus:    operatorv1.ConditionFalse,
			expectRetries:           true,
		},
		{
			name: "Failure after all retries",
			setupMockStartFunc: func() func(context.Context, []*corev1.Node, *controllercmd.ControllerContext, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error {
				return func(_ context.Context, _ []*corev1.Node, _ *controllercmd.ControllerContext, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
					return &retryableError{msg: "persistent failure"}
				}
			},
			expectDegradedCondition: true,
			expectDegradedStatus:    operatorv1.ConditionTrue,
			expectRetries:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup test environment
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			fakeKubeClient := fake.NewSimpleClientset()
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				u.StaticPodOperatorStatus(),
				nil,
				nil,
			)

			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-retry", &corev1.ObjectReference{}, clock.RealClock{})

			controllerContext := &controllercmd.ControllerContext{
				EventRecorder: eventRecorder,
			}

			kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
				fakeKubeClient,
				operatorclient.TargetNamespace,
			)

			// Create etcd informer
			operatorClientFake := operatorversionedclientfake.NewClientset()
			etcdInformers := extinfops.NewSharedInformerFactory(operatorClientFake, 10*time.Minute)

			// Create lifecycle manager
			manager := &PacemakerLifecycleManager{
				operatorClient:             fakeOperatorClient,
				kubeClient:                 fakeKubeClient,
				controllerContext:          controllerContext,
				kubeInformersForNamespaces: kubeInformersForNamespaces,
				etcdInformer:               etcdInformers.Operator().V1().Etcds(),
			}

			// Store original startTnfJobcontrollersFunc and replace with mock
			originalStartFunc := startTnfJobcontrollersFunc
			startTnfJobcontrollersFunc = tt.setupMockStartFunc()
			defer func() { startTnfJobcontrollersFunc = originalStartFunc }()

			// Store original backoff config and use faster settings for testing
			originalBackoff := retryBackoffConfig
			retryBackoffConfig = wait.Backoff{
				Duration: 100 * time.Millisecond,
				Factor:   2.0,
				Steps:    5, // Much shorter for testing
				Cap:      500 * time.Millisecond,
			}
			defer func() { retryBackoffConfig = originalBackoff }()

			// Create test nodes
			nodes := []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "master-1"}},
			}

			// Run retryInitialTransitionOrDegrade
			err := manager.retryInitialTransitionOrDegrade(ctx, nodes)

			// Verify error expectation
			if tt.expectDegradedStatus == operatorv1.ConditionTrue {
				require.Error(t, err, "Expected error when retries exhausted")
			} else {
				require.NoError(t, err, "Expected no error on success")
			}

			// Verify the operator condition was set correctly
			if tt.expectDegradedCondition {
				_, status, _, err := fakeOperatorClient.GetStaticPodOperatorState()
				require.NoError(t, err, "failed to get operator status")

				// Find the TNFJobControllersDegraded condition
				var foundCondition *operatorv1.OperatorCondition
				for i, condition := range status.Conditions {
					if condition.Type == conditionTypeTNFJobControllersDegraded {
						foundCondition = &status.Conditions[i]
						break
					}
				}

				require.NotNil(t, foundCondition, "TNFJobControllersDegraded condition not found")
				require.Equal(t, tt.expectDegradedStatus, foundCondition.Status,
					"Expected degraded status %v but got %v", tt.expectDegradedStatus, foundCondition.Status)

				if tt.expectDegradedStatus == operatorv1.ConditionTrue {
					require.Equal(t, "SetupFailed", foundCondition.Reason,
						"Expected reason SetupFailed but got %s", foundCondition.Reason)
					require.Contains(t, foundCondition.Message, "Failed to setup TNF job controllers",
						"Expected message to contain failure info")
				} else {
					require.Equal(t, "AsExpected", foundCondition.Reason,
						"Expected reason AsExpected but got %s", foundCondition.Reason)
					require.Contains(t, foundCondition.Message, "successfully",
						"Expected success message")
				}
			}
		})
	}
}

// TestStartJobControllers tests the entry point logic for starting job controllers.
// This includes transition checks, node count validation, and path selection.
func TestStartJobControllers(t *testing.T) {
	tests := []struct {
		name               string
		transitionComplete bool
		nodeCount          int
		readyNodeCount     int
		expectStartCalled  bool
		expectRetryPath    bool
	}{
		{
			name:               "before transition - 2 ready nodes - bootstrap with retry",
			transitionComplete: false,
			nodeCount:          2,
			readyNodeCount:     2,
			expectStartCalled:  true,
			expectRetryPath:    true,
		},
		{
			name:               "before transition - 1 ready node - wait",
			transitionComplete: false,
			nodeCount:          2,
			readyNodeCount:     1,
			expectStartCalled:  false,
			expectRetryPath:    false,
		},
		{
			name:               "before transition - 3 nodes - skip (pacemaker only supports 2)",
			transitionComplete: false,
			nodeCount:          3,
			readyNodeCount:     3,
			expectStartCalled:  false,
			expectRetryPath:    false,
		},
		{
			name:               "after transition - 2 ready nodes - ensure running",
			transitionComplete: true,
			nodeCount:          2,
			readyNodeCount:     2,
			expectStartCalled:  true,
			expectRetryPath:    false,
		},
		{
			name:               "after transition - 1 ready node - ensure running (handles single-node)",
			transitionComplete: true,
			nodeCount:          1,
			readyNodeCount:     1,
			expectStartCalled:  true,
			expectRetryPath:    false,
		},
		{
			name:               "after transition - 0 ready nodes - skip",
			transitionComplete: true,
			nodeCount:          1,
			readyNodeCount:     0,
			expectStartCalled:  false,
			expectRetryPath:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create fake clients
			fakeKubeClient := fake.NewSimpleClientset()

			// Create nodes
			var nodes []runtime.Object
			for i := 0; i < tt.nodeCount; i++ {
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:   fmt.Sprintf("master-%d", i),
						Labels: map[string]string{"node-role.kubernetes.io/master": ""},
					},
					Status: corev1.NodeStatus{},
				}
				// Mark nodes as ready up to readyNodeCount
				if i < tt.readyNodeCount {
					node.Status.Conditions = []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					}
				} else {
					node.Status.Conditions = []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
					}
				}
				nodes = append(nodes, node)
			}

			// Add nodes to fake client
			for _, node := range nodes {
				_, err := fakeKubeClient.CoreV1().Nodes().Create(ctx, node.(*corev1.Node), metav1.CreateOptions{})
				require.NoError(t, err)
			}

			// Create operator client with transition state
			var conditions []operatorv1.OperatorCondition
			if tt.transitionComplete {
				conditions = append(conditions, operatorv1.OperatorCondition{
					Type:   ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition,
					Status: operatorv1.ConditionTrue,
				})
			}
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				&operatorv1.StaticPodOperatorStatus{
					OperatorStatus: operatorv1.OperatorStatus{
						Conditions: conditions,
					},
				},
				nil,
				nil,
			)

			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-start", &corev1.ObjectReference{}, clock.RealClock{})

			controllerContext := &controllercmd.ControllerContext{
				EventRecorder: eventRecorder,
			}

			kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
				fakeKubeClient,
				operatorclient.TargetNamespace,
			)

			// Create node informer
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, node := range nodes {
				require.NoError(t, nodeIndexer.Add(node), "failed to add node to indexer")
			}
			nodeInformer := &mockNodeInformerLM{
				indexer: nodeIndexer,
				synced:  true,
			}

			// Create etcd informer
			operatorClientFake := operatorversionedclientfake.NewClientset()
			etcdInformers := extinfops.NewSharedInformerFactory(operatorClientFake, 10*time.Minute)

			// Create lifecycle manager
			manager := &PacemakerLifecycleManager{
				operatorClient:             fakeOperatorClient,
				kubeClient:                 fakeKubeClient,
				nodeInformer:               nodeInformer,
				controllerContext:          controllerContext,
				kubeInformersForNamespaces: kubeInformersForNamespaces,
				etcdInformer:               etcdInformers.Operator().V1().Etcds(),
			}

			// Track if startTnfJobcontrollersFunc was called
			startCalled := false
			originalStartFunc := startTnfJobcontrollersFunc
			startTnfJobcontrollersFunc = func(_ context.Context, _ []*corev1.Node, _ *controllercmd.ControllerContext, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
				startCalled = true
				return nil
			}
			defer func() { startTnfJobcontrollersFunc = originalStartFunc }()

			// Use faster backoff for testing
			originalBackoff := retryBackoffConfig
			retryBackoffConfig = wait.Backoff{
				Duration: 10 * time.Millisecond,
				Factor:   2.0,
				Steps:    2,
				Cap:      50 * time.Millisecond,
			}
			defer func() { retryBackoffConfig = originalBackoff }()

			// Execute
			err := manager.StartJobControllers(ctx)

			// Verify
			require.NoError(t, err)
			require.Equal(t, tt.expectStartCalled, startCalled,
				"Expected startTnfJobcontrollersFunc called=%v, got=%v", tt.expectStartCalled, startCalled)

			// If retry path expected, verify TNFJobControllersDegraded condition was set
			if tt.expectRetryPath && tt.expectStartCalled {
				_, status, _, err := fakeOperatorClient.GetStaticPodOperatorState()
				require.NoError(t, err)

				var foundCondition *operatorv1.OperatorCondition
				for i, condition := range status.Conditions {
					if condition.Type == conditionTypeTNFJobControllersDegraded {
						foundCondition = &status.Conditions[i]
						break
					}
				}

				require.NotNil(t, foundCondition, "TNFJobControllersDegraded condition should be set on retry path")
				require.Equal(t, operatorv1.ConditionFalse, foundCondition.Status,
					"TNFJobControllersDegraded should be False on success")
			}
		})
	}
}

// retryableError is a helper type for testing retry logic
type retryableError struct {
	msg string
}

func (e *retryableError) Error() string {
	return e.msg
}
