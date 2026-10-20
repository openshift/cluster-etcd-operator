package targetconfigcontroller

import (
	"context"
	"strings"
	"testing"

	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"k8s.io/client-go/kubernetes/scheme"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	"github.com/openshift/cluster-etcd-operator/bindata"
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

func TestTargetConfigControllerArbiterRoleRevisionContent(test *testing.T) {
	ctx := context.Background()
	recorder, _, controller, client := getController(test, u.StaticPodOperatorStatus(
		u.WithLatestRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
	), configv1.HighlyAvailableArbiterMode)
	scripts := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-scripts", Namespace: operatorclient.TargetNamespace},
		Data:       map[string]string{},
	}
	for _, name := range []string{"cluster-backup.sh", "cluster-restore.sh", "quorum-restore.sh", "disable-etcd.sh", "etcd-common-tools"} {
		scripts.Data[name] = string(bindata.MustAsset("etcd/" + name))
	}
	_, err := client.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Create(ctx, scripts, metav1.CreateOptions{})
	require.NoError(test, err)

	var previousContent map[string]string
	for _, scenario := range []struct {
		name      string
		roles     map[string]string
		unchanged bool
	}{
		{name: "before role environment"},
		{name: "mixed control plane and arbiter", roles: map[string]string{
			"NODE_master_0_IS_ARBITER":  "false",
			"NODE_master_1_IS_ARBITER":  "false",
			"NODE_arbiter_0_IS_ARBITER": "true",
		}},
		{name: "same roles are stable", unchanged: true, roles: map[string]string{
			"NODE_arbiter_0_IS_ARBITER": "true",
			"NODE_master_1_IS_ARBITER":  "false",
			"NODE_master_0_IS_ARBITER":  "false",
		}},
		{name: "changed node roles", roles: map[string]string{
			"NODE_master_0_IS_ARBITER":  "false",
			"NODE_master_1_IS_ARBITER":  "true",
			"NODE_arbiter_0_IS_ARBITER": "false",
		}},
		{name: "changed roles remain stable", unchanged: true, roles: map[string]string{
			"NODE_arbiter_0_IS_ARBITER": "false",
			"NODE_master_1_IS_ARBITER":  "true",
			"NODE_master_0_IS_ARBITER":  "false",
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			envVars := map[string]string{"ALL_ETCD_ENDPOINTS": "1,3", "OTHER_ENDPOINTS_IP": "192.168.2.42"}
			for name, value := range scenario.roles {
				envVars[name] = value
			}
			controller.envVarGetter = etcdenvvar.FakeEnvVar{EnvVars: envVars}
			require.NoError(test, controller.sync(ctx, factory.NewSyncContext("test", recorder)))
			podConfigMap, err := client.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(ctx, "etcd-pod", metav1.GetOptions{})
			require.NoError(test, err)
			if previousContent != nil {
				if scenario.unchanged {
					require.Equal(test, previousContent, podConfigMap.Data)
				} else {
					require.NotEqual(test, previousContent["pod.yaml"], podConfigMap.Data["pod.yaml"])
				}
			}
			previousContent = podConfigMap.Data

			pod := &corev1.Pod{}
			_, _, err = scheme.Codecs.UniversalDeserializer().Decode([]byte(podConfigMap.Data["pod.yaml"]), nil, pod)
			require.NoError(test, err)
			var copyInit *corev1.Container
			for index := range pod.Spec.InitContainers {
				if pod.Spec.InitContainers[index].Name == "etcd-resources-copy" {
					copyInit = &pod.Spec.InitContainers[index]
				}
			}
			require.NotNil(test, copyInit)
			require.Len(test, copyInit.Env, len(envVars))
			for name, value := range envVars {
				require.Contains(test, copyInit.Env, corev1.EnvVar{Name: name, Value: value})
			}
			require.Len(test, copyInit.Command, 3)
			require.Equal(test, []string{"/bin/sh", "-c"}, copyInit.Command[:2])
			command := copyInit.Command[2]
			require.Contains(test, command, `node_is_arbiter="${NODE_NODE_ENVVAR_NAME_IS_ARBITER:-}"`)
			require.Contains(test, command, `case "${node_is_arbiter}" in`)
			require.Contains(test, command, `true|false) ;;`)
			require.Contains(test, command, `*) echo "Invalid or missing arbiter role for this node" >&2; exit 1 ;;`)
			require.Less(test, strings.Index(command, "case"), strings.Index(command, "rm -f"))
			require.Contains(test, command, "if [ \"${node_is_arbiter}\" = \"true\" ]; then\n  rm -f /usr/local/bin/cluster-backup.sh")
			require.Contains(test, command, "if [ \"${node_is_arbiter}\" = \"true\" ] && [ \"${script##*/}\" = \"cluster-backup.sh\" ]; then\n    continue")
			require.Contains(test, command, `cp -p "${script}" /usr/local/bin`)

			actualScripts, err := client.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(ctx, scripts.Name, metav1.GetOptions{})
			require.NoError(test, err)
			require.Equal(test, scripts, actualScripts)
		})
	}
}

func TestTargetConfigController(t *testing.T) {

	scenarios := []struct {
		name                         string
		staticPodStatus              *operatorv1.StaticPodOperatorStatus
		etcdMembers                  []*etcdserverpb.Member
		expectedEtcdContainerRemoved bool
		expectedErr                  error
		topology                     configv1.TopologyMode
	}{
		{
			name: "HappyPath",
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
			topology: configv1.HighlyAvailableTopologyMode,
		},
		{
			name: "Quorum not fault tolerant but bootstrapping",
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
			topology: configv1.HighlyAvailableTopologyMode,
		},
		{
			name: "BackupVar Test HappyPath",
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
			topology: configv1.HighlyAvailableTopologyMode,
		},
		{
			name: "Backup Var Test with empty spec",
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
			topology: configv1.HighlyAvailableTopologyMode,
		},
		{
			name: "ExternalEtcd Cluster - Enabled",
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			topology: configv1.DualReplicaTopologyMode,
		},
		{
			name: "ExternalEtcd Cluster - Bootstrap Completed but Not Ready",
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithOperatorCondition(ceohelpers.OperatorConditionEtcdRunningInCluster, operatorv1.ConditionTrue),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			topology: configv1.DualReplicaTopologyMode,
		},
		{
			name: "ExternalEtcd Cluster - Ready for Etcd Transition",
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithOperatorCondition(ceohelpers.OperatorConditionEtcdRunningInCluster, operatorv1.ConditionTrue),
				u.WithOperatorCondition(ceohelpers.OperatorConditionExternalEtcdReadyForTransition, operatorv1.ConditionTrue),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			expectedEtcdContainerRemoved: true,
			topology:                     configv1.DualReplicaTopologyMode,
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

			etcdPodCM, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(context.TODO(), "etcd-pod", metav1.GetOptions{})
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
				"etcdctl":      envWithRevision,
				"etcd":         envWithRevision,
				"etcd-metrics": envWithRevision,
				"etcd-readyz":  envWithoutRevision,
				"etcd-rev":     envWithoutRevision,
			}

			expectedImage := map[string]string{
				"etcdctl":      etcdPullSpec,
				"etcd":         etcdPullSpec,
				"etcd-metrics": etcdPullSpec,
				"etcd-readyz":  operatorPullSpec,
				"etcd-rev":     operatorPullSpec,
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
	topology configv1.TopologyMode) (events.Recorder, v1helpers.StaticPodOperatorClient, *TargetConfigController, *fake.Clientset) {
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
	}

	eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-targetconfigcontroller", &corev1.ObjectReference{}, clock.RealClock{})
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

	controller := &TargetConfigController{
		targetImagePullSpec:   etcdPullSpec,
		operatorImagePullSpec: operatorPullSpec,
		operatorClient:        fakeOperatorClient,
		infrastructureLister:  u.FakeInfrastructureLister(t, topology),
		kubeClient:            fakeKubeClient,
		envVarGetter:          envVar,
		enqueueFn:             func() {},
		etcdLister:            operatorv1listers.NewEtcdLister(etcdIndexer),
	}

	return eventRecorder, fakeOperatorClient, controller, fakeKubeClient
}
