package etcdenvvar

import (
	"context"
	"fmt"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backendquotahelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/stretchr/testify/require"

	"gopkg.in/yaml.v3"
)

var (
	backendQuotaFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{backendquotahelpers.BackendQuotaFeatureGateName},
		[]configv1.FeatureGateName{})

	defaultEnvResult = map[string]string{
		"ALL_ETCD_ENDPOINTS":                       "https://192.168.2.0:2379,https://192.168.2.1:2379,https://192.168.2.2:2379",
		"ETCDCTL_API":                              "3",
		"ETCDCTL_CACERT":                           "/etc/kubernetes/static-pod-certs/configmaps/etcd-all-bundles/server-ca-bundle.crt",
		"ETCDCTL_CERT":                             "/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt",
		"ETCDCTL_ENDPOINTS":                        "https://192.168.2.0:2379,https://192.168.2.1:2379,https://192.168.2.2:2379",
		"ETCDCTL_KEY":                              "/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key",
		"ETCD_CIPHER_SUITES":                       "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"ETCD_DATA_DIR":                            "/var/lib/etcd",
		"ETCD_ELECTION_TIMEOUT":                    "1000",
		"ETCD_ENABLE_PPROF":                        "true",
		"ETCD_EXPERIMENTAL_MAX_LEARNERS":           "1",
		"ETCD_EXPERIMENTAL_WARNING_APPLY_DURATION": "200ms",
		"ETCD_EXPERIMENTAL_WATCH_PROGRESS_NOTIFY_INTERVAL": "5s",
		"ETCD_HEARTBEAT_INTERVAL":                          "100",
		"ETCD_IMAGE":                                       "",
		"ETCD_INITIAL_CLUSTER_STATE":                       "existing",
		"ETCD_QUOTA_BACKEND_BYTES":                         "8589934592",
		"ETCD_SOCKET_REUSE_ADDRESS":                        "true",
		"ETCD_TLS_MIN_VERSION":                             "TLS1.2",
		"NODE_master_0_ETCD_NAME":                          "master-0",
		"NODE_master_0_ETCD_URL_HOST":                      "192.168.2.0",
		"NODE_master_0_IP":                                 "192.168.2.0",
		"NODE_master_1_ETCD_NAME":                          "master-1",
		"NODE_master_1_ETCD_URL_HOST":                      "192.168.2.1",
		"NODE_master_1_IP":                                 "192.168.2.1",
		"NODE_master_2_ETCD_NAME":                          "master-2",
		"NODE_master_2_ETCD_URL_HOST":                      "192.168.2.2",
		"NODE_master_2_IP":                                 "192.168.2.2",
	}
)

func TestEnvVarController(t *testing.T) {
	scenarios := []struct {
		name            string
		objects         []runtime.Object
		staticPodStatus *operatorv1.StaticPodOperatorStatus
		certSecret      map[string][]byte
		expectedEnv     map[string]string
		expectedErr     error
	}{
		{
			name:        "HappyPath",
			expectedEnv: defaultEnvResult,
		},
		{
			name:            "MissingNodeStatus",
			staticPodStatus: u.StaticPodOperatorStatus(u.WithLatestRevision(3)),
			expectedEnv:     map[string]string{},
			expectedErr:     fmt.Errorf("empty NodeStatuses, can't generate environment for getEscapedIPAddress"),
		},
		{
			name: "MissingCertSecrets",
			certSecret: map[string][]byte{
				fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("master-0")): {},
				fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("master-2")): {},
			},
			expectedEnv: map[string]string{},
			expectedErr: fmt.Errorf("could not find serving cert for node [master-1] and key [etcd-serving-master-1.key]"),
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			observedConfig := map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"cipherSuites": []string{
						"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
						"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
						"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
				},
			}
			observedConfigYaml, err := yaml.Marshal(observedConfig)
			require.NoError(t, err)

			if scenario.staticPodStatus == nil {
				scenario.staticPodStatus = u.StaticPodOperatorStatus(
					u.WithLatestRevision(3),
					u.WithNodeStatusAtCurrentRevisionNamed(3, "master-0"),
					u.WithNodeStatusAtCurrentRevisionNamed(3, "master-1"),
					u.WithNodeStatusAtCurrentRevisionNamed(3, "master-2"),
				)
			}

			if scenario.certSecret == nil {
				scenario.certSecret = map[string][]byte{
					fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("master-0")): {},
					fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("master-1")): {},
					fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("master-2")): {},
				}
			}

			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
						ObservedConfig:  runtime.RawExtension{Raw: observedConfigYaml},
					},
				},
				scenario.staticPodStatus,
				nil,
				nil,
			)

			// the network lister needs to be isolated, as it also has a "cluster" resources conflicting with the infra one
			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, networkIndexer.Add(
				&configv1.Network{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: ceohelpers.InfrastructureClusterName,
					},
					Status: configv1.NetworkStatus{ServiceNetwork: []string{"192.168.0.0/32"}},
				},
			))

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
				u.FakeNode("master-0", u.WithNodeInternalIP("192.168.2.0")),
				u.FakeNode("master-1", u.WithNodeInternalIP("192.168.2.1")),
				u.FakeNode("master-2", u.WithNodeInternalIP("192.168.2.2")),
				u.EndpointsConfigMap(
					u.WithEndpoint(0, "https://192.168.2.0:2379"),
					u.WithEndpoint(1, "https://192.168.2.1:2379"),
					u.WithEndpoint(2, "https://192.168.2.2:2379")),
				u.ClusterConfigConfigMap(1),
				u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.EtcdAllCertsSecretName, scenario.certSecret),
			}

			fakeKubeClient := fake.NewSimpleClientset(scenario.objects...)
			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-envvarcontroller", &corev1.ObjectReference{}, clock.RealClock{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range defaultObjects {
				require.NoError(t, indexer.Add(obj))
			}

			for _, obj := range scenario.objects {
				require.NoError(t, indexer.Add(obj))
			}

			etcdIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, etcdIndexer.Add(&operatorv1.Etcd{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name: ceohelpers.InfrastructureClusterName,
				},
			}))

			controller := EnvVarController{
				operatorClient:       fakeOperatorClient,
				eventRecorder:        eventRecorder,
				configmapLister:      corev1listers.NewConfigMapLister(indexer),
				secretLister:         corev1listers.NewSecretLister(indexer),
				infrastructureLister: configv1listers.NewInfrastructureLister(indexer),
				masterNodeLister:     corev1listers.NewNodeLister(indexer),
				networkLister:        configv1listers.NewNetworkLister(networkIndexer),
				etcdLister:           operatorv1listers.NewEtcdLister(etcdIndexer),
				featureGateAccessor:  backendQuotaFeatureGateAccessor,
			}

			err = controller.sync(context.TODO())
			require.Equal(t, err, scenario.expectedErr)

			m := controller.GetEnvVars()
			require.Equal(t, scenario.expectedEnv, m)
		})
	}
}
