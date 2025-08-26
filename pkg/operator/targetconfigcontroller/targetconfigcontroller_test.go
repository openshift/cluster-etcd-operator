package targetconfigcontroller

import (
	"context"
	"testing"

	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"k8s.io/client-go/kubernetes/scheme"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/stretchr/testify/require"
)

const etcdPullSpec = "etcd-pull-spec"
const operatorPullSpec = "operator-pull-spec"

// mockClusterStatus implements status.ClusterStatus for testing
type mockClusterStatus struct {
	isDualReplicaTopology bool
	isBootstrapCompleted  bool
	isReadyForEtcdRemoval bool
}

func (m *mockClusterStatus) IsDualReplicaTopology() bool {
	return m.isDualReplicaTopology
}

func (m *mockClusterStatus) IsBootstrapCompleted() bool {
	return m.isBootstrapCompleted
}

func (m *mockClusterStatus) IsReadyForEtcdRemoval() bool {
	return m.isReadyForEtcdRemoval
}

func (m *mockClusterStatus) SetBootstrapCompleted() {
	m.isBootstrapCompleted = true
}

func TestTargetConfigController(t *testing.T) {

	scenarios := []struct {
		name                         string
		objects                      []runtime.Object
		staticPodStatus              *operatorv1.StaticPodOperatorStatus
		etcdMembers                  []*etcdserverpb.Member
		dualReplicaStatus            *mockClusterStatus
		expectedSyncSkipped          bool
		expectedEtcdContainerRemoved bool
		expectedErr                  error
	}{
		{
			name: "HappyPath",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			dualReplicaStatus: &mockClusterStatus{},
		},
		{
			name: "Quorum not fault tolerant but bootstrapping",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("not complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(2),
			},
			dualReplicaStatus: &mockClusterStatus{},
		},
		{
			name: "BackupVar Test HappyPath",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			dualReplicaStatus: &mockClusterStatus{},
		},
		{
			name: "Backup Var Test with empty spec",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			dualReplicaStatus: &mockClusterStatus{},
		},
		{
			name: "Dual Replica Cluster - Topology Enabled",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			dualReplicaStatus: &mockClusterStatus{
				isDualReplicaTopology: true,
			},
		},
		{
			name: "Dual Replica Cluster - Bootstrap Completed but Not Ready",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			dualReplicaStatus: &mockClusterStatus{
				isDualReplicaTopology: true,
				isBootstrapCompleted:  true,
			},
			expectedSyncSkipped: true,
		},
		{
			name: "Dual Replica Cluster - Ready for Etcd Removal",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			dualReplicaStatus: &mockClusterStatus{
				isDualReplicaTopology: true,
				isBootstrapCompleted:  true,
				isReadyForEtcdRemoval: true,
			},
			expectedEtcdContainerRemoved: true,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			eventRecorder, _, controller, fakeKubeClient := getController(t, scenario.staticPodStatus, scenario.objects, scenario.dualReplicaStatus)
			err := controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			require.Equal(t, scenario.expectedErr, err)

			if scenario.expectedErr != nil {
				return
			}

			etcdPodCM, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(context.TODO(), "etcd-pod", metav1.GetOptions{})
			if scenario.expectedSyncSkipped {
				require.ErrorContains(t, err, "not found")
				return
			}
			require.NoError(t, err)

			podYaml := etcdPodCM.Data["pod.yaml"]
			pod := &corev1.Pod{}
			_, _, err = scheme.Codecs.UniversalDeserializer().Decode([]byte(podYaml), nil, pod)
			require.NoError(t, err)

			envWithoutRevision := []corev1.EnvVar{
				{Name: "ALL_ETCD_ENDPOINTS", Value: "1,3"},
				{Name: "OTHER_ENDPOINTS_IP", Value: "192.168.2.42"},
			}

			envWithRevision := append(envWithoutRevision, corev1.EnvVar{
				Name:  "ETCD_STATIC_POD_VERSION",
				Value: "REVISION",
			})

			expectedEnv := map[string][]corev1.EnvVar{
				"etcdctl":            envWithRevision,
				"etcd":               envWithRevision,
				"etcd-metrics":       envWithRevision,
				"etcd-readyz":        envWithoutRevision,
				"etcd-rev":           envWithoutRevision,
				"etcd-backup-server": envWithoutRevision,
			}

			expectedImage := map[string]string{
				"etcdctl":            etcdPullSpec,
				"etcd":               etcdPullSpec,
				"etcd-metrics":       etcdPullSpec,
				"etcd-readyz":        operatorPullSpec,
				"etcd-rev":           operatorPullSpec,
				"etcd-backup-server": operatorPullSpec,
			}

			etcdContainerFound := false

			for _, container := range pod.Spec.Containers {
				require.Contains(t, expectedEnv, container.Name)
				require.Equal(t, expectedEnv[container.Name], container.Env)
				require.Contains(t, expectedImage, container.Name)
				require.Equal(t, expectedImage[container.Name], container.Image)

				if container.Name == "etcd" {
					etcdContainerFound = true
				}
			}

			require.Equal(t, scenario.expectedEtcdContainerRemoved, !etcdContainerFound)
		})
	}
}

func getController(
	t *testing.T,
	staticPodStatus *operatorv1.StaticPodOperatorStatus,
	objects []runtime.Object,
	dualReplicaStatus *mockClusterStatus) (events.Recorder, v1helpers.StaticPodOperatorClient, *TargetConfigController, *fake.Clientset) {
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

	fakeKubeClient := fake.NewSimpleClientset(objects...)

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
				ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
		},
	}

	eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-targetconfigcontroller", &corev1.ObjectReference{}, clock.RealClock{})
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, obj := range defaultObjects {
		require.NoError(t, indexer.Add(obj))
	}
	for _, obj := range objects {
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

	controller := &TargetConfigController{
		targetImagePullSpec:      etcdPullSpec,
		operatorImagePullSpec:    operatorPullSpec,
		operatorClient:           fakeOperatorClient,
		dualReplicaClusterStatus: dualReplicaStatus,
		kubeClient:               fakeKubeClient,
		envVarGetter:             envVar,
		enqueueFn:                func() {},
		etcdLister:               operatorv1listers.NewEtcdLister(etcdIndexer),
	}

	return eventRecorder, fakeOperatorClient, controller, fakeKubeClient
}
