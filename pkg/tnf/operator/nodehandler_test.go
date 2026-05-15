package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclientfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	extinfops "github.com/openshift/client-go/operator/informers/externalversions"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

func TestHandleNodes(t *testing.T) {
	tests := []struct {
		name                           string
		nodes                          []*corev1.Node
		triggeringNode                 *corev1.Node
		eventType                      string
		existingJobs                   []runtime.Object
		externalEtcdTransitionComplete bool
		mockStartControllers           func() error
		mockUpdateSetup                func() error
		expectError                    bool
		expectStartControllers         bool
		expectUpdateSetup              bool
		errorContains                  string
	}{
		{
			name:                   "ready event: 1 node, no jobs - wait for 2 nodes (initial setup requires 2 nodes)",
			nodes:                  []*corev1.Node{createReadyNode("master-0")},
			triggeringNode:         createReadyNode("master-0"),
			eventType:              NodeEventTypeReady,
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name:                           "delete event: 1 node remaining, transition complete - run reconciliation",
			nodes:                          []*corev1.Node{createReadyNode("master-0")},
			triggeringNode:                 createReadyNode("master-1"), // deleted node
			eventType:                      NodeEventTypeDelete,
			existingJobs:                   []runtime.Object{createTNFJob("tnf-after-setup-master-0")},
			externalEtcdTransitionComplete: true,
			mockStartControllers: func() error {
				return nil
			},
			mockUpdateSetup: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      true,
		},
		{
			name:                           "delete event: 1 node, after-setup job in progress, transition complete - run controllers and update-setup",
			nodes:                          []*corev1.Node{createReadyNode("master-0")},
			triggeringNode:                 createReadyNode("master-1"),
			eventType:                      NodeEventTypeDelete,
			existingJobs:                   []runtime.Object{createTNFJobWithStatus("tnf-after-setup-master-0", 0)},
			externalEtcdTransitionComplete: true,
			mockStartControllers: func() error {
				return nil
			},
			mockUpdateSetup: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      true,
		},
		{
			name: "delete event: deleted node NotReady in list, survivor Ready - readiness uses survivors only",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createNotReadyNode("master-1"),
			},
			triggeringNode:                 createNotReadyNode("master-1"),
			eventType:                      NodeEventTypeDelete,
			existingJobs:                   []runtime.Object{createTNFJob("tnf-after-setup-master-0")},
			externalEtcdTransitionComplete: true,
			mockStartControllers: func() error {
				return nil
			},
			mockUpdateSetup: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      true,
		},
		{
			name:                           "delete event: survivor not Ready - error so retries can run update-setup later",
			nodes:                          []*corev1.Node{createNotReadyNode("master-0")},
			triggeringNode:                 createReadyNode("master-1"),
			eventType:                      NodeEventTypeDelete,
			existingJobs:                   []runtime.Object{},
			externalEtcdTransitionComplete: true,
			expectError:                    true,
			expectStartControllers:         false,
			expectUpdateSetup:              false,
			errorContains:                  "waiting for remaining control-plane nodes to be Ready",
		},
		{
			name: "add event: >3 nodes - returns nil without action",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createReadyNode("master-1"),
				createReadyNode("master-2"),
				createReadyNode("master-3"),
			},
			triggeringNode:         createReadyNode("master-3"),
			eventType:              NodeEventTypeAdd,
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name:                   "ready event: 2 nodes but first not ready - returns nil without action",
			nodes:                  []*corev1.Node{createNotReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode:         createReadyNode("master-1"),
			eventType:              NodeEventTypeReady,
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name:                   "ready event: 2 nodes but second not ready - returns nil without action",
			nodes:                  []*corev1.Node{createReadyNode("master-0"), createNotReadyNode("master-1")},
			triggeringNode:         createReadyNode("master-0"),
			eventType:              NodeEventTypeReady,
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name:           "ready event: 2 ready nodes, no existing jobs - starts controllers only (initial setup)",
			nodes:          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode: createReadyNode("master-1"),
			eventType:      NodeEventTypeReady,
			existingJobs:   []runtime.Object{},
			mockStartControllers: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      false,
		},
		{
			name:                           "ready event: 2 ready nodes, ExternalEtcdHasCompletedTransition true but no jobs - still starts initial bootstrap",
			nodes:                          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode:                 createReadyNode("master-1"),
			eventType:                      NodeEventTypeReady,
			existingJobs:                   []runtime.Object{},
			externalEtcdTransitionComplete: true,
			mockStartControllers: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      false,
		},
		{
			name:           "ready event: 2 ready nodes, TNF jobs already exist - still starts controllers (idempotent)",
			nodes:          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode: createReadyNode("master-1"),
			eventType:      NodeEventTypeReady,
			existingJobs:   []runtime.Object{createTNFJob("tnf-auth-job-master-0")},
			mockStartControllers: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      false,
		},
		{
			name:                           "add event: 2 ready nodes, transition complete - start controllers and update-setup",
			nodes:                          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode:                 createReadyNode("master-1"), // newly added node
			eventType:                      NodeEventTypeAdd,
			existingJobs:                   []runtime.Object{},
			externalEtcdTransitionComplete: true,
			mockStartControllers: func() error {
				return nil
			},
			mockUpdateSetup: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      true,
		},
		{
			name:           "ready event: 2 ready nodes - error starting controllers",
			nodes:          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode: createReadyNode("master-1"),
			eventType:      NodeEventTypeReady,
			existingJobs:   []runtime.Object{},
			mockStartControllers: func() error {
				return errors.New("failed to start controllers")
			},
			expectError:            true,
			expectStartControllers: true,
			expectUpdateSetup:      false,
			errorContains:          "failed to start TNF job controllers",
		},
		{
			name:                           "add event: 2 ready nodes, transition complete - error updating setup",
			nodes:                          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode:                 createReadyNode("master-1"),
			eventType:                      NodeEventTypeAdd,
			existingJobs:                   []runtime.Object{},
			externalEtcdTransitionComplete: true,
			mockStartControllers: func() error {
				return nil
			},
			mockUpdateSetup: func() error {
				return errors.New("failed to update")
			},
			expectError:            true,
			expectStartControllers: true,
			expectUpdateSetup:      true,
			errorContains:          "failed to update pacemaker setup",
		},
		{
			name:           "add event: 2 ready nodes, TNF jobs exist but transition not complete - no update-setup (regression)",
			nodes:          []*corev1.Node{createReadyNode("master-0"), createReadyNode("master-1")},
			triggeringNode: createReadyNode("master-1"),
			eventType:      NodeEventTypeAdd,
			existingJobs:   []runtime.Object{createTNFJob("tnf-auth-job-master-0")},
			mockStartControllers: func() error {
				return nil
			},
			mockUpdateSetup: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			ctx := context.Background()

			// Create fake kubernetes client with jobs
			fakeKubeClient := fake.NewSimpleClientset(tt.existingJobs...)

			// Create fake operator client
			statusOpts := []func(*operatorv1.StaticPodOperatorStatus){
				u.WithLatestRevision(1),
				u.WithNodeStatusAtCurrentRevision(1),
				u.WithNodeStatusAtCurrentRevision(1),
			}
			if tt.externalEtcdTransitionComplete {
				statusOpts = append(statusOpts, u.WithOperatorCondition(ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition, operatorv1.ConditionTrue))
			}
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				u.StaticPodOperatorStatus(statusOpts...),
				nil,
				nil,
			)

			// Create controller context
			eventRecorder := events.NewRecorder(
				fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-nodehandler",
				&corev1.ObjectReference{},
				clock.RealClock{},
			)
			controllerContext := &controllercmd.ControllerContext{
				EventRecorder: eventRecorder,
			}

			// Create node informer and lister
			nodeInformer := informers.NewSharedInformerFactory(fakeKubeClient, 0).Core().V1().Nodes()
			for _, node := range tt.nodes {
				err := nodeInformer.Informer().GetIndexer().Add(node)
				require.NoError(t, err)
			}
			controlPlaneNodeLister := corev1listers.NewNodeLister(nodeInformer.Informer().GetIndexer())

			// Create etcd informer
			operatorClientFake := operatorversionedclientfake.NewClientset()
			etcdInformers := extinfops.NewSharedInformerFactory(operatorClientFake, 10*time.Minute)
			etcdIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, etcdIndexer.Add(&operatorv1.Etcd{
				ObjectMeta: metav1.ObjectMeta{
					Name: ceohelpers.InfrastructureClusterName,
				},
			}))
			etcdInformers.Operator().V1().Etcds().Informer().AddIndexers(etcdIndexer.GetIndexers())
			ctx2 := t.Context()
			etcdInformers.Start(ctx2.Done())
			synced := etcdInformers.WaitForCacheSync(ctx2.Done())
			for v, ok := range synced {
				require.True(t, ok, "cache failed to sync: %v", v)
			}

			// Create kubeInformersForNamespaces
			kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
				fakeKubeClient,
				"",
				operatorclient.GlobalUserSpecifiedConfigNamespace,
				operatorclient.GlobalMachineSpecifiedConfigNamespace,
				operatorclient.TargetNamespace,
				operatorclient.OperatorNamespace,
				"kube-system",
			)

			// Track function calls
			startControllersCalled := false
			updateSetupCalled := false

			// Mock startTnfJobcontrollers
			originalStartFunc := startTnfJobcontrollersFunc
			if tt.mockStartControllers != nil {
				startTnfJobcontrollersFunc = func(
					nodeList []*corev1.Node,
					ctx context.Context,
					controllerContext *controllercmd.ControllerContext,
					operatorClient v1helpers.StaticPodOperatorClient,
					kubeClient kubernetes.Interface,
					kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
					etcdInformer operatorv1informers.EtcdInformer,
				) error {
					startControllersCalled = true
					return tt.mockStartControllers()
				}
			}
			defer func() { startTnfJobcontrollersFunc = originalStartFunc }()

			// Mock updateSetup
			originalUpdateFunc := updateSetupFunc
			if tt.mockUpdateSetup != nil {
				updateSetupFunc = func(
					nodeList []*corev1.Node,
					triggeringNode *corev1.Node,
					eventType string,
					ctx context.Context,
					controllerContext *controllercmd.ControllerContext,
					operatorClient v1helpers.StaticPodOperatorClient,
					kubeClient kubernetes.Interface,
					kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
				) error {
					updateSetupCalled = true
					return tt.mockUpdateSetup()
				}
			}
			defer func() { updateSetupFunc = originalUpdateFunc }()

			// Execute
			err := handleNodes(
				controllerContext,
				controlPlaneNodeLister,
				ctx,
				fakeOperatorClient,
				fakeKubeClient,
				kubeInformersForNamespaces,
				etcdInformers.Operator().V1().Etcds(),
				tt.triggeringNode,
				tt.eventType,
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

			require.Equal(t, tt.expectStartControllers, startControllersCalled,
				"Expected startTnfJobcontrollers called=%v, but got=%v",
				tt.expectStartControllers, startControllersCalled)

			require.Equal(t, tt.expectUpdateSetup, updateSetupCalled,
				"Expected updateSetup called=%v, but got=%v",
				tt.expectUpdateSetup, updateSetupCalled)
		})
	}
}

// Helper functions

func createReadyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func createNotReadyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}
}

func TestControlPlaneNodesExceptName(t *testing.T) {
	m0 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}}
	m1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-1"}}
	nilNode := (*corev1.Node)(nil)

	require.Empty(t, controlPlaneNodesExceptName(nil, "master-0"))
	require.Empty(t, controlPlaneNodesExceptName([]*corev1.Node{nilNode, nilNode}, "master-0"))

	out := controlPlaneNodesExceptName([]*corev1.Node{m0, m1}, "master-0")
	require.Len(t, out, 1)
	require.Equal(t, "master-1", out[0].Name)

	out = controlPlaneNodesExceptName([]*corev1.Node{m0, m1}, "unknown")
	require.Len(t, out, 2)
}

func createTNFJob(name string) *batchv1.Job {
	return createTNFJobWithStatus(name, 1)
}

func createTNFJobWithStatus(name string, succeeded int32) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/component": "two-node-fencing-setup",
	}
	// Align app.kubernetes.io/name with production Job manifests (see jobs.RunTNFJobController hooks).
	switch {
	case strings.HasPrefix(name, "tnf-after-setup"):
		labels["app.kubernetes.io/name"] = "tnf-after-setup"
	case strings.HasPrefix(name, "tnf-fencing-job"):
		labels["app.kubernetes.io/name"] = "tnf-fencing-job"
	case strings.HasPrefix(name, "tnf-auth-job"):
		labels["app.kubernetes.io/name"] = "tnf-auth-job"
	case strings.HasPrefix(name, "tnf-setup-job"):
		labels["app.kubernetes.io/name"] = "tnf-setup-job"
	case strings.HasPrefix(name, "tnf-update-setup-job"):
		labels["app.kubernetes.io/name"] = "tnf-update-setup-job"
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operatorclient.TargetNamespace,
			Labels:    labels,
		},
		Status: batchv1.JobStatus{
			Succeeded: succeeded,
		},
	}
}
