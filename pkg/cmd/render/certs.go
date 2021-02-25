package render

import (
	"crypto/x509/pkix"
	"time"

	"github.com/google/uuid"
	"github.com/openshift/library-go/pkg/crypto"
	"k8s.io/apiserver/pkg/authentication/user"
)

// keyMaterial simplifies handling of the key data produced by createKeyMaterial
type keyMaterial struct {
	caCert     []byte
	caKey      []byte
	caBundle   []byte
	clientCert []byte
	clientKey  []byte
}

// createKeyMaterial returns the key material for a self-signed CA, its CA bundle,
// and a client cert signed by the CA.
func createKeyMaterial(signerName, clientName string) (*keyMaterial, error) {
	// CA
	caSubject := pkix.Name{CommonName: signerName, OrganizationalUnit: []string{"openshift"}}
	caExpiryDays := 10 * 365 // 10 years
	signerCAConfig, err := crypto.MakeSelfSignedCAConfigForSubject(caSubject, caExpiryDays)
	if err != nil {
		return nil, err
	}
	caCert, caKey, err := signerCAConfig.GetPEMBytes()
	if err != nil {
		return nil, err
	}

	// Bundle
	caBundle, err := crypto.EncodeCertificates(signerCAConfig.Certs[0])
	if err != nil {
		return nil, err
	}

	// Client cert
	ca := &crypto.CA{
		Config:          signerCAConfig,
		SerialGenerator: &crypto.RandomSerialGenerator{},
	}
	clientUser := &user.DefaultInfo{
		Name:   clientName,
		UID:    uuid.New().String(),
		Groups: []string{clientName},
	}
	clientCertDuration := time.Hour * 24 * 365 * 10 // 10 years
	clientCertConfig, err := ca.MakeClientCertificateForDuration(clientUser, clientCertDuration)
	if err != nil {
		return nil, err
	}
	clientCert, clientKey, err := clientCertConfig.GetPEMBytes()
	if err != nil {
		return nil, err
	}

	return &keyMaterial{
		caCert:     caCert,
		caKey:      caKey,
		caBundle:   caBundle,
		clientCert: clientCert,
		clientKey:  clientKey,
	}, nil
}
