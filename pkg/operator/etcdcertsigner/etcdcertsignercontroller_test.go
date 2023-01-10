package etcdcertsigner

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

func TestCheckCertValidity(t *testing.T) {
	ipAddresses := []string{"127.0.0.1"}
	differentIpAddresses := []string{"127.0.0.2"}

	nodeUID := "foo-bar"
	differentNodeUID := "bar-foo"

	expireDays := 100

	caConfig, err := crypto.MakeSelfSignedCAConfig("foo", expireDays)
	if err != nil {
		t.Fatalf("Failed to create ca config: %v", err)
	}
	ca := &crypto.CA{
		Config:          caConfig,
		SerialGenerator: &crypto.RandomSerialGenerator{},
	}

	testCases := map[string]struct {
		invalidCertPair     bool
		lessThanMinDuration bool
		certIPAddresses     []string
		nodeIPAddresses     []string
		storedNodeUID       string
		expectedRegenMsg    bool
		expectedErr         bool
	}{
		"invalid bytes": {
			invalidCertPair:  true,
			expectedRegenMsg: true,
		},
		"missing ip address; node uid changed": {
			certIPAddresses:  ipAddresses,
			nodeIPAddresses:  differentIpAddresses,
			storedNodeUID:    differentNodeUID,
			expectedRegenMsg: true,
		},
		"less than minimum duration remaining": {
			certIPAddresses:     ipAddresses,
			nodeIPAddresses:     ipAddresses,
			storedNodeUID:       nodeUID,
			lessThanMinDuration: true,
			expectedRegenMsg:    true,
		},
		"valid": {
			certIPAddresses: ipAddresses,
			nodeIPAddresses: ipAddresses,
			storedNodeUID:   nodeUID,
		},
	}
	for testName, tc := range testCases {
		t.Run(testName, func(t *testing.T) {
			certBytes := []byte{}
			keyBytes := []byte{}
			if !tc.invalidCertPair {
				// Generate a valid cert for the cert ip addresses
				certConfig, err := ca.MakeServerCert(sets.NewString(tc.certIPAddresses...), expireDays)
				if err != nil {
					t.Fatalf("Error generating cert: %v", err)
				}
				if tc.lessThanMinDuration {
					certConfig = ensureLessThanMinDuration(t, certConfig)
				}
				certBytes, keyBytes, err = certConfig.GetPEMBytes()
				if err != nil {
					t.Fatalf("Error converting cert to bytes: %v", err)
				}
			}
			msg, err := checkCertValidity(certBytes, keyBytes, tc.nodeIPAddresses, nodeUID, tc.storedNodeUID)
			if tc.expectedRegenMsg && len(msg) == 0 {
				t.Fatalf("Expected a regen message")
			}
			if !tc.expectedRegenMsg && len(msg) > 0 {
				t.Fatalf("Unexpected regen message: %s", msg)
			}
			if tc.expectedErr && err == nil {
				t.Fatalf("Expected an error")
			}
			if !tc.expectedErr && err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}

func TestEnsureCertForNode(t *testing.T) {
	// Any one of the cert configs will be representative
	certConfig := certConfigs["peer"]

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "master-0",
			UID:  uuid.NewUUID(),
		},
	}

	secretName := certConfig.secretNameFunc(node.Name)
	testCases := map[string]struct {
		certSecret     *corev1.Secret
		createExpected bool
		updateExpected bool
		ipAddresses    []string
	}{
		"missing cert secret is created": {
			createExpected: true,
			ipAddresses:    []string{"127.0.0.1"},
		},
		"invalid cert secret is regenerated": {
			certSecret:     newCertSecret(secretName, "", nil, nil),
			ipAddresses:    []string{"127.0.0.1"},
			updateExpected: true,
		},
		// this is a very common case when adding NICs on day2 operations
		"cert secret is regenerated when ip addresses change": {
			certSecret:     newCertSecretForIPs(t, secretName, []string{"127.0.0.1"}),
			ipAddresses:    []string{"127.0.0.1", "127.0.0.2"},
			createExpected: true,
			updateExpected: true,
		},
		// The test for a valid cert secret is performed after a successful
		// cert creation to simplify test setup.
	}
	for testName, tc := range testCases {
		t.Run(testName, func(t *testing.T) {
			objects := []runtime.Object{}
			if tc.certSecret != nil {
				objects = append(objects, tc.certSecret)
			}

			fakeKubeClient, controller, recorder := setupController(t, objects)
			secretName := certConfig.secretNameFunc(node.Name)
			nodeUID := string(node.UID)
			_, _, err := controller.ensureCertSecret(context.TODO(), secretName, nodeUID, tc.ipAddresses, certConfig, recorder)
			if err != nil {
				t.Fatal(err)
			}

			var updatedSecret *corev1.Secret
			var createdSecret *corev1.Secret
			for _, action := range fakeKubeClient.Actions() {
				if action.Matches("update", "secrets") {
					updateAction := action.(clientgotesting.UpdateAction)
					updatedSecret = updateAction.GetObject().(*corev1.Secret)
					break
				}
				if action.Matches("create", "secrets") {
					createAction := action.(clientgotesting.CreateAction)
					createdSecret = createAction.GetObject().(*corev1.Secret)
					break
				}
			}
			if !tc.updateExpected && updatedSecret != nil {
				t.Fatalf("Secret unexpectedly updated")
			}
			if updatedSecret != nil {
				validateTestSecret(t, "updated", updatedSecret, tc.ipAddresses)
			}
			if !tc.createExpected && createdSecret != nil {
				t.Fatalf("Secret unexpectedly created")
			}
			if createdSecret != nil {
				validateTestSecret(t, "created", createdSecret, tc.ipAddresses)

				// Verify that a cert secret created by ensureCertSecret will
				// not be updated when immediately round-tripped.
				objects := []runtime.Object{createdSecret}
				fakeKubeClient, controller, recorder := setupController(t, objects)
				_, _, err := controller.ensureCertSecret(context.TODO(), secretName, nodeUID,
					tc.ipAddresses, certConfig, recorder)
				if err != nil {
					t.Fatal(err)
				}
				for _, action := range fakeKubeClient.Actions() {
					if action.Matches("update", "secrets") {
						t.Fatal("Valid secret unexpectedly updated")
					}
					if action.Matches("create", "secrets") {
						t.Fatal("Valid secret unexpectedly updated")
					}
				}
			}
		})
	}
}

func TestSyncSkipsOnInsufficientQuorum(t *testing.T) {
	_, controller, recorder := setupController(t, []runtime.Object{})

	err := controller.sync(context.TODO(), factory.NewSyncContext("test", recorder))
	require.NoError(t, err)

	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
	}
	_, controller, recorder = setupControllerWithEtcd(t, []runtime.Object{
		u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
	}, etcdMembers)
	err = controller.sync(context.TODO(), factory.NewSyncContext("test", recorder))
	assert.Equal(t, "EtcdCertSignerController can't evaluate whether quorum is safe: etcd cluster has quorum of 2 which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.2:2380\" clientURLs:\"https://10.0.0.2:2907\"  Healthy:true Took: Error:<nil>}]",
		err.Error())
}

// Validate that a successful test run will result in a secret per
// cert type per node and an aggregated secret per cert type.
func TestSyncAllMasters(t *testing.T) {
	fakeKubeClient, controller, recorder := setupController(t, []runtime.Object{})
	err := controller.syncAllMasters(context.TODO(), recorder)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := fakeKubeClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secretMap := map[string]corev1.Secret{}
	for _, secret := range secrets.Items {
		secretMap[secret.Name] = secret
	}
	for _, certConfig := range certConfigs {
		// Cert secret per type per node
		for _, node := range nodes.Items {
			secretName := certConfig.secretNameFunc(node.Name)
			t.Run(secretName, func(t *testing.T) {
				secret, ok := secretMap[secretName]
				if !ok {
					t.Fatalf("secret %s is missing", secretName)
				}
				checkCertPairSecret(t, secretName, "tls.crt", "tls.key", secret.Data)
			})
		}
	}
	// A single aggregated secret
	secretName := tlshelpers.EtcdAllCertsSecretName
	t.Run(secretName, func(t *testing.T) {
		allSecret, ok := secretMap[secretName]
		if !ok {
			t.Fatalf("secret %s is missing", secretName)
		}
		// Cert pair per type per node
		for _, certConfig := range certConfigs {
			for _, node := range nodes.Items {
				secretName := certConfig.secretNameFunc(node.Name)
				certName := fmt.Sprintf("%s.crt", secretName)
				keyName := fmt.Sprintf("%s.key", secretName)
				checkCertPairSecret(t, secretName, certName, keyName, allSecret.Data)
			}
		}
	})
}

func checkCertPairSecret(t *testing.T, secretName, certName, keyName string, secretData map[string][]byte) {
	for _, key := range []string{certName, keyName} {
		if _, ok := secretData[certName]; !ok {
			t.Fatalf("secret %s is missing %s", secretName, key)
		}
	}
}

func setupController(t *testing.T, objects []runtime.Object) (*fake.Clientset, *EtcdCertSignerController, events.Recorder) {
	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
	}
	return setupControllerWithEtcd(t, objects, etcdMembers)
}

// setupController configures EtcdCertSignerController for testing with etcd members.
func setupControllerWithEtcd(t *testing.T, objects []runtime.Object, etcdMembers []*etcdserverpb.Member) (*fake.Clientset, *EtcdCertSignerController, events.Recorder) {
	// Add nodes and CAs
	objects = append(objects,
		u.FakeNode("master-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1")),
		u.FakeNode("master-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
		u.FakeNode("master-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3")),
	)
	signerNames := sets.NewString()
	for _, certConfig := range certConfigs {
		name := certConfig.caSecretName
		if signerNames.Has(name) {
			continue
		}
		signerNames.Insert(name)
		objects = append(objects, newCASecret(t, name))
	}

	fakeKubeClient := fake.NewSimpleClientset(objects...)
	indexer := cache.NewIndexer(
		cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	objects = append(objects,
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

	fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(etcdMembers)
	if err != nil {
		t.Fatal(err)
	}

	quorumChecker := ceohelpers.NewQuorumChecker(
		corev1listers.NewConfigMapLister(indexer),
		corev1listers.NewNamespaceLister(indexer),
		configv1listers.NewInfrastructureLister(indexer),
		fakeOperatorClient,
		fakeEtcdClient)

	controller := &EtcdCertSignerController{
		kubeClient:     fakeKubeClient,
		operatorClient: fakeOperatorClient,
		nodeLister:     corev1listers.NewNodeLister(indexer),
		secretClient:   fakeKubeClient.CoreV1(),
		quorumChecker:  quorumChecker,
	}
	recorder := events.NewRecorder(
		fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
		"test-cert-signer",
		&corev1.ObjectReference{},
	)
	return fakeKubeClient, controller, recorder
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

func newCertSecretForIPs(t *testing.T, secretName string, ips []string) *corev1.Secret {
	expireDays := 100

	caConfig, err := crypto.MakeSelfSignedCAConfig("foo", expireDays)
	if err != nil {
		t.Fatalf("Failed to create ca config: %v", err)
	}
	ca := &crypto.CA{
		Config:          caConfig,
		SerialGenerator: &crypto.RandomSerialGenerator{},
	}

	certConfig, err := ca.MakeServerCert(sets.NewString(ips...), expireDays)
	if err != nil {
		t.Fatalf("Error generating cert: %v", err)
	}

	certBytes, keyBytes, err := certConfig.GetPEMBytes()
	if err != nil {
		t.Fatalf("Error converting cert to bytes: %v", err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: operatorclient.TargetNamespace,
			Name:      secretName,
		},
		Data: map[string][]byte{
			"tls.crt": certBytes,
			"tls.key": keyBytes,
		},
	}
}

// validateTestSecret checks that a secret created or updated by
// ensureCertSecret is valid acording to checkCertValidity.
func validateTestSecret(t *testing.T, action string, secret *corev1.Secret, ipAddresses []string) {
	if secret.Data == nil {
		t.Fatalf("%s secret is empty", action)
	}
	storedNodeUID := secret.Annotations[nodeUIDAnnotation]
	msg, err := checkCertValidity(secret.Data["tls.crt"], secret.Data["tls.key"], ipAddresses, storedNodeUID, storedNodeUID)
	if len(msg) > 0 {
		t.Fatalf("%s secret is invalid with message: %s", action, msg)
	}
	if err != nil {
		t.Fatalf("%s secret is invalid with error: %v", action, err)
	}
}

func ensureLessThanMinDuration(t *testing.T, caConfig *crypto.TLSCertificateConfig) *crypto.TLSCertificateConfig {
	caCert := caConfig.Certs[0]

	// Issued 9 hours ago, 1 hour remaining: < 20% duration remaining
	caCert.NotBefore = time.Now().Add(-9 * time.Hour)
	caCert.NotAfter = time.Now().Add(time.Hour)

	rawCert, err := x509.CreateCertificate(rand.Reader, caCert, caCert, caCert.PublicKey, caConfig.Key)
	if err != nil {
		t.Fatalf("error creating certificate: %v", err)
	}
	parsedCerts, err := x509.ParseCertificates(rawCert)
	if err != nil {
		t.Fatalf("error parsing certificate: %v", err)
	}

	return &crypto.TLSCertificateConfig{
		Certs: []*x509.Certificate{parsedCerts[0]},
		Key:   caConfig.Key,
	}
}
