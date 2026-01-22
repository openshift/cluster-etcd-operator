package etcdcertsigner

import (
	"context"
	"crypto/x509"
	"fmt"
	"sort"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	features "github.com/openshift/api/features"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/utils/clock"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

func TestHasNodeCertDiffGeneratesLeafs(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	// generate one valid certificate setup
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	// ensure that the bundled secrets do not contain any matching nodes
	_, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Update(context.TODO(),
		u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.EtcdAllCertsSecretName, map[string][]byte{}), metav1.UpdateOptions{})
	require.NoError(t, err)

	err = controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder))
	require.NoError(t, err)
	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	require.Equal(t, 14, len(secretMap))
	require.Equal(t, 3, len(configMaps))
}

func TestHasNodeCertDiffDoesNotRotateSigners(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	// generate one valid certificate setup
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	_, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Update(context.TODO(),
		u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.EtcdAllCertsSecretName, map[string][]byte{}), metav1.UpdateOptions{})
	require.NoError(t, err)
	signerCa, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdSignerCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	// ensure that the signer is outdated - by standards of library-go, this will identify this cert as unmanaged
	signerCa.Annotations = map[string]string{}
	_, err = fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Update(context.TODO(), signerCa, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = fakeOperatorClient.UpdateStaticPodOperatorStatus(context.TODO(), "2", u.StaticPodOperatorStatus(
		u.WithLatestRevision(4),
		u.WithNodeStatusAtCurrentRevision(4),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
	))
	require.NoError(t, err)

	err = controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder))
	require.NoError(t, err)

	// the signer should stay the exact same as before
	curSignerCa, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdSignerCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, signerCa.Data, curSignerCa.Data)

	// the certificates themselves should still be valid
	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)
}

// Validate that a successful test run will result in a secret per
// cert type per node and an aggregated secret per cert type.
func TestSyncAllMasters(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)
}

// Validate that a successful sync run can regenerate a missing etcd-all-bundles configmap
func TestSyncAllMastersMissingBundleConfigmap(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)

	// Delete the etcd-all-bundles configmap before the sync
	err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdAllBundlesConfigMapName, metav1.DeleteOptions{})
	require.NoError(t, err)

	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	// Validate that the etcd-all-bundles configmap is regenerated
	cm, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdAllBundlesConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, cm, "expected to find etcd-all-bundles configmap after sync")

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)
}

func TestSyncAllMastersForceSkip(t *testing.T) {
	fakeKubeClient, _, controller, recorder := setupController(t, []runtime.Object{}, true)
	// a single sync call should be enough now to generate all certs, this is important for bootstrap render
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)
}

func TestSignerRotation(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)

	// this will create an entirely new signer during the next sync loop
	err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdSignerCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, newSecretMap, newConfigMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, newSecretMap)
	assertStaticPodAllCerts(t, nodes, newSecretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, newConfigMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, newSecretMap)
	assertExpirationMetric(t)
	require.NotEqual(t, secretMap, newSecretMap)
	require.NotEqual(t, configMaps, newConfigMaps)
}

func TestMetricsSignerRotation(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)

	// this will create an entirely new signer during the next sync loop
	err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdMetricsSignerCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, newSecretMap, newConfigMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, newSecretMap)
	assertStaticPodAllCerts(t, nodes, newSecretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, newConfigMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, newSecretMap)
	assertExpirationMetric(t)
	require.NotEqual(t, secretMap, newSecretMap)
	require.NotEqual(t, configMaps, newConfigMaps)
}

func TestSignerRotationBlocksWithRevisionRollout(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	assertExpirationMetric(t)

	err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdSignerCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// this sync will create new signers and bundles
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
	// this should block because we didn't roll out a new revision (yet)
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
	_, newSecretMap, newConfigMaps := allNodesAndSecrets(t, fakeKubeClient)
	// subsequently the leaf certificates should not have changed
	require.Equal(t, filterLeafCerts(secretMap), filterLeafCerts(newSecretMap))
	// the metrics bundle should also be left untouched
	require.Equal(t, configMaps[tlshelpers.EtcdMetricsSignerCaBundleConfigMapName], newConfigMaps[tlshelpers.EtcdMetricsSignerCaBundleConfigMapName])
	// the signer bundles and the signer cert should be different
	require.NotEqual(t, secretMap[tlshelpers.EtcdSignerCertSecretName], newSecretMap[tlshelpers.EtcdSignerCertSecretName])
	require.NotEqual(t, configMaps[tlshelpers.EtcdSignerCaBundleConfigMapName], newConfigMaps[tlshelpers.EtcdSignerCaBundleConfigMapName])
}

func TestNewNodeAdded(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)

	_, err := fakeKubeClient.CoreV1().Nodes().Create(context.TODO(), u.FakeNode("master-3", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.4")), metav1.CreateOptions{})
	require.NoError(t, err)

	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps = allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 4, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
}

func TestNodeChangingIPs(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)

	n, err := fakeKubeClient.CoreV1().Nodes().Get(context.TODO(), "cp-1", metav1.GetOptions{})
	require.NoError(t, err)
	// change its IP to a completely different subnet
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.100.100.1"}}
	_, err = fakeKubeClient.CoreV1().Nodes().Update(context.TODO(), n, metav1.UpdateOptions{})
	require.NoError(t, err)

	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps = allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
}

func TestClientCertsRemoval(t *testing.T) {
	fakeKubeClient, fakeOperatorClient, controller, recorder := setupController(t, []runtime.Object{}, false)
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps := allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)

	oldClientCert, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdClientCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	err = fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdClientCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	oldMetricClientCert, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Get(context.TODO(), tlshelpers.EtcdMetricsClientCertSecretName, metav1.GetOptions{})
	require.NoError(t, err)
	err = fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).Delete(context.TODO(), tlshelpers.EtcdMetricsClientCertSecretName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// this should regenerate the certificates
	runSyncWithRevisionRollout(t, controller, fakeOperatorClient, recorder)

	nodes, secretMap, configMaps = allNodesAndSecrets(t, fakeKubeClient)
	require.Equal(t, 3, len(nodes.Items))
	assertNodeCerts(t, nodes, secretMap)
	assertStaticPodAllCerts(t, nodes, secretMap)
	assertCertificateCorrectness(t, secretMap)
	assertStaticPodAllBundles(t, configMaps)
	assertBundleCorrectness(t, secretMap, configMaps)
	assertClientCerts(t, secretMap)
	// test that the secrets actually differ and the cert was regenerated
	require.NotEqual(t, oldClientCert.Data, secretMap[tlshelpers.EtcdClientCertSecretName])
	require.NotEqual(t, oldMetricClientCert.Data, secretMap[tlshelpers.EtcdMetricsClientCertSecretName])
}

func TestSecretApplyFailureSyncError(t *testing.T) {
	fakeKubeClient, _, controller, recorder := setupController(t, []runtime.Object{}, true)
	fakeKubeClient.PrependReactor("create", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, &corev1.Secret{}, fmt.Errorf("apply failed")
	})
	require.Error(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
}

func TestConfigmapApplyFailureSyncError(t *testing.T) {
	fakeKubeClient, _, controller, recorder := setupController(t, []runtime.Object{}, true)
	fakeKubeClient.PrependReactor("create", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, &corev1.ConfigMap{}, fmt.Errorf("apply failed")
	})
	require.Error(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
}

func runSyncWithRevisionRollout(t *testing.T, controller factory.Controller, fakeOperatorClient v1helpers.StaticPodOperatorClient, recorder events.Recorder) {
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
	_, status, resourceVersion, err := fakeOperatorClient.GetStaticPodOperatorStateWithQuorum(context.TODO())
	require.NoError(t, err)
	status.LatestAvailableRevision += 1
	var ns []operatorv1.NodeStatus
	for _, nodeStatus := range status.NodeStatuses {
		cp := nodeStatus.DeepCopy()
		cp.CurrentRevision = status.LatestAvailableRevision
		ns = append(ns, *cp)
	}
	status.NodeStatuses = ns

	_, err = fakeOperatorClient.UpdateStaticPodOperatorStatus(context.TODO(), resourceVersion, status)
	require.NoError(t, err)
	require.NoError(t, controller.Sync(context.TODO(), factory.NewSyncContext("test", recorder)))
}

func allNodesAndSecrets(t *testing.T, fakeKubeClient *fake.Clientset) (*corev1.NodeList, map[string]corev1.Secret, map[string]corev1.ConfigMap) {
	nodes, err := fakeKubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	secrets, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	secretMap := map[string]corev1.Secret{}
	for _, secret := range secrets.Items {
		secretMap[secret.Name] = secret
	}

	configMaps, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	configMapsMap := map[string]corev1.ConfigMap{}
	for _, cm := range configMaps.Items {
		configMapsMap[cm.Name] = cm
	}
	return nodes, secretMap, configMapsMap
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

func assertCertificateCorrectness(t *testing.T, secretMap map[string]corev1.Secret) {
	var allSecrets []corev1.Secret
	for _, secret := range secretMap {
		allSecrets = append(allSecrets, secret)
	}
	u.AssertCertificateCorrectness(t, allSecrets)
}

func assertStaticPodAllBundles(t *testing.T, configMaps map[string]corev1.ConfigMap) {
	cmName := tlshelpers.EtcdAllBundlesConfigMapName
	t.Run(cmName, func(t *testing.T) {
		allBundles, ok := configMaps[cmName]
		require.Truef(t, ok, "expected configmaps/%s to exist", cmName)
		// always should have server and metrics bundle
		require.Equal(t, 2, len(allBundles.Data))
		require.NotNil(t, allBundles.Data["server-ca-bundle.crt"])
		require.NotNil(t, allBundles.Data["metrics-ca-bundle.crt"])
	})
}

func assertBundleCorrectness(t *testing.T, secretMap map[string]corev1.Secret, configMaps map[string]corev1.ConfigMap) {
	var allSecrets []corev1.Secret
	for _, secret := range secretMap {
		allSecrets = append(allSecrets, secret)
	}
	var allBundles []corev1.ConfigMap
	for _, cm := range configMaps {
		allBundles = append(allBundles, cm)
	}
	u.AssertBundleCorrectness(t, allSecrets, allBundles)
}

func assertNodeCerts(t *testing.T, nodes *corev1.NodeList, secretMap map[string]corev1.Secret) {
	var allSecrets []corev1.Secret
	for _, secret := range secretMap {
		allSecrets = append(allSecrets, secret)
	}

	// Cert secret per type per node
	for _, node := range nodes.Items {
		u.AssertNodeSpecificCertificates(t, &node, allSecrets)

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

func assertExpirationMetric(t *testing.T) {
	m, err := legacyregistry.DefaultGatherer.Gather()
	require.NoError(t, err)
	require.Equal(t, 1, len(m))

	require.Equal(t, signerExpirationMetricName, m[0].GetName())
	require.Equal(t, 2, len(m[0].Metric))
	sort.SliceStable(m[0].Metric, func(i, j int) bool {
		return strings.Compare(m[0].Metric[i].Label[0].GetValue(), m[0].Metric[j].Label[0].GetValue()) < 0
	})
	require.Equal(t, "metrics-signer-ca", m[0].Metric[0].Label[0].GetValue())
	require.Equal(t, "signer-ca", m[0].Metric[1].Label[0].GetValue())
	require.InEpsilon(t, float64(tlshelpers.EtcdCaCertValidity), m[0].Metric[0].GetGauge().GetValue(), float64(1))
	require.InEpsilon(t, float64(tlshelpers.EtcdCaCertValidity), m[0].Metric[1].GetGauge().GetValue(), float64(1))
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

func setupController(t *testing.T, objects []runtime.Object, forceSkipRollout bool) (*fake.Clientset, v1helpers.StaticPodOperatorClient, factory.Controller, events.Recorder) {
	// Add nodes and CAs
	objects = append(objects,
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		u.FakeNode("cp-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1")),
		u.FakeNode("cp-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
		u.FakeNode("cp-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3")),
		u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
		u.FakeSecret(operatorclient.TargetNamespace, tlshelpers.EtcdAllCertsSecretName, map[string][]byte{
			fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("cp-0")): {},
			fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("cp-1")): {},
			fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode("cp-2")): {},
		}),
		u.FakeConfigMap(operatorclient.TargetNamespace, tlshelpers.EtcdAllBundlesConfigMapName, map[string]string{}),
	)

	indexer := cache.NewIndexer(
		cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	fakeKubeClient := fake.NewSimpleClientset(objects...)
	kubeInformerForNamespace := v1helpers.NewKubeInformersForNamespaces(fakeKubeClient,
		"",
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.KubeSystemNamespace)

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
		u.StaticPodOperatorStatus(
			u.WithLatestRevision(3),
			u.WithNodeStatusAtCurrentRevision(3),
			u.WithNodeStatusAtCurrentRevision(3),
			u.WithNodeStatusAtCurrentRevision(3),
		),
		nil,
		nil,
	)

	recorder := events.NewRecorder(
		fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-cert-signer",
		&corev1.ObjectReference{},
		clock.RealClock{},
	)

	nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
	require.NoError(t, err)

	registry := metrics.NewKubeRegistry()
	legacyregistry.DefaultGatherer = registry

	enabledFeatureGates := sets.New(features.FeatureShortCertRotation)
	disabledFeatureGates := sets.New[configv1.FeatureGateName]()
	featureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(enabledFeatureGates.UnsortedList(), disabledFeatureGates.UnsortedList())
	infraLister := u.FakeInfrastructureLister(t, configv1.HighlyAvailableTopologyMode)
	controller, err := NewEtcdCertSignerController(
		health.NewMultiAlivenessChecker(),
		fakeKubeClient,
		fakeOperatorClient,
		kubeInformerForNamespace,
		kubeInformerForNamespace.InformersFor("").Core().V1().Nodes().Informer(),
		kubeInformerForNamespace.InformersFor("").Core().V1().Nodes().Lister(),
		nodeSelector,
		infraLister,
		recorder,
		registry,
		forceSkipRollout,
		featureGateAccessor)
	require.NoError(t, err)

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

func filterLeafCerts(secretMap map[string]corev1.Secret) map[string]corev1.Secret {
	filtered := map[string]corev1.Secret{}
	for s, secret := range secretMap {
		if s != tlshelpers.EtcdMetricsSignerCertSecretName && s != tlshelpers.EtcdSignerCertSecretName {
			filtered[s] = secret
		}
	}

	return filtered
}
