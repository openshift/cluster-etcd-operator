package operator

import (
	"context"
	"errors"
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
		name                   string
		nodes                  []*corev1.Node
		existingJobs           []runtime.Object
		mockStartControllers   func() error
		mockUpdateSetup        func() error
		expectError            bool
		expectStartControllers bool
		expectUpdateSetup      bool
		errorContains          string
	}{
		{
			name: "Less than 2 nodes - returns nil without action",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
			},
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name: "More than 2 nodes - returns nil without action",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createReadyNode("master-1"),
				createReadyNode("master-2"),
			},
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name: "2 nodes but first not ready - returns nil without action",
			nodes: []*corev1.Node{
				createNotReadyNode("master-0"),
				createReadyNode("master-1"),
			},
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name: "2 nodes but second not ready - returns nil without action",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createNotReadyNode("master-1"),
			},
			existingJobs:           []runtime.Object{},
			expectError:            false,
			expectStartControllers: false,
			expectUpdateSetup:      false,
		},
		{
			name: "2 ready nodes, no existing jobs - starts controllers only",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createReadyNode("master-1"),
			},
			existingJobs: []runtime.Object{},
			mockStartControllers: func() error {
				return nil
			},
			expectError:            false,
			expectStartControllers: true,
			expectUpdateSetup:      false,
		},
		{
			name: "2 ready nodes, existing jobs - starts controllers and updates setup",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createReadyNode("master-1"),
			},
			existingJobs: []runtime.Object{
				createTNFJob("tnf-setup"),
			},
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
			name: "2 ready nodes - error starting controllers",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createReadyNode("master-1"),
			},
			existingJobs: []runtime.Object{},
			mockStartControllers: func() error {
				return errors.New("failed to start controllers")
			},
			expectError:            true,
			expectStartControllers: true,
			expectUpdateSetup:      false,
			errorContains:          "failed to start TNF job controllers",
		},
		{
			name: "2 ready nodes, existing jobs - error updating setup",
			nodes: []*corev1.Node{
				createReadyNode("master-0"),
				createReadyNode("master-1"),
			},
			existingJobs: []runtime.Object{
				createTNFJob("tnf-setup"),
			},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			ctx := context.Background()

			// Create fake kubernetes client with jobs
			fakeKubeClient := fake.NewSimpleClientset(tt.existingJobs...)

			// Create fake operator client
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				u.StaticPodOperatorStatus(
					u.WithLatestRevision(1),
					u.WithNodeStatusAtCurrentRevision(1),
					u.WithNodeStatusAtCurrentRevision(1),
				),
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
			ctx2, cancel := context.WithCancel(context.Background())
			defer cancel()
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

func createTNFJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operatorclient.TargetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/component": "two-node-fencing-setup",
			},
		},
	}
}
