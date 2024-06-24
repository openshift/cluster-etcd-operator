package tlshelpers

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/certrotation"
	corev1 "k8s.io/api/core/v1"
)

// CARotatingTargetCertCreator ensures we also rotate leaf certificates when we detect a change in signer.
// The certrotation.TargetCertCreator only assumes the bundle to change on a CA rotation, whereas we have to keep
// the bundle around for some time for a proper static pod rollout.
type CARotatingTargetCertCreator struct {
	certrotation.TargetCertCreator
}

func (c *CARotatingTargetCertCreator) NeedNewTargetCertKeyPair(
	secret *corev1.Secret,
	signer *crypto.CA,
	caBundleCerts []*x509.Certificate,
	refresh time.Duration,
	refreshOnlyWhenExpired bool,
	secretDoesntExist bool) string {

	result := c.TargetCertCreator.NeedNewTargetCertKeyPair(secret, signer, caBundleCerts, refresh, refreshOnlyWhenExpired, secretDoesntExist)
	if result != "" {
		return result
	}

	// TODO(thomas): we measured that this parsing is not very CPU intensive compared to TLS handshakes to etcd
	// we could save about 3-5% cpu usage here by caching the certs based on their hashes.
	var currentCert *x509.Certificate
	if crtBytes, ok := secret.Data["tls.crt"]; ok {
		pemCerts, err := crypto.CertsFromPEM(crtBytes)
		if err != nil {
			return fmt.Sprintf("could not parse pem x509 tls.crt in secret %s: %v", secret.Name, err)
		}
		if len(pemCerts) > 0 {
			currentCert = pemCerts[len(pemCerts)-1]
		}
	}

	if currentCert == nil {
		return fmt.Sprintf("missing current certificate in secret: %s", secret.Name)
	}

	// in some cases, e.g. with etcd, we need to bundle the signer CA before we can rotate a certificate
	// hence we also check whether the signer itself has changed, denoted by its AKI/SKI
	if len(currentCert.AuthorityKeyId) > 0 && len(signer.Config.Certs) > 0 && len(signer.Config.Certs[0].SubjectKeyId) > 0 {
		if !bytes.Equal(currentCert.AuthorityKeyId, signer.Config.Certs[0].SubjectKeyId) {
			return fmt.Sprintf("signer subject key for secret %s does not match cert authority key anymore", secret.Name)
		}
	} else {
		// no AKI/SKI available to us, we have to check whether the cert was actually signed with this signer
		pool := x509.NewCertPool()
		for _, crt := range signer.Config.Certs {
			pool.AddCert(crt)
		}

		_, err := currentCert.Verify(x509.VerifyOptions{
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		})
		if err != nil {
			return fmt.Sprintf("cert isn't signed by most recent signer anymore: %v", err)
		}
	}

	return ""
}
