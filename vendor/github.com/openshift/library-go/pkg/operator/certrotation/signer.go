package certrotation

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehelper"
	"github.com/openshift/library-go/pkg/pki"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

// RotatedSigningCASecret rotates a self-signed signing CA stored in a secret. It creates a new one when
// - refresh duration is over
// - or 80% of validity is over (if RefreshOnlyWhenExpired is false)
// - or the CA is expired.
type RotatedSigningCASecret struct {
	// Namespace is the namespace of the Secret.
	namespace string
	// Name is the name of the Secret.
	name string
	// Validity is the duration from time.Now() until the signing CA expires. If RefreshOnlyWhenExpired
	// is false, the signing cert is rotated when 80% of validity is reached.
	validity time.Duration
	// Refresh is the duration after signing CA creation when it is rotated at the latest. It is ignored
	// if RefreshOnlyWhenExpired is true, or if Refresh > Validity.
	refresh time.Duration
	// RefreshOnlyWhenExpired set to true means to ignore 80% of validity and the Refresh duration for rotation,
	// but only rotate when the signing CA expires. This is useful for auto-recovery when we want to enforce
	// rotation on expiration only, but not interfere with the ordinary rotation controller.
	refreshOnlyWhenExpired bool

	// Owner is an optional reference to add to the secret that this rotator creates. Use this when downstream
	// consumers of the signer CA need to be aware of changes to the object.
	// WARNING: be careful when using this option, as deletion of the owning object will cascade into deletion
	// of the signer. If the lifetime of the owning object is not a superset of the lifetime in which the signer
	// is used, early deletion will be catastrophic.
	owner *metav1.OwnerReference

	// AdditionalAnnotations is a collection of annotations set for the secret
	additionalAnnotations AdditionalAnnotations

	// CertificateName is the logical name of this certificate for PKI profile resolution.
	certificateName string

	// PKIProfileProvider, when non-nil, enables ConfigurablePKI certificate
	// key algorithm resolution. When nil, legacy certificate generation is used.
	pkiProfileProvider pki.PKIProfileProvider

	// Plumbing:
	informer      corev1informers.SecretInformer
	lister        corev1listers.SecretLister
	client        corev1client.SecretsGetter
	eventRecorder events.Recorder

	clock clock.PassiveClock
}

func NewRotatedSigningCASecret(
	namespace string,
	name string,
	certificateName string,
	validity time.Duration,
	refresh time.Duration,
	refreshOnlyWhenExpired bool,
	informer corev1informers.SecretInformer,
	client corev1client.SecretsGetter,
	additionalAnnotations AdditionalAnnotations,
	owner *metav1.OwnerReference,
	eventRecorder events.Recorder,
	pkiProfileProvider pki.PKIProfileProvider,
) RotatedSigningCASecret {
	return RotatedSigningCASecret{
		name:                   name,
		namespace:              namespace,
		certificateName:        certificateName,
		validity:               validity,
		refresh:                refresh,
		refreshOnlyWhenExpired: refreshOnlyWhenExpired,
		informer:               informer,
		lister:                 informer.Lister(),
		client:                 client,
		additionalAnnotations:  additionalAnnotations,
		owner:                  owner,
		eventRecorder:          eventRecorder,
		pkiProfileProvider:     pkiProfileProvider,
		clock:                  clock.RealClock{},
	}
}

// EnsureSigningCertKeyPair manages the entire lifecycle of a signer cert as a secret, from creation to continued rotation.
// It always returns the currently used CA pair, a bool indicating whether it was created/updated within this function call and an error.
func (c RotatedSigningCASecret) EnsureSigningCertKeyPair(ctx context.Context) (*crypto.CA, bool, error) {
	creationRequired := false
	updateRequired := false
	originalSigningCertKeyPairSecret, err := c.lister.Secrets(c.namespace).Get(c.name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	signingCertKeyPairSecret := originalSigningCertKeyPairSecret.DeepCopy()
	if apierrors.IsNotFound(err) {
		// create an empty one
		signingCertKeyPairSecret = &corev1.Secret{
			ObjectMeta: NewTLSArtifactObjectMeta(
				c.name,
				c.namespace,
				c.additionalAnnotations,
			),
			Type: corev1.SecretTypeTLS,
		}
		creationRequired = true
	}

	// run Update if metadata needs changing unless we're in RefreshOnlyWhenExpired mode
	if !c.refreshOnlyWhenExpired {
		needsMetadataUpdate := ensureOwnerRefAndTLSAnnotations(signingCertKeyPairSecret, c.owner, c.additionalAnnotations)
		needsTypeChange := ensureSecretTLSTypeSet(signingCertKeyPairSecret)
		updateRequired = needsMetadataUpdate || needsTypeChange
	}

	// run Update if signer content needs changing
	signerUpdated := false

	if needed, reason := c.needNewSigningCertKeyPair(signingCertKeyPairSecret); needed || creationRequired {
		if creationRequired {
			reason = "secret doesn't exist"
		}
		c.eventRecorder.Eventf("SignerUpdateRequired", "%q in %q requires a new signing cert/key pair: %v", c.name, c.namespace, reason)
		if err = c.setSigningCertKeyPairSecretAndTLSAnnotations(signingCertKeyPairSecret, originalSigningCertKeyPairSecret); err != nil {
			return nil, false, err
		}

		LabelAsManagedSecret(signingCertKeyPairSecret, CertificateTypeSigner)

		updateRequired = true
		signerUpdated = true
	}

	if creationRequired {
		actualSigningCertKeyPairSecret, err := c.client.Secrets(c.namespace).Create(ctx, signingCertKeyPairSecret, metav1.CreateOptions{})
		resourcehelper.ReportCreateEvent(c.eventRecorder, actualSigningCertKeyPairSecret, err)
		if err != nil {
			return nil, false, err
		}
		klog.V(2).Infof("Created secret %s/%s", actualSigningCertKeyPairSecret.Namespace, actualSigningCertKeyPairSecret.Name)
		signingCertKeyPairSecret = actualSigningCertKeyPairSecret
	} else if updateRequired {
		actualSigningCertKeyPairSecret, err := c.client.Secrets(c.namespace).Update(ctx, signingCertKeyPairSecret, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			// ignore error if its attempting to update outdated version of the secret
			return nil, false, nil
		}
		resourcehelper.ReportUpdateEvent(c.eventRecorder, actualSigningCertKeyPairSecret, err)
		if err != nil {
			return nil, false, err
		}
		klog.V(2).Infof("Updated secret %s/%s", actualSigningCertKeyPairSecret.Namespace, actualSigningCertKeyPairSecret.Name)
		signingCertKeyPairSecret = actualSigningCertKeyPairSecret
	}

	// at this point, the secret has the correct signer, so we should read that signer to be able to sign
	signingCertKeyPair, err := crypto.GetCAFromBytes(signingCertKeyPairSecret.Data["tls.crt"], signingCertKeyPairSecret.Data["tls.key"])
	if err != nil {
		return nil, signerUpdated, err
	}

	return signingCertKeyPair, signerUpdated, nil
}

// ensureOwnerReference adds the owner to the list of owner references in meta, if necessary
func ensureOwnerReference(meta *metav1.ObjectMeta, owner *metav1.OwnerReference) bool {
	var found bool
	for _, ref := range meta.OwnerReferences {
		if ref == *owner {
			found = true
			break
		}
	}
	if !found {
		meta.OwnerReferences = append(meta.OwnerReferences, *owner)
		return true
	}
	return false
}

func (c RotatedSigningCASecret) needNewSigningCertKeyPair(secret *corev1.Secret) (bool, string) {
	annotations := secret.Annotations
	notBefore, notAfter, reason := getValidityFromAnnotations(annotations)
	if len(reason) > 0 {
		return true, reason
	}

	if c.clock.Now().After(notAfter) {
		return true, "already expired"
	}

	if c.refreshOnlyWhenExpired {
		return false, ""
	}

	validity := notAfter.Sub(notBefore)
	at80Percent := notAfter.Add(-validity / 5)
	if c.clock.Now().After(at80Percent) {
		return true, fmt.Sprintf("past refresh time (80%% of validity): %v", at80Percent)
	}

	developerSpecifiedRefresh := notBefore.Add(c.refresh)
	if c.clock.Now().After(developerSpecifiedRefresh) {
		return true, fmt.Sprintf("past its refresh time %v", developerSpecifiedRefresh)
	}

	// If we support configuration of the signature algorithm, a mismatch between
	// the desired signature algorithm and the current signature algorithm
	// means we need to rotate.
	if c.pkiProfileProvider != nil {
		keygen, err := c.resolveKeyPairGenerator()
		if err != nil {
			klog.Warningf("failed to resolve the desired key algorithm, refusing to cause rotation based on key algorithm state")
			return false, ""
		}

		if keygen == nil {
			keygen = crypto.RSAKeyPairGenerator{Bits: 2048}
		}

		if keygen.Algorithm() != annotations[CertificateKeyAlgorithm] {
			return true, fmt.Sprintf("key algorithm changed from %q to %q", annotations[CertificateKeyAlgorithm], keygen.Algorithm())
		}
	}

	return false, ""
}

func getValidityFromAnnotations(annotations map[string]string) (notBefore time.Time, notAfter time.Time, reason string) {
	notAfterString := annotations[CertificateNotAfterAnnotation]
	if len(notAfterString) == 0 {
		return notBefore, notAfter, "missing notAfter"
	}
	notAfter, err := time.Parse(time.RFC3339, notAfterString)
	if err != nil {
		return notBefore, notAfter, fmt.Sprintf("bad expiry: %q", notAfterString)
	}
	notBeforeString := annotations[CertificateNotBeforeAnnotation]
	if len(notBeforeString) == 0 {
		return notBefore, notAfter, "missing notBefore"
	}
	notBefore, err = time.Parse(time.RFC3339, notBeforeString)
	if err != nil {
		return notBefore, notAfter, fmt.Sprintf("bad expiry: %q", notBeforeString)
	}

	return notBefore, notAfter, ""
}

func (c RotatedSigningCASecret) resolveKeyPairGenerator() (crypto.KeyPairGenerator, error) {
	return resolveKeyPairGeneratorWithFallback(c.pkiProfileProvider, pki.CertificateTypeSigner, c.certificateName)
}

// resolveKeyPairGeneratorWithFallback resolves the key pair generator from the
// PKI profile provider. Returns nil for Unmanaged mode (no key override).
//
// TODO(sanchezl): Remove the fallback to DefaultPKIProfile() once installer
// support for the PKI resource is in place. Until then, the PKI resource may
// not exist in TechPreview clusters. Once removed, callers can use
// pki.ResolveCertificateConfig directly.
func resolveKeyPairGeneratorWithFallback(provider pki.PKIProfileProvider, certType pki.CertificateType, name string) (crypto.KeyPairGenerator, error) {
	cfg, err := pki.ResolveCertificateConfig(provider, certType, name)
	if err != nil {
		klog.Warningf("Failed to resolve PKI config for %s %q, falling back to default profile: %v", certType, name, err)
		defaultProfile := pki.DefaultPKIProfile()
		cfg, err = pki.ResolveCertificateConfig(pki.NewStaticPKIProfileProvider(&defaultProfile), certType, name)
		if err != nil {
			return nil, err
		}
	}
	if cfg == nil {
		return nil, nil
	}
	return cfg.Key, nil
}

// setSigningCertKeyPairSecretAndTLSAnnotations generates a new signing certificate and key pair,
// stores them in the specified secret, and adds predefined TLS annotations to that secret.
func (c RotatedSigningCASecret) setSigningCertKeyPairSecretAndTLSAnnotations(signingCertKeyPairSecret *corev1.Secret, originalSigningCertKeyPairSecret *corev1.Secret) error {
	ca, err := c.setSigningCertKeyPairSecret(signingCertKeyPairSecret, originalSigningCertKeyPairSecret)
	if err != nil {
		return err
	}

	c.setTLSAnnotationsOnSigningCertKeyPairSecret(signingCertKeyPairSecret, ca)
	return nil
}

// setSigningCertKeyPairSecret creates a new signing cert/key pair and sets them in the secret.
func (c RotatedSigningCASecret) setSigningCertKeyPairSecret(signingCertKeyPairSecret *corev1.Secret, originalSigningCertKeyPairSecret *corev1.Secret) (*crypto.TLSCertificateConfig, error) {
	signerName := fmt.Sprintf("%s_%s@%d", signingCertKeyPairSecret.Namespace, signingCertKeyPairSecret.Name, c.clock.Now().Unix())

	var keyPairGenerator crypto.KeyPairGenerator = crypto.RSAKeyPairGenerator{Bits: 2048}
	if c.pkiProfileProvider != nil {
		keyGen, err := c.resolveKeyPairGenerator()
		if err != nil {
			return nil, err
		}

		if keyGen != nil {
			keyPairGenerator = keyGen
		}
	}

	ca, err := crypto.NewSigningCertificate(signerName, keyPairGenerator, crypto.WithLifetime(c.validity), crypto.WithClock(c.clock))
	if err != nil {
		return nil, err
	}

	// If we have an existing original signing cert key pair secret,
	// that means we have an older singer cert key pair that has been used for
	// certificate issuance and we need to bridge trust for the new signing
	// cert key pair for clients that only trust the old signing cer key pair.
	// We do this by appending an intermediate certificate where the old
	// signer signs the new signer certificate.
	if originalSigningCertKeyPairSecret != nil {
		certData := originalSigningCertKeyPairSecret.Data["tls.crt"]
		keyData := originalSigningCertKeyPairSecret.Data["tls.key"]

		if len(certData) > 0 && len(keyData) > 0 {
			oldSigner, err := crypto.GetCAFromBytes(originalSigningCertKeyPairSecret.Data["tls.crt"], originalSigningCertKeyPairSecret.Data["tls.key"])
			if err != nil {
				return nil, fmt.Errorf("building old signer from original singing cert key pair: %w", err)
			}

			// if the old signer certificate is already expired it is too late to cross-sign.
			if c.clock.Now().Before(oldSigner.Config.Certs[0].NotAfter) {
				oldSignerCARemainingLifetime := -1 * c.clock.Now().Sub(oldSigner.Config.Certs[0].NotAfter)
				intermediateCA, err := crypto.NewSigningCertificate(signerName, crypto.NewStaticKeyPairGenerator(ca.Key), crypto.WithSigner(oldSigner), crypto.WithClock(c.clock), crypto.WithLifetime(oldSignerCARemainingLifetime))
				if err != nil {
					return nil, fmt.Errorf("creating intermediate certificate for bridging trust between old and new signer certs: %w", err)
				}

				ca.Certs = append(ca.Certs, intermediateCA.Certs[0])

				// check for any cross-signed certificates for the old signer that are still valid
				// and carry them over. This allows multiple rotations within the original validity window
				// while still retaining intermediates that bridge trust across that entire chain.
				for _, cert := range oldSigner.Config.Certs {
					if cert.IsCA && bytes.Equal(cert.RawIssuer, cert.RawSubject) {
						if err := cert.CheckSignatureFrom(cert); err == nil {
							continue
						}
					}

					// already expired
					if cert.NotAfter.Before(c.clock.Now()) {
						continue
					}

					ca.Certs = append(ca.Certs, cert)
				}
			}
		}
	}

	certBytes := &bytes.Buffer{}
	keyBytes := &bytes.Buffer{}
	if err = ca.WriteCertConfig(certBytes, keyBytes); err != nil {
		return nil, err
	}

	if signingCertKeyPairSecret.Annotations == nil {
		signingCertKeyPairSecret.Annotations = map[string]string{}
	}
	if signingCertKeyPairSecret.Data == nil {
		signingCertKeyPairSecret.Data = map[string][]byte{}
	}
	signingCertKeyPairSecret.Data["tls.crt"] = certBytes.Bytes()
	signingCertKeyPairSecret.Data["tls.key"] = keyBytes.Bytes()
	return ca, nil
}

// setTLSAnnotationsOnSigningCertKeyPairSecret applies predefined TLS annotations to the given secret.
//
// This function does not perform nil checks on its parameters and assumes that the
// secret's Annotations field has already been initialized.
//
// These assumptions are safe because this function is only called after the secret
// has been initialized in setSigningCertKeyPairSecret.
func (c RotatedSigningCASecret) setTLSAnnotationsOnSigningCertKeyPairSecret(signingCertKeyPairSecret *corev1.Secret, ca *crypto.TLSCertificateConfig) {
	signingCertKeyPairSecret.Annotations[CertificateIssuer] = ca.Certs[0].Issuer.CommonName

	tlsAnnotations := c.additionalAnnotations
	tlsAnnotations.NotBefore = ca.Certs[0].NotBefore.Format(time.RFC3339)
	tlsAnnotations.NotAfter = ca.Certs[0].NotAfter.Format(time.RFC3339)
	tlsAnnotations.RefreshPeriod = c.refresh.String()
	tlsAnnotations.KeyAlgorithm = crypto.KeyAlgorithmForPrivateKey(ca.Key)
	_ = tlsAnnotations.EnsureTLSMetadataUpdate(&signingCertKeyPairSecret.ObjectMeta)
}
