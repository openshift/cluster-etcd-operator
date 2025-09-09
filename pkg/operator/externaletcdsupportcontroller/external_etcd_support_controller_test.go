package externaletcdsupportcontroller

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

const etcdPullSpec = "etcd-pull-spec"
const operatorPullSpec = "operator-pull-spec"

func TestExternalEtcdSupportController(t *testing.T) {
	scenarios := []struct {
		name                    string
		staticPodStatus         *operatorv1.StaticPodOperatorStatus
		expectedConfigMapExists bool
		expectedErr             error
		topology                configv1.TopologyMode
	}{
		{
			name: "Not on ExternalEtcd cluster",
			staticPodStatus: testutils.StaticPodOperatorStatus(
				testutils.WithLatestRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
			),
			expectedConfigMapExists: false,
			expectedErr:             nil,
			topology:                configv1.HighlyAvailableTopologyMode,
		},
		{
			name: "ExternalEtcd cluster but bootstrap not completed",
			staticPodStatus: testutils.StaticPodOperatorStatus(
				testutils.WithLatestRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
			),
			expectedConfigMapExists: false,
			expectedErr:             nil,
			topology:                configv1.DualReplicaTopologyMode,
		},
		{
			name: "ExternalEtcd cluster and bootstrap completed",
			staticPodStatus: testutils.StaticPodOperatorStatus(
				testutils.WithLatestRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithNodeStatusAtCurrentRevision(3),
				testutils.WithOperatorCondition("EtcdRunningInCluster", operatorv1.ConditionTrue),
			),
			expectedConfigMapExists: true,
			expectedErr:             nil,
			topology:                configv1.DualReplicaTopologyMode,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			eventRecorder, _, controller, fakeKubeClient := getController(t, scenario.staticPodStatus, scenario.topology)
			err := controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			require.Equal(t, scenario.expectedErr, err)

			if scenario.expectedErr != nil {
				return
			}

			etcdPodCM, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(context.TODO(), "external-etcd-pod", metav1.GetOptions{})
			if !scenario.expectedConfigMapExists {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			podYaml := etcdPodCM.Data["pod.yaml"]
			pod := &corev1.Pod{}
			_, _, err = scheme.Codecs.UniversalDeserializer().Decode([]byte(podYaml), nil, pod)
			require.NoError(t, err)

			require.Equal(t, 1, len(pod.Spec.Containers))
			require.Equal(t, "etcd", pod.Spec.Containers[0].Name)
			require.Equal(t, etcdPullSpec, pod.Spec.Containers[0].Image)
		})
	}
}

func getController(
	t *testing.T,
	staticPodStatus *operatorv1.StaticPodOperatorStatus, topology configv1.TopologyMode) (events.Recorder, v1helpers.StaticPodOperatorClient, *ExternalEtcdEnablerController, *fake.Clientset) {
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		staticPodStatus,
		nil,
		nil,
	)

	fakeKubeClient := fake.NewSimpleClientset()

	defaultObjects := []runtime.Object{
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		&configv1.Infrastructure{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name: ceohelpers.InfrastructureClusterName,
			},
			Status: configv1.InfrastructureStatus{
				ControlPlaneTopology: topology},
		},
	}

	eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-externaletcdsupportcontroller", &corev1.ObjectReference{}, clock.RealClock{})

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, obj := range defaultObjects {
		require.NoError(t, indexer.Add(obj))
	}

	envVar := etcdenvvar.FakeEnvVar{EnvVars: map[string]string{
		"ALL_ETCD_ENDPOINTS": "1,3",
		"OTHER_ENDPOINTS_IP": "192.168.2.42",
	}}

	etcdIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, etcdIndexer.Add(&operatorv1.Etcd{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: ceohelpers.InfrastructureClusterName,
		},
	}))

	controller := &ExternalEtcdEnablerController{
		operatorClient:        fakeOperatorClient,
		infrastructureLister:  configv1listers.NewInfrastructureLister(indexer),
		targetImagePullSpec:   etcdPullSpec,
		operatorImagePullSpec: operatorPullSpec,
		envVarGetter:          envVar,
		kubeClient:            fakeKubeClient,
		enqueueFn:             func() {},
		etcdLister:            operatorv1listers.NewEtcdLister(etcdIndexer),
	}
	return eventRecorder, fakeOperatorClient, controller, fakeKubeClient
}
