package etcdcertsigner

import (
	"context"
	"crypto/x509"
	"fmt"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"testing"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

func TestSyncSkipsOnInsufficientQuorum(t *testing.T) {
	_, _, controller, recorder := setupController(t, []runtime.Object{})

	err := controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder))
	require.NoError(t, err)

	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
	}
	status := u.StaticPodOperatorStatus(
		u.WithLatestRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
	)
	_, _, controller, recorder = setupControllerWithEtcd(t, []runtime.Object{u.BootstrapConfigMap(u.WithBootstrapStatus("complete"))}, etcdMembers, status)
	err = controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder))
	assert.Equal(t, "EtcdCertSignerController can't evaluate whether quorum is safe: etcd cluster has quorum of 2 which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.2:2380\" clientURLs:\"https://10.0.0.2:2907\"  Healthy:true Took: Error:<nil>}]",
		err.Error())
}

func TestSyncSkipsOnRevisionRollingOut(t *testing.T) {
	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
	}
	status := u.StaticPodOperatorStatus(
		u.WithLatestRevision(5),
		u.WithNodeStatusAtCurrentRevision(5),
		u.WithNodeStatusAtCurrentRevision(5),
		u.WithNodeStatusAtCurrentRevision(3),
	)
	fakeClient, _, controller, recorder := setupControllerWithEtcd(t, []runtime.Object{u.BootstrapConfigMap(u.WithBootstrapStatus("complete"))}, etcdMembers, status)
	err := controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder))
	require.Nil(t, err)
	// ensure no secret has been queried to determine that we early exited the controller
	for _, action := range fakeClient.Actions() {
		if action.Matches("get", "secrets") {
			require.Fail(t, "found action that queried a secret, assuming the operator logic ran")
		}
	}
}

// Validate that a successful test run will result in a secret per
// cert type per node and an aggregated secret per cert type.
func TestSyncAllMasters(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{})
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)
}

func TestNewNodeAdded(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{})

	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)

	_, err := fakeKubeClient.CoreV1().Nodes().Create(context.TODO(), u.FakeNode("master-3", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.4")), metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap = allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 4, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)
}

func TestNodeChangingIPs(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{})

	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)

	n, err := fakeKubeClient.CoreV1().Nodes().Get(context.TODO(), "master-1", metav1.GetOptions{})
	require.NoError(t, err)
	// change its IP to a completely different subnet
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.100.100.1"}}
	_, err = fakeKubeClient.CoreV1().Nodes().Update(context.TODO(), n, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap = allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)
}

func TestClientCertsRemoval(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{})

	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)

	oldClientCert, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdClientCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	err = fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdClientCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	oldMetricClientCert, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdMetricsClientCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	err = fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdMetricsClientCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// this should regenerate the certificates
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap = allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertClientCerts(t, secretMap)
	assertOperatorStatus(t, fakeOperatorClient)

	// test that the secrets actually differ and the cert was regenerated
	require.NotEqual(t, oldClientCert.Data, secretMap[tlshelpers.EtcdClientCertSecretName])
	require.NotEqual(t, oldMetricClientCert.Data, secretMap[tlshelpers.EtcdMetricsClientCertSecretName])
}

func TestSecretApplyFailureSyncError(t *testing.T) {
	fakeKubeClient, _, controller, recorder := setupController(t, []runtime.Object{})
	fakeKubeClient.PrependReactor("create", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("apply failed")
	})
	require.Error(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
}

func allNodesAndSecrets(t *testing.T, fakeKubeClient *fake.Clientset) (*corev1.NodeList, map[string]corev1.Secret) {
	nodes, err := fakeKubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	secrets, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	secretMap := map[string]corev1.Secret{}
	for _, secret := range secrets.Items {
		secretMap[secret.Name] = secret
	}
	return nodes, secretMap
}

func assertStaticPodAllCerts(t *testing.T, nodes *corev1.NodeList, secretMap map[string]corev1.Secret) {
	// A single aggregated secret
	secretName := tlshelpers.EtcdAllCertsSecretName
	t.Run(secretName, func(t *testing.T) {
		allSecret, ok := secretMap[secretName]
		require.Truef(t, ok, "expected secret/%s to exist", secretName)
		// Cert pair per type per node
		for _, node := range nodes.Items {
			for _, secretName := range []string{
				tlshelpers.GetPeerClientSecretNameForNode(node.Name),
				tlshelpers.GetServingSecretNameForNode(node.Name),
				tlshelpers.GetServingMetricsSecretNameForNode(node.Name),
			} {
				certName := fmt.Sprintf("%s.crt", secretName)
				keyName := fmt.Sprintf("%s.key", secretName)
				checkCertPairSecret(t, secretName, certName, keyName, allSecret.Data)
			}
		}
	})
}

func assertNodeCerts(t *testing.T, nodes *corev1.NodeList, secretMap map[string]corev1.Secret) {
	// Cert secret per type per node
	for _, node := range nodes.Items {
		for _, secretName := range []string{
			tlshelpers.GetPeerClientSecretNameForNode(node.Name),
			tlshelpers.GetServingSecretNameForNode(node.Name),
			tlshelpers.GetServingMetricsSecretNameForNode(node.Name),
		} {
			t.Run(secretName, func(t *testing.T) {
				secret, ok := secretMap[secretName]
				require.Truef(t, ok, "expected secret/%s to exist", secretName)
				checkCertPairSecret(t, secretName, "tls.crt", "tls.key", secret.Data)
				checkCertNodeValidity(t, node, "tls.crt", "tls.key", secret.Data)
			})
		}
	}
}

func assertClientCerts(t *testing.T, secretMap map[string]corev1.Secret) {
	require.Containsf(t, secretMap, tlshelpers.EtcdClientCertSecretName, "expected secret/%s to exist", tlshelpers.EtcdClientCertSecretName)
	require.Containsf(t, secretMap, tlshelpers.EtcdMetricsClientCertSecretName, "expected secret/%s to exist", tlshelpers.EtcdMetricsClientCertSecretName)
}

func assertOperatorStatus(t *testing.T, client v1helpers.StaticPodOperatorClient) {
	_, status, _, _ := client.GetStaticPodOperatorState()
	revision, err := getCertRotationRevision(status, tlshelpers.EtcdSignerCertSecretName)
	require.NoError(t, err)
	require.Equal(t, int32(3), revision)

	revision, err = getCertRotationRevision(status, tlshelpers.EtcdMetricsSignerCertSecretName)
	require.NoError(t, err)
	require.Equal(t, int32(3), revision)
}

func checkCertNodeValidity(t *testing.T, node corev1.Node, certName, keyName string, secretData map[string][]byte) {
	cfg, err := crypto.GetTLSCertificateConfigFromBytes(secretData[certName], secretData[keyName])
	require.NoError(t, err)

	names, err := dnshelpers.GetInternalIPAddressesForNodeName(&node)
	require.NoError(t, err)

	expectedUsages := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
	// all internal names should be in any of the certs
	for _, name := range names {
		for _, cert := range cfg.Certs {
			if len(cert.ExtKeyUsage) > 0 {
				require.Equal(t, expectedUsages, cert.ExtKeyUsage)
				require.Containsf(t, cert.DNSNames, name, "expected node %s to have DNS name %s, but only had: %v", node.Name, name, cert.DNSNames)
			}
		}
	}
}

func checkCertPairSecret(t *testing.T, secretName, certName, keyName string, secretData map[string][]byte) {
	for _, key := range []string{certName, keyName} {
		if _, ok := secretData[key]; !ok {
			t.Fatalf("secret %s is missing %s", secretName, key)
		}
	}
}

func setupController(t *testing.T, objects []runtime.Object) (*fake.Clientset, v1helpers.StaticPodOperatorClient, factory.Controller, events.Recorder) {
	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
	}
	status := u.StaticPodOperatorStatus(
		u.WithLatestRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
	)

	return setupControllerWithEtcd(t, objects, etcdMembers, status)
}

// setupController configures EtcdCertSignerController for testing with etcd members.
func setupControllerWithEtcd(
	t *testing.T,
	objects []runtime.Object,
	etcdMembers []*etcdserverpb.Member,
	staticPodStatus *operatorv1.StaticPodOperatorStatus) (*fake.Clientset, v1helpers.StaticPodOperatorClient, factory.Controller, events.Recorder) {
	// Add nodes and CAs
	objects = append(objects,
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		u.FakeNode("master-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1")),
		u.FakeNode("master-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
		u.FakeNode("master-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3")),
	)

	objects = append(objects, newCASecret(t, tlshelpers.EtcdSignerCertSecretName), newCASecret(t, tlshelpers.EtcdMetricsSignerCertSecretName))

	indexer := cache.NewIndexer(
		cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	fakeKubeClient := fake.NewSimpleClientset(objects...)
	kubeInformerForNamespace := v1helpers.NewKubeInformersForNamespaces(fakeKubeClient, "",
		operatorclient.TargetNamespace, operatorclient.OperatorNamespace, operatorclient.GlobalUserSpecifiedConfigNamespace)

	objects = append(objects, &configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: ceohelpers.InfrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
	})
	for _, obj := range objects {
		require.NoError(t, indexer.Add(obj))
	}
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

	fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(etcdMembers)
	require.NoError(t, err)

	quorumChecker := ceohelpers.NewQuorumChecker(
		corev1listers.NewConfigMapLister(indexer),
		corev1listers.NewNamespaceLister(indexer),
		configv1listers.NewInfrastructureLister(indexer),
		fakeOperatorClient,
		fakeEtcdClient)

	recorder := events.NewRecorder(
		fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-cert-signer",
		&corev1.ObjectReference{},
	)

	nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
	require.NoError(t, err)

	controller := NewEtcdCertSignerController(
		health.NewMultiAlivenessChecker(),
		fakeKubeClient,
		fakeOperatorClient,
		kubeInformerForNamespace,
		kubeInformerForNamespace.InformersFor("").Core().V1().Nodes().Informer(),
		kubeInformerForNamespace.InformersFor("").Core().V1().Nodes().Lister(),
		nodeSelector,
		recorder,
		quorumChecker,
		// TODO(thomas): we need to test the revision gating now too :-)
		true)

	stopChan := make(chan struct{})
	t.Cleanup(func() {
		close(stopChan)
	})

	kubeInformerForNamespace.Start(stopChan)
	for ns := range kubeInformerForNamespace.Namespaces() {
		kubeInformerForNamespace.InformersFor(ns).WaitForCacheSync(stopChan)
	}

	return fakeKubeClient, fakeOperatorClient, controller, recorder
}

func newCASecret(t *testing.T, secretName string) *corev1.Secret {
	caConfig, err := crypto.MakeSelfSignedCAConfig("foo", 100)
	if err != nil {
		t.Fatalf("Failed to create ca config for %s: %v", secretName, err)
	}
	caCertBytes, caKeyBytes, err := caConfig.GetPEMBytes()
	if err != nil {
		t.Fatalf("Error converting ca %s to bytes: %v", secretName, err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace,
			Name:      secretName,
		},
		Data: map[string][]byte{
			"tls.crt": caCertBytes,
			"tls.key": caKeyBytes,
		},
	}
}
