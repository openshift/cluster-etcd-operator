package etcdcertsigner

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
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
		invalidCertPair  bool
		certIPAddresses  []string
		nodeIPAddresses  []string
		storedNodeUID    string
		expectedRegenMsg bool
		expectedErr      bool
	}{
		"invalid bytes": {
			invalidCertPair:  true,
			expectedRegenMsg: true,
		},
		"missing ip address; node uid unchanged": {
			certIPAddresses: ipAddresses,
			nodeIPAddresses: differentIpAddresses,
			storedNodeUID:   nodeUID,
			expectedErr:     true,
		},
		"missing ip address; node uid not stored": {
			certIPAddresses: ipAddresses,
			nodeIPAddresses: differentIpAddresses,
			expectedErr:     true,
		},
		"missing ip address; node uid changed": {
			certIPAddresses:  ipAddresses,
			nodeIPAddresses:  differentIpAddresses,
			storedNodeUID:    differentNodeUID,
			expectedRegenMsg: true,
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
	ipAddresses := []string{"127.0.0.1"}

	testCases := map[string]struct {
		certSecret     *corev1.Secret
		createExpected bool
		updateExpected bool
	}{
		"missing cert secret is created": {
			createExpected: true,
		},
		"invalid cert secret is regenerated": {
			certSecret:     newCertSecret(secretName, "", nil, nil),
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
			_, _, err := controller.ensureCertSecret(secretName, nodeUID, ipAddresses, certConfig, recorder)
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
				validateTestSecret(t, "updated", updatedSecret, ipAddresses)
			}
			if !tc.createExpected && createdSecret != nil {
				t.Fatalf("Secret unexpectedly created")
			}
			if createdSecret != nil {
				validateTestSecret(t, "created", createdSecret, ipAddresses)

				// Verify that a cert secret created by ensureCertSecret will
				// not be updated when immediately round-tripped.
				objects := []runtime.Object{createdSecret}
				fakeKubeClient, controller, recorder := setupController(t, objects)
				_, _, err := controller.ensureCertSecret(secretName, nodeUID, ipAddresses, certConfig, recorder)
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

// Validate that a successful test run will result in a secret per
// cert type per node and an aggregated secret per cert type.
func TestSyncAllMasters(t *testing.T) {
	fakeKubeClient, controller, recorder := setupController(t, []runtime.Object{})
	err := controller.syncAllMasters(recorder)
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

// setupController configures EtcdCertSignerController for testing.
func setupController(t *testing.T, objects []runtime.Object) (*fake.Clientset, *EtcdCertSignerController, events.Recorder) {
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
	for _, obj := range objects {
		if err := indexer.Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	controller := &EtcdCertSignerController{
		kubeClient:   fakeKubeClient,
		nodeLister:   corev1listers.NewNodeLister(indexer),
		secretLister: corev1listers.NewSecretLister(indexer),
		secretClient: fakeKubeClient.CoreV1(),
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
