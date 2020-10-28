package tlshelpers

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/library-go/pkg/crypto"
)

const (
	etcdCertValidity = 3 * 365 * 24 * time.Hour

	peerOrg   = "system:etcd-peers"
	serverOrg = "system:etcd-servers"
	metricOrg = "system:etcd-metrics"

	// TODO debt left for @hexfusion or @sanchezl
	fakePodFQDN = "etcd-client"

	EtcdAllPeerSecretName           = "etcd-all-peer"
	EtcdAllServingSecretName        = "etcd-all-serving"
	EtcdAllServingMetricsSecretName = "etcd-all-serving-metrics"
)

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
	cn, err := getCommonNameFromOrg(org)
	etcdCAKeyPair, err := crypto.GetCAFromBytes(caCert, caKey)
	if err != nil {
		return nil, nil, err
	}

	certConfig, err := etcdCAKeyPair.MakeServerCertForDuration(sets.NewString(hostNames...), etcdCertValidity, func(cert *x509.Certificate) error {

		cert.Issuer = pkix.Name{
			OrganizationalUnit: []string{"openshift"},
			CommonName:         cn,
		}
		cert.Subject = pkix.Name{
			Organization: []string{org},
			CommonName:   strings.TrimSuffix(org, "s") + ":" + podFQDN,
		}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}

		// TODO: Extended Key Usage:
		// All profiles expect a x509.ExtKeyUsageCodeSigning set on extended Key Usages
		// need to investigage: https://github.com/etcd-io/etcd/issues/9398#issuecomment-435340312
		// TODO: some extensions are missing form cfssl.
		// e.g.
		//	X509v3 Subject Key Identifier:
		//		B7:30:0B:CF:47:4E:21:AE:13:60:74:42:B0:D9:C4:F3:26:69:63:03
		//	X509v3 Authority Key Identifier:
		//		keyid:9B:C0:6B:0C:8E:5C:73:6A:83:B1:E4:54:97:D3:62:18:8A:9C:BC:1E
		// TODO: Change serial number logic, to something as follows.
		// The following is taken from CFSSL library.
		// If CFSSL is providing the serial numbers, it makes
		// sense to use the max supported size.

		//	serialNumber := make([]byte, 20)
		//	_, err = io.ReadFull(rand.Reader, serialNumber)
		//	if err != nil {
		//		return err
		//	}
		//
		//	// SetBytes interprets buf as the bytes of a big-endian
		//	// unsigned integer. The leading byte should be masked
		//	// off to ensure it isn't negative.
		//	serialNumber[0] &= 0x7F
		//	cert.SerialNumber = new(big.Int).SetBytes(serialNumber)
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

func getCommonNameFromOrg(org string) (string, error) {
	if strings.Contains(org, "peer") || strings.Contains(org, "server") {
		return "etcd-signer", nil
	}
	if strings.Contains(org, "metric") {
		return "etcd-metric-signer", nil
	}
	return "", errors.New("unable to recognise secret name")
}
