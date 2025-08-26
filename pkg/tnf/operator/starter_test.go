package operator

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/openshift/client-go/config/informers/externalversions"
	v1 "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorversionedclientfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	extinfops "github.com/openshift/client-go/operator/informers/externalversions"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
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
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/operator/dualreplicahelpers"
)

type args struct {
	ctx                        context.Context
	controllerContext          *controllercmd.ControllerContext
	featureGateAccessor        featuregates.FeatureGateAccess
	configInformers            externalversions.SharedInformerFactory
	operatorClient             v1helpers.StaticPodOperatorClient
	envVarGetter               etcdenvvar.EnvVar
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces
	networkInformer            v1.NetworkInformer
	controlPlaneNodeInformer   cache.SharedIndexInformer
	etcdInformer               operatorv1informers.EtcdInformer
	kubeClient                 kubernetes.Interface
	dynamicClient              dynamic.Interface
	handler                    *DualReplicaClusterHandler
	handlerInitErr             error
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
			args:               getArgs(t, false, false),
			wantHandlerInitErr: false,
			wantStarted:        false,
			wantErr:            false,
		},
		{
			name:               "Dual replica topology without feature gate",
			args:               getArgs(t, true, false),
			wantHandlerInitErr: true,
			wantStarted:        false,
			wantErr:            false,
		},
		{
			name:               "Dual replica feature gate without topology",
			args:               getArgs(t, false, true),
			wantHandlerInitErr: false,
			wantStarted:        false,
			wantErr:            false,
		},
		{
			name:               "Dual replica topology with feature gate",
			args:               getArgs(t, true, true),
			wantHandlerInitErr: false,
			wantStarted:        true,
			wantErr:            false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			if tt.wantHandlerInitErr && tt.args.handlerInitErr == nil || !tt.wantHandlerInitErr && tt.args.handlerInitErr != nil {
				t.Errorf("NewDualReplicaClusterHandler handlerInitErr = %v, wantHandlerInitErr %v", tt.args.handlerInitErr, tt.wantHandlerInitErr)
			}
			if tt.wantHandlerInitErr {
				return
			}

			started, err := tt.args.handler.HandleDualReplicaClusters(
				tt.args.controllerContext,
				tt.args.configInformers,
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

func getArgs(t *testing.T, dualReplicaControlPlaneEnabled, dualReplicaFeatureGateEnabled bool) args {

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
		&operatorv1.StaticPodOperatorStatus{},
		nil,
		nil,
	)

	enabledFeatureGates := make([]configv1.FeatureGateName, 0)
	disabledFeatureGates := make([]configv1.FeatureGateName, 0)
	if dualReplicaFeatureGateEnabled {
		enabledFeatureGates = append(enabledFeatureGates, dualreplicahelpers.DualReplicaFeatureGateName)
	} else {
		disabledFeatureGates = append(disabledFeatureGates, dualreplicahelpers.DualReplicaFeatureGateName)
	}
	fga := featuregates.NewHardcodedFeatureGateAccess(enabledFeatureGates, disabledFeatureGates)

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

	// Create the DualReplicaClusterHandler
	handler, handlerErr := NewDualReplicaClusterHandler(
		context.Background(),
		fakeOperatorClient,
		fakeKubeClient,
		fga,
		configInformers,
	)

	return args{
		ctx: context.Background(),
		controllerContext: &controllercmd.ControllerContext{
			EventRecorder: eventRecorder,
		},
		featureGateAccessor:        fga,
		configInformers:            configInformers,
		operatorClient:             fakeOperatorClient,
		envVarGetter:               envVar,
		kubeInformersForNamespaces: kubeInformersForNamespaces,
		networkInformer:            networkInformer,
		controlPlaneNodeInformer:   controlPlaneNodeInformer,
		etcdInformer:               etcdInformers.Operator().V1().Etcds(),
		kubeClient:                 fakeKubeClient,
		dynamicClient:              fakeDynamicClient,
		handler:                    handler,
		handlerInitErr:             handlerErr,
	}
}
