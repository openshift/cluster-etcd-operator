package tlshelpers

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openshift/library-go/pkg/crypto"
	"go.etcd.io/etcd/client/pkg/v3/tlsutil"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/klog/v2"
)

const (
	etcdCertValidity = 3 * 365 * 24 * time.Hour

	peerOrg   = "system:etcd-peers"
	serverOrg = "system:etcd-servers"
	metricOrg = "system:etcd-metrics"

	// TODO debt left for @hexfusion or @sanchezl
	fakePodFQDN = "etcd-client"

	EtcdAllCertsSecretName = "etcd-all-certs"
)

// SignerKeyMaterial simplifies handling of the key data produced by CreateSignerKeyMaterial
type SignerKeyMaterial struct {
	CaCert     []byte
	CaKey      []byte
	CaBundle   []byte
	ClientCert []byte
	ClientKey  []byte
}

func GetPeerClientSecretNameForNode(nodeName string) string {
	return fmt.Sprintf("etcd-peer-%s", nodeName)
}
func GetServingSecretNameForNode(nodeName string) string {
	return fmt.Sprintf("etcd-serving-%s", nodeName)
}
func GetServingMetricsSecretNameForNode(nodeName string) string {
	return fmt.Sprintf("etcd-serving-metrics-%s", nodeName)
}

func getPeerHostNames(nodeInternalIPs []string) []string {
	return append([]string{"localhost"}, nodeInternalIPs...)
}

func getServerHostNames(nodeInternalIPs []string) []string {
	return append([]string{
		"localhost",
		"etcd.kube-system.svc",
		"etcd.kube-system.svc.cluster.local",
		"etcd.openshift-etcd.svc",
		"etcd.openshift-etcd.svc.cluster.local",
		"127.0.0.1",
		"::1",
		"0:0:0:0:0:0:0:1",
	}, nodeInternalIPs...)
}

func CreatePeerCertKey(caCert, caKey []byte, nodeInternalIPs []string) (*bytes.Buffer, *bytes.Buffer, error) {
	return createNewCombinedClientAndServingCerts(caCert, caKey, fakePodFQDN, peerOrg, getPeerHostNames(nodeInternalIPs))
}

func CreateServerCertKey(caCert, caKey []byte, nodeInternalIPs []string) (*bytes.Buffer, *bytes.Buffer, error) {
	return createNewCombinedClientAndServingCerts(caCert, caKey, fakePodFQDN, serverOrg, getServerHostNames(nodeInternalIPs))
}

func CreateMetricCertKey(caCert, caKey []byte, nodeInternalIPs []string) (*bytes.Buffer, *bytes.Buffer, error) {
	return createNewCombinedClientAndServingCerts(caCert, caKey, fakePodFQDN, metricOrg, getServerHostNames(nodeInternalIPs))
}

func createNewCombinedClientAndServingCerts(caCert, caKey []byte, podFQDN, org string, hostNames []string) (*bytes.Buffer, *bytes.Buffer, error) {
	etcdCAKeyPair, err := crypto.GetCAFromBytes(caCert, caKey)
	if err != nil {
		return nil, nil, err
	}

	certConfig, err := etcdCAKeyPair.MakeServerCertForDuration(sets.NewString(hostNames...), etcdCertValidity, func(cert *x509.Certificate) error {
		cert.Subject = pkix.Name{
			Organization: []string{org},
			CommonName:   strings.TrimSuffix(org, "s") + ":" + podFQDN,
		}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}

		// TODO: Extended Key Usage:
		// All profiles expect a x509.ExtKeyUsageCodeSigning set on extended Key Usages
		// need to investigage: https://github.com/etcd-io/etcd/issues/9398#issuecomment-435340312

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	certBytes := &bytes.Buffer{}
	keyBytes := &bytes.Buffer{}
	if err := certConfig.WriteCertConfig(certBytes, keyBytes); err != nil {
		return nil, nil, err
	}
	return certBytes, keyBytes, nil
}

func SupportedEtcdCiphers(cipherSuites []string) []string {
	allowedCiphers := []string{}
	for _, cipher := range cipherSuites {
		_, ok := tlsutil.GetCipherSuite(cipher)
		if !ok {
			// skip and log unsupported ciphers
			klog.Warningf("cipher is not supported for use with etcd, skipping: %q", cipher)
			continue
		}
		allowedCiphers = append(allowedCiphers, cipher)
	}
	return allowedCiphers

}

// CreateSignerKeyMaterial returns the key material for a self-signed CA, its CA bundle,
// and a client cert signed by the CA.
func CreateSignerKeyMaterial(signerName, clientName string) (*SignerKeyMaterial, error) {
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

	return &SignerKeyMaterial{
		CaCert:     caCert,
		CaKey:      caKey,
		CaBundle:   caBundle,
		ClientCert: clientCert,
		ClientKey:  clientKey,
	}, nil
}
