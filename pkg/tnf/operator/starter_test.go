package operator

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/openshift/client-go/config/informers/externalversions"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	v1 "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorversionedclientfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	extinfops "github.com/openshift/client-go/operator/informers/externalversions"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
)

type args struct {
	ctx                        context.Context
	controllerContext          *controllercmd.ControllerContext
	infrastructureInformer     configv1informers.InfrastructureInformer
	operatorClient             v1helpers.StaticPodOperatorClient
	envVarGetter               etcdenvvar.EnvVar
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces
	networkInformer            v1.NetworkInformer
	controlPlaneNodeInformer   cache.SharedIndexInformer
	etcdInformer               operatorv1informers.EtcdInformer
	kubeClient                 kubernetes.Interface
	dynamicClient              dynamic.Interface
	initErr                    error
}

func TestHandleDualReplicaClusters(t *testing.T) {
	tests := []struct {
		name               string
		args               args
		wantHandlerInitErr bool
		wantStarted        bool
		wantErr            bool
	}{
		{
			name:               "Normal cluster",
			args:               getArgs(t, false),
			wantHandlerInitErr: false,
			wantStarted:        false,
			wantErr:            false,
		},
		{
			name:               "DualReplica topology",
			args:               getArgs(t, true),
			wantHandlerInitErr: false,
			wantStarted:        true,
			wantErr:            false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			if tt.wantHandlerInitErr && tt.args.initErr == nil || !tt.wantHandlerInitErr && tt.args.initErr != nil {
				t.Errorf("NewDualReplicaClusterHandler handlerInitErr = %v, wantHandlerInitErr %v", tt.args.initErr, tt.wantHandlerInitErr)
			}
			if tt.wantHandlerInitErr {
				return
			}

			started, err := HandleDualReplicaClusters(
				tt.args.ctx,
				tt.args.controllerContext,
				tt.args.infrastructureInformer,
				tt.args.operatorClient,
				tt.args.envVarGetter,
				tt.args.kubeInformersForNamespaces,
				tt.args.networkInformer,
				tt.args.controlPlaneNodeInformer,
				tt.args.etcdInformer,
				tt.args.kubeClient,
				tt.args.dynamicClient)

			if started != tt.wantStarted {
				t.Errorf("HandleDualReplicaClusters() started = %v, wantStarted %v", started, tt.wantStarted)
			}

			if (err != nil) != tt.wantErr {
				t.Errorf("HandleDualReplicaClusters() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSetupJobConditionsBasedOnExternalEtcd(t *testing.T) {
	tests := []struct {
		name                     string
		isReadyForEtcdTransition bool
		expectedAvailableInSetup bool
	}{
		{
			name:                     "Job sets the Available condition when not ready for etcd transition",
			isReadyForEtcdTransition: false,
			expectedAvailableInSetup: true,
		},
		{
			name:                     "Job does not set the Available condition when ready for etcd transition",
			isReadyForEtcdTransition: true,
			expectedAvailableInSetup: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up operator status with the appropriate condition
			operatorStatus := &operatorv1.StaticPodOperatorStatus{}
			if tt.isReadyForEtcdTransition {
				operatorStatus.Conditions = []operatorv1.OperatorCondition{
					{
						Type:   ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition,
						Status: operatorv1.ConditionTrue,
					},
				}
			}

			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				operatorStatus,
				nil,
				nil,
			)

			hasExternalEtcdCompletedTransition, err := ceohelpers.HasExternalEtcdCompletedTransition(context.Background(), fakeOperatorClient)
			if err != nil {
				t.Errorf("failed to get external etcd status: %v", err)
			}

			// Determine setup conditions based on the etcd transition status
			setupConditions := jobs.DefaultConditions
			if !hasExternalEtcdCompletedTransition {
				setupConditions = jobs.AllConditions
			}

			hasAvailableCondition := false
			for _, condition := range setupConditions {
				if condition == operatorv1.OperatorStatusTypeAvailable {
					hasAvailableCondition = true
					break
				}
			}

			require.Equalf(t, tt.expectedAvailableInSetup, hasAvailableCondition,
				"Setup job should have Available condition: %v, but got: %v",
				tt.expectedAvailableInSetup, hasAvailableCondition)
		})
	}
}

func getArgs(t *testing.T, dualReplicaControlPlaneEnabled bool) args {

	fakeKubeClient := fake.NewSimpleClientset()
	fakeDynamicClient := fakedynamic.NewSimpleDynamicClient(scheme.Scheme)

	cpt := configv1.HighlyAvailableTopologyMode
	if dualReplicaControlPlaneEnabled {
		cpt = configv1.DualReplicaTopologyMode
	}
	infra := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: ceohelpers.InfrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: cpt,
		},
	}
	fakeConfigClient := fakeconfig.NewSimpleClientset([]runtime.Object{infra}...)

	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-tnfcontrollers", &corev1.ObjectReference{}, clock.RealClock{})

	envVar := etcdenvvar.FakeEnvVar{}

	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
		fakeKubeClient,
		"",
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
		"kube-system",
	)

	controlPlaneNodeInformer := corev1informers.NewFilteredNodeInformer(fakeKubeClient, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, func(listOptions *metav1.ListOptions) {
		listOptions.LabelSelector = "node-role.kubernetes.io/master"
	})

	etcdIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, etcdIndexer.Add(&operatorv1.Etcd{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: ceohelpers.InfrastructureClusterName,
		},
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	operatorClientFake := operatorversionedclientfake.NewClientset()
	etcdInformers := extinfops.NewSharedInformerFactory(operatorClientFake, 10*time.Minute)
	etcdInformers.Operator().V1().Etcds().Informer().AddIndexers(etcdIndexer.GetIndexers())
	etcdInformers.Start(ctx.Done())

	configInformers := externalversions.NewSharedInformerFactory(fakeConfigClient, 10*time.Minute)
	configInformers.Config().V1().Infrastructures().Informer().AddIndexers(cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	networkInformer := configInformers.Config().V1().Networks()
	configInformers.Start(ctx.Done())
	synced := configInformers.WaitForCacheSync(ctx.Done())
	maps.Copy(synced, etcdInformers.WaitForCacheSync(ctx.Done()))

	for v, ok := range synced {
		if !ok {
			t.Errorf("caches failed to sync: %v", v)
		}
	}

	return args{
		initErr: nil, // Default to no error
		ctx:     context.Background(),
		controllerContext: &controllercmd.ControllerContext{
			EventRecorder: eventRecorder,
		},
		infrastructureInformer:     configInformers.Config().V1().Infrastructures(),
		operatorClient:             fakeOperatorClient,
		envVarGetter:               envVar,
		kubeInformersForNamespaces: kubeInformersForNamespaces,
		networkInformer:            networkInformer,
		controlPlaneNodeInformer:   controlPlaneNodeInformer,
		etcdInformer:               etcdInformers.Operator().V1().Etcds(),
		kubeClient:                 fakeKubeClient,
		dynamicClient:              fakeDynamicClient,
	}
}

func TestHandleNodesWithRetry(t *testing.T) {
	tests := []struct {
		name                    string
		setupMockHandleNodes    func() func(*controllercmd.ControllerContext, corev1listers.NodeLister, context.Context, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error
		expectDegradedCondition bool
		expectDegradedStatus    operatorv1.ConditionStatus
		expectRetries           bool
	}{
		{
			name: "Success on first attempt",
			setupMockHandleNodes: func() func(*controllercmd.ControllerContext, corev1listers.NodeLister, context.Context, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error {
				return func(_ *controllercmd.ControllerContext, _ corev1listers.NodeLister, _ context.Context, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
					return nil
				}
			},
			expectDegradedCondition: true,
			expectDegradedStatus:    operatorv1.ConditionFalse,
			expectRetries:           false,
		},
		{
			name: "Success after retries",
			setupMockHandleNodes: func() func(*controllercmd.ControllerContext, corev1listers.NodeLister, context.Context, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error {
				attemptCount := 0
				return func(_ *controllercmd.ControllerContext, _ corev1listers.NodeLister, _ context.Context, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
					attemptCount++
					if attemptCount < 3 {
						return errors.New("temporary failure")
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
			setupMockHandleNodes: func() func(*controllercmd.ControllerContext, corev1listers.NodeLister, context.Context, v1helpers.StaticPodOperatorClient, kubernetes.Interface, v1helpers.KubeInformersForNamespaces, operatorv1informers.EtcdInformer) error {
				return func(_ *controllercmd.ControllerContext, _ corev1listers.NodeLister, _ context.Context, _ v1helpers.StaticPodOperatorClient, _ kubernetes.Interface, _ v1helpers.KubeInformersForNamespaces, _ operatorv1informers.EtcdInformer) error {
					return errors.New("persistent failure")
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
			testArgs := getArgs(t, true)

			// Create NodeLister from the informer
			controlPlaneNodeLister := corev1listers.NewNodeLister(testArgs.controlPlaneNodeInformer.GetIndexer())

			// Store original handleNodesFunc and replace with mock
			originalHandleNodesFunc := handleNodesFunc
			handleNodesFunc = tt.setupMockHandleNodes()
			defer func() { handleNodesFunc = originalHandleNodesFunc }()

			// Store original backoff config and use faster settings for testing
			originalBackoff := retryBackoffConfig
			retryBackoffConfig = wait.Backoff{
				Duration: 100 * time.Millisecond,
				Factor:   2.0,
				Steps:    5, // Much shorter for testing
				Cap:      500 * time.Millisecond,
			}
			defer func() { retryBackoffConfig = originalBackoff }()

			// For faster testing, use a reasonable timeout
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Run handleNodesWithRetry
			handleNodesWithRetry(testArgs.controllerContext, controlPlaneNodeLister, ctx, testArgs.operatorClient,
				testArgs.kubeClient, testArgs.kubeInformersForNamespaces, testArgs.etcdInformer)

			// Verify the operator condition was set correctly
			if tt.expectDegradedCondition {
				_, status, _, err := testArgs.operatorClient.GetStaticPodOperatorState()
				require.NoError(t, err, "failed to get operator status")

				// Find the TNFJobControllersDegraded condition
				var foundCondition *operatorv1.OperatorCondition
				for i, condition := range status.Conditions {
					if condition.Type == "TNFJobControllersDegraded" {
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
