package tlshelpers

import (
	"bytes"
	"context"
	gcrypto "crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/operator/certrotation"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/davecgh/go-spew/spew"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
)

type testEmbed struct {
	result string
}

func (t *testEmbed) NewCertificate(_ *crypto.CA, _ time.Duration) (*crypto.TLSCertificateConfig, error) {
	panic("implement me")
}

func (t *testEmbed) SetAnnotations(_ *crypto.TLSCertificateConfig, _ map[string]string) map[string]string {
	panic("implement me")
}

func (t *testEmbed) NeedNewTargetCertKeyPair(_ *corev1.Secret, _ *crypto.CA, _ []*x509.Certificate, _ time.Duration, _ bool, _ bool) string {
	return t.result
}

func TestEmbeddedStructHasPriority(t *testing.T) {
	embedded := CARotatingTargetCertCreator{&testEmbed{result: "definitive-result"}}
	require.Equal(t, "definitive-result", embedded.NeedNewTargetCertKeyPair(nil, nil, nil, time.Minute, false, false))
}

func TestSignerSignatureRotation(t *testing.T) {
	newCaWithAuthority := func() (*crypto.CA, error) {
		ski := make([]byte, 32)
		_, err := rand.Read(ski)
		if err != nil {
			return nil, err
		}

		return newTestCACertificateWithAuthority(pkix.Name{CommonName: "signer-tests"}, int64(1), metav1.Duration{Duration: time.Hour * 24 * 60}, time.Now, ski)
	}

	tests := []struct {
		name           string
		caFn           func() (*crypto.CA, error)
		matchingLeaf   bool
		expectRotation bool
	}{
		{
			name:           "leaf matches signer, no authority set",
			matchingLeaf:   true,
			expectRotation: false,
			caFn: func() (*crypto.CA, error) {
				return newTestCACertificate(pkix.Name{CommonName: "signer-tests"}, int64(1), metav1.Duration{Duration: time.Hour * 24 * 60}, time.Now)
			},
		},
		{
			name:           "leaf matches signer, authority set",
			matchingLeaf:   true,
			expectRotation: false,
			caFn:           newCaWithAuthority,
		},
		{
			name:           "leaf does not match signer, authority set",
			matchingLeaf:   false,
			expectRotation: true,
			caFn:           newCaWithAuthority,
		},
		{
			name:           "leaf does not match signer, no authority set",
			matchingLeaf:   false,
			expectRotation: true,
			caFn: func() (*crypto.CA, error) {
				return newTestCACertificate(pkix.Name{CommonName: "signer-tests"}, int64(1), metav1.Duration{Duration: time.Hour * 24 * 60}, time.Now)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

			client := kubefake.NewSimpleClientset()
			c := &certrotation.RotatedSelfSignedCertKeySecret{
				Namespace: "ns",
				Validity:  24 * time.Hour,
				Refresh:   12 * time.Hour,
				Name:      "target-secret",
				CertCreator: &CARotatingTargetCertCreator{
					&certrotation.SignerRotation{SignerName: "lower-signer"},
				},

				Client:        client.CoreV1(),
				Lister:        corev1listers.NewSecretLister(indexer),
				EventRecorder: events.NewInMemoryRecorder("test", clock.RealClock{}),
			}

			ca, err := test.caFn()
			if err != nil {
				t.Fatal(err)
			}

			secret, err := c.EnsureTargetCertKeyPair(context.TODO(), ca, ca.Config.Certs)
			if err != nil {
				t.Fatal(err)
			}

			// need to ensure the client returns the created secret now
			_ = indexer.Add(secret)

			if !test.matchingLeaf {
				ca, err = test.caFn()
				if err != nil {
					t.Fatal(err)
				}
			}

			newSecret, err := c.EnsureTargetCertKeyPair(context.TODO(), ca, ca.Config.Certs)
			if err != nil {
				t.Fatal(err)
			}

			if test.expectRotation {
				if bytes.Equal(newSecret.Data["tls.crt"], secret.Data["tls.crt"]) {
					t.Error("expected the certificate to rotate")
				}
				secretUpdated := false
				for _, action := range client.Actions() {
					if action.Matches("update", "secrets") {
						secretUpdated = true
					}
				}
				if !secretUpdated {
					t.Errorf("expected secret to get updated, but only found actions: %s", spew.Sdump(client.Actions()))
				}
			} else {
				if !bytes.Equal(newSecret.Data["tls.crt"], secret.Data["tls.crt"]) {
					t.Error("expected the certificate to not rotate")
				}

				secretUpdated := false
				for _, action := range client.Actions() {
					if action.Matches("update", "secrets") {
						secretUpdated = true
					}
				}
				if secretUpdated {
					t.Errorf("expected secret to not get updated, found actions: %s", spew.Sdump(client.Actions()))
				}
			}

		})
	}
}

// NewCACertificate generates and signs new CA certificate and key.
func newTestCACertificate(subject pkix.Name, serialNumber int64, validity metav1.Duration, currentTime func() time.Time) (*crypto.CA, error) {
	return newTestCACertificateWithAuthority(subject, serialNumber, validity, currentTime, nil)
}

func newTestCACertificateWithAuthority(subject pkix.Name, serialNumber int64, validity metav1.Duration, currentTime func() time.Time, subjectKeyId []byte) (*crypto.CA, error) {
	caPublicKey, caPrivateKey, err := crypto.NewKeyPair()
	if err != nil {
		return nil, err
	}

	caCert := &x509.Certificate{
		Subject: subject,

		SignatureAlgorithm: x509.SHA256WithRSA,

		NotBefore:      currentTime().Add(-1 * time.Second),
		NotAfter:       currentTime().Add(validity.Duration),
		SerialNumber:   big.NewInt(serialNumber),
		AuthorityKeyId: subjectKeyId,
		SubjectKeyId:   subjectKeyId,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	cert, err := signCertificate(caCert, caPublicKey, caCert, caPrivateKey)
	if err != nil {
		return nil, err
	}

	return &crypto.CA{
		Config: &crypto.TLSCertificateConfig{
			Certs: []*x509.Certificate{cert},
			Key:   caPrivateKey,
		},
		SerialGenerator: &crypto.RandomSerialGenerator{},
	}, nil
}

func signCertificate(template *x509.Certificate, requestKey gcrypto.PublicKey, issuer *x509.Certificate, issuerKey gcrypto.PrivateKey) (*x509.Certificate, error) {
	derBytes, err := x509.CreateCertificate(rand.Reader, template, issuer, requestKey, issuerKey)
	if err != nil {
		return nil, err
	}
	certs, err := x509.ParseCertificates(derBytes)
	if err != nil {
		return nil, err
	}
	if len(certs) != 1 {
		return nil, errors.New("Expected a single certificate")
	}
	return certs[0], nil
}
