package etcdcertsigner

import (
	"bytes"
	"crypto/x509"
	"fmt"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/cert"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestCertSingleNode(t *testing.T) {
	node := u.FakeNode("cp-1", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.1"))
	secrets, bundles, err := CreateCertSecrets([]*corev1.Node{node})
	require.NoError(t, err)

	require.Equal(t, 11, len(secrets))
	require.Equal(t, 9, len(bundles))

	assertCertificateCorrectness(t, secrets)
	assertBundleCorrectness(t, secrets, bundles)
	assertNodeSpecificCertificates(t, node, secrets)
}

func TestCertsMultiNode(t *testing.T) {
	nodes := []*corev1.Node{
		u.FakeNode("cp-1", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.1")),
		u.FakeNode("cp-2", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.2")),
		u.FakeNode("cp-3", u.WithMasterLabel(), u.WithNodeInternalIP("192.168.2.3")),
	}
	secrets, bundles, err := CreateCertSecrets(nodes)
	require.NoError(t, err)

	require.Equal(t, 17, len(secrets))
	require.Equal(t, 9, len(bundles))

	assertCertificateCorrectness(t, secrets)
	assertBundleCorrectness(t, secrets, bundles)
	for _, node := range nodes {
		assertNodeSpecificCertificates(t, node, secrets)
	}
}

// assertCertificateCorrectness ensures that the certificates are signed by the correct signer
func assertCertificateCorrectness(t *testing.T, secrets []corev1.Secret) {
	certMap := map[string]string{
		"etcd-client":  tlshelpers.EtcdSignerCertSecretName,
		"etcd-peer-.*": tlshelpers.EtcdSignerCertSecretName,
		// adding the unit test node name convention here to avoid conflicts with etcd-serving-metrics certificates
		"etcd-serving-cp.*": tlshelpers.EtcdSignerCertSecretName,

		"etcd-metric-client":      tlshelpers.EtcdMetricsSignerCertSecretName,
		"etcd-serving-metrics-.*": tlshelpers.EtcdMetricsSignerCertSecretName,
	}

	signerMap := map[string]*x509.CertPool{}
	for _, s := range secrets {
		if s.Name == tlshelpers.EtcdSignerCertSecretName || s.Name == tlshelpers.EtcdMetricsSignerCertSecretName {
			ca, err := crypto.GetCAFromBytes(s.Data["tls.crt"], s.Data["tls.key"])
			require.NoError(t, err)

			pool := x509.NewCertPool()
			for _, crt := range ca.Config.Certs {
				pool.AddCert(crt)
			}
			signerMap[s.Name] = pool
		}
	}

	successfulMatches := sets.NewString()
	for _, s := range secrets {
		for certPattern, signerName := range certMap {
			if matches, _ := regexp.MatchString(certPattern, s.Name); matches {
				crt, err := crypto.CertsFromPEM(s.Data["tls.crt"])
				require.NoError(t, err)

				caPool := signerMap[signerName]
				for _, certificate := range crt {
					_, err := certificate.Verify(x509.VerifyOptions{
						Roots:     caPool,
						KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
					})
					require.NoErrorf(t, err, "unable to verify secret %s with signer %s", s.Name, signerName)
				}
				successfulMatches.Insert(certPattern)
				break
			}
		}
	}

	require.Equalf(t, successfulMatches.Len(), len(certMap), "missing coverage for some certificates, only found: %v", successfulMatches)
}

func assertBundleCorrectness(t *testing.T, secrets []corev1.Secret, bundles []corev1.ConfigMap) {
	signerMap := map[string][]*x509.Certificate{}
	for _, s := range secrets {
		if s.Name == tlshelpers.EtcdSignerCertSecretName || s.Name == tlshelpers.EtcdMetricsSignerCertSecretName {
			ca, err := crypto.GetCAFromBytes(s.Data["tls.crt"], s.Data["tls.key"])
			require.NoError(t, err)

			if sl, ok := signerMap[s.Name]; ok {
				sl = append(sl, ca.Config.Certs[0])
				signerMap[s.Name] = sl
			} else {
				signerMap[s.Name] = []*x509.Certificate{ca.Config.Certs[0]}
			}
		}
	}

	for _, bundle := range bundles {
		bundleCerts, err := cert.ParseCertsPEM([]byte(bundle.Data["ca-bundle.crt"]))
		require.NoError(t, err)

		var signers []*x509.Certificate
		if strings.Contains(bundle.Name, "metric") {
			signers = signerMap[tlshelpers.EtcdMetricsSignerCertSecretName]
		} else {
			signers = signerMap[tlshelpers.EtcdSignerCertSecretName]
		}

		sort.SliceStable(signers, func(i, j int) bool {
			return bytes.Compare(signers[i].Raw, signers[j].Raw) < 0
		})
		sort.SliceStable(bundleCerts, func(i, j int) bool {
			return bytes.Compare(bundleCerts[i].Raw, bundleCerts[j].Raw) < 0
		})

		require.Equal(t, signers, bundleCerts)
	}
}

// we only check that all certificates were generated, the correctness is verified above
func assertNodeSpecificCertificates(t *testing.T, node *corev1.Node, secrets []corev1.Secret) {
	expectedSet := sets.NewString(fmt.Sprintf("etcd-peer-%s", node.Name),
		fmt.Sprintf("etcd-serving-%s", node.Name),
		fmt.Sprintf("etcd-serving-metrics-%s", node.Name))

	for _, s := range secrets {
		expectedSet = expectedSet.Delete(s.Name)
	}

	require.Equalf(t, 0, expectedSet.Len(), "missing certificates for node: %v", expectedSet.List())
}
