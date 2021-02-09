package etcdcertsigner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/crypto"

	operatorv1 "github.com/openshift/api/operator/v1"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"go.etcd.io/etcd/pkg/mock/mockserver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

var (
	rootcaPublicKey  *rsa.PublicKey
	rootcaPrivateKey *rsa.PrivateKey
	publicKeyHash    []byte
	DummyPrivateKey  []byte
	DummyCertificate []byte
	nodeUIDs         []types.UID
)

func initialize(t *testing.T) {

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf(err.Error())
	}
	rootcaPublicKey = &privateKey.PublicKey
	rootcaPrivateKey = privateKey
	if err == nil {
		hash := sha1.New()
		hash.Write(rootcaPublicKey.N.Bytes())
		publicKeyHash = hash.Sum(nil)
	}
	if err != nil {
		t.Fatalf(err.Error())
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(2020),
		Subject: pkix.Name{
			Organization: []string{"Red Hat"},
			Country:      []string{"US"},
			Province:     []string{""},
			Locality:     []string{"Raleigh"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, rootcaPublicKey, rootcaPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := new(bytes.Buffer)
	pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})
	caPrivKeyPEM := new(bytes.Buffer)
	pem.Encode(caPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(rootcaPrivateKey),
	})
	DummyCertificate = caPEM.Bytes()
	DummyPrivateKey = caPrivKeyPEM.Bytes()

	nodeUIDs = []types.UID{
		uuid.NewUUID(),
		uuid.NewUUID(),
		uuid.NewUUID(),
	}
}

func TestValidateSAN(t *testing.T) {
	initialize(t)
	mockEtcd, err := mockserver.StartMockServers(3)
	if err != nil {
		t.Fatalf("failed to start mock servers: %s", err)
	}
	defer mockEtcd.Stop()
	scenarios := []struct {
		name            string
		objects         []runtime.Object
		staticPodStatus *operatorv1.StaticPodOperatorStatus
		wantErr         bool
		validateFunc    func(ts *testing.T, actions []clientgotesting.Action)
	}{
		{
			// All secrets match the node and IP address configuration.
			name: "NormalSteadyState",
			objects: []runtime.Object{
				u.FakeNodeWithUID("master-0", nodeUIDs[0], u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1")),
				u.FakeNodeWithUID("master-1", nodeUIDs[1], u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
				u.FakeNodeWithUID("master-2", nodeUIDs[2], u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3")),
				u.EndpointsConfigMap(
					u.WithAddress("10.0.0.1"),
					u.WithAddress("10.0.0.2"),
					u.WithAddress("10.0.0.3"),
				),
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "openshift-config",
						Name:      "etcd-signer",
					},
					Data: map[string][]byte{
						"tls.crt": DummyCertificate,
						"tls.key": DummyPrivateKey,
					},
				},

				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "openshift-config",
						Name:      "etcd-metric-signer",
					},
					Data: map[string][]byte{
						"tls.crt": DummyCertificate,
						"tls.key": DummyPrivateKey,
					},
				},
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-peer-%s", nodeUIDs[0]), makeCerts(t, []string{"localhost", "10.0.0.1"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-peer-%s", nodeUIDs[1]), makeCerts(t, []string{"localhost", "10.0.0.2"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-peer-%s", nodeUIDs[2]), makeCerts(t, []string{"localhost", "10.0.0.3"})),

				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-%s", nodeUIDs[0]), makeCerts(t, []string{"localhost", "10.0.0.1"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-%s", nodeUIDs[1]), makeCerts(t, []string{"localhost", "10.0.0.2"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-%s", nodeUIDs[2]), makeCerts(t, []string{"localhost", "10.0.0.3"})),

				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-metrics-%s", nodeUIDs[0]), makeCerts(t, []string{"localhost", "10.0.0.1"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-metrics-%s", nodeUIDs[1]), makeCerts(t, []string{"localhost", "10.0.0.2"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-metrics-%s", nodeUIDs[2]), makeCerts(t, []string{"localhost", "10.0.0.3"})),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			wantErr: false,
			validateFunc: func(ts *testing.T, actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "secrets") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.Secret)
						ts.Errorf("unexpected secret update: %#v", actual)
					}
				}
			},
		},

		{
			// IP addresses do not match the SAN configured in the certificates.
			// master-2 has a new IP addr (10.0.0.33) but certs still have old SAN (10.0.0.3).
			name: "IPMismatch",
			objects: []runtime.Object{
				u.FakeNodeWithUID("master-0", nodeUIDs[0], u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1")),
				u.FakeNodeWithUID("master-1", nodeUIDs[1], u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
				u.FakeNodeWithUID("master-2", nodeUIDs[2], u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.33")),
				u.EndpointsConfigMap(
					u.WithAddress("10.0.0.1"),
					u.WithAddress("10.0.0.2"),
					u.WithAddress("10.0.0.33"),
				),
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "openshift-config",
						Name:      "etcd-signer",
					},
					Data: map[string][]byte{
						"tls.crt": DummyCertificate,
						"tls.key": DummyPrivateKey,
					},
				},

				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "openshift-config",
						Name:      "etcd-metric-signer",
					},
					Data: map[string][]byte{
						"tls.crt": DummyCertificate,
						"tls.key": DummyPrivateKey,
					},
				},
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-peer-%s", nodeUIDs[0]), makeCerts(t, []string{"localhost", "10.0.0.1"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-peer-%s", nodeUIDs[1]), makeCerts(t, []string{"localhost", "10.0.0.2"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-peer-%s", nodeUIDs[2]), makeCerts(t, []string{"localhost", "10.0.0.3"})),

				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-%s", nodeUIDs[0]), makeCerts(t, []string{"localhost", "10.0.0.1"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-%s", nodeUIDs[1]), makeCerts(t, []string{"localhost", "10.0.0.2"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-%s", nodeUIDs[2]), makeCerts(t, []string{"localhost", "10.0.0.3"})),

				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-metrics-%s", nodeUIDs[0]), makeCerts(t, []string{"localhost", "10.0.0.1"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-metrics-%s", nodeUIDs[1]), makeCerts(t, []string{"localhost", "10.0.0.2"})),
				u.FakeSecret("openshift-etcd", fmt.Sprintf("etcd-serving-metrics-%s", nodeUIDs[2]), makeCerts(t, []string{"localhost", "10.0.0.3"})),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			wantErr: true,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				scenario.staticPodStatus,
				nil,
				nil,
			)

			fakeKubeClient := fake.NewSimpleClientset(scenario.objects...)
			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace), "test-etcdendpointscontroller", &corev1.ObjectReference{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			controller := &EtcdCertSignerController{
				kubeClient:           fakeKubeClient,
				operatorClient:       fakeOperatorClient,
				infrastructureLister: configlistersv1.NewInfrastructureLister(indexer),
				secretLister:         corev1listers.NewSecretLister(indexer),
				nodeLister:           corev1listers.NewNodeLister(indexer),
				secretClient:         fakeKubeClient.CoreV1(),
			}

			err := controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			if (err != nil) != scenario.wantErr {
				t.Fatal(err)
			}
			if scenario.validateFunc != nil {
				scenario.validateFunc(t, fakeKubeClient.Actions())
			}
		})
	}
}

func makeCerts(t *testing.T, hosts []string) map[string][]byte {

	ipAddrs, dnsAddrs := crypto.IPAddressesDNSNames(hosts)
	rootcaTemplate := &x509.Certificate{
		Subject:               pkix.Name{CommonName: fmt.Sprintf("etcd-cert-signer_@%d", time.Now().Unix())},
		SignatureAlgorithm:    x509.SHA256WithRSA,
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		SerialNumber:          big.NewInt(1),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           ipAddrs,
		DNSNames:              dnsAddrs,
		AuthorityKeyId:        publicKeyHash,
		SubjectKeyId:          publicKeyHash,
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, rootcaTemplate, rootcaTemplate, rootcaPublicKey, rootcaPrivateKey)
	if err != nil {
		t.Fatalf(err.Error())
	}
	caPEM := new(bytes.Buffer)
	pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})
	caPrivKeyPEM := new(bytes.Buffer)
	pem.Encode(caPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(rootcaPrivateKey),
	})
	return map[string][]byte{"tls.crt": caPEM.Bytes(), "tls.key": caPrivKeyPEM.Bytes()}
}
