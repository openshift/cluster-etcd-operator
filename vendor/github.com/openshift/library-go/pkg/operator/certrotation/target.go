package certrotation

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehelper"
	"github.com/openshift/library-go/pkg/pki"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// RotatedSelfSignedCertKeySecret rotates a key and cert signed by a signing CA and stores it in a secret.
//
// It creates a new one when
// - refresh duration is over
// - or 80% of validity is over (if RefreshOnlyWhenExpired is false)
// - or the cert is expired.
// - or the signing CA changes.
type RotatedSelfSignedCertKeySecret struct {
	// Namespace is the namespace of the Secret.
	namespace string
	// Name is the name of the Secret.
	name string
	// Validity is the duration from time.Now() until the certificate expires. If RefreshOnlyWhenExpired
	// is false, the key and certificate is rotated when 80% of validity is reached.
	validity time.Duration
	// Refresh is the duration after certificate creation when it is rotated at the latest. It is ignored
	// if RefreshOnlyWhenExpired is true, or if Refresh > Validity.
	// Refresh is ignored until the signing CA at least 10% in its life-time to ensure it is deployed
	// through-out the cluster.
	refresh time.Duration
	// RefreshOnlyWhenExpired allows rotating only certs that are already expired. (for autorecovery)
	// If false (regular flow) it rotates at the refresh interval but no later then 4/5 of the cert lifetime.

	// RefreshOnlyWhenExpired set to true means to ignore 80% of validity and the Refresh duration for rotation,
	// but only rotate when the certificate expires. This is useful for auto-recovery when we want to enforce
	// rotation on expiration only, but not interfere with the ordinary rotation controller.
	refreshOnlyWhenExpired bool

	// Owner is an optional reference to add to the secret that this rotator creates. Use this when downstream
	// consumers of the certificate need to be aware of changes to the object.
	// WARNING: be careful when using this option, as deletion of the owning object will cascade into deletion
	// of the certificate. If the lifetime of the owning object is not a superset of the lifetime in which the
	// certificate is used, early deletion will be catastrophic.
	owner *metav1.OwnerReference

	// AdditionalAnnotations is a collection of annotations set for the secret
	additionalAnnotations AdditionalAnnotations

	// CertCreator does the actual cert generation.
	certCreator TargetCertCreator

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

func NewRotatedSelfSignedCertKeySecret(
	namespace string,
	name string,
	certificateName string,
	validity time.Duration,
	refresh time.Duration,
	refreshOnlyWhenExpired bool,
	owner *metav1.OwnerReference,
	additionalAnnotations AdditionalAnnotations,
	certCreator TargetCertCreator,
	pkiProfileProvider pki.PKIProfileProvider,
	informer corev1informers.SecretInformer,
	client corev1client.SecretsGetter,
	eventRecorder events.Recorder,
) RotatedSelfSignedCertKeySecret {
	return RotatedSelfSignedCertKeySecret{
		name:                   name,
		namespace:              namespace,
		certificateName:        certificateName,
		validity:               validity,
		refresh:                refresh,
		refreshOnlyWhenExpired: refreshOnlyWhenExpired,
		owner:                  owner,
		additionalAnnotations:  additionalAnnotations,
		certCreator:            certCreator,
		pkiProfileProvider:     pkiProfileProvider,
		informer:               informer,
		lister:                 informer.Lister(),
		client:                 client,
		eventRecorder:          eventRecorder,
		clock:                  clock.RealClock{},
	}
}

type TargetCertCreator interface {
	// NewCertificate creates a new key-cert pair with the given signer. If keyGen
	// is non-nil, it is used to generate the key pair; otherwise legacy defaults are used.
	NewCertificate(signer *crypto.CA, validity time.Duration, keyGen crypto.KeyPairGenerator) (*crypto.TLSCertificateConfig, error)
	// NeedNewTargetCertKeyPair decides whether a new cert-key pair is needed. It returns a non-empty reason if it is the case.
	NeedNewTargetCertKeyPair(currentCertSecret *corev1.Secret, signer *crypto.CA, caBundleCerts []*x509.Certificate, refresh time.Duration, refreshOnlyWhenExpired, creationRequired bool, keyGen crypto.KeyPairGenerator) string
	// SetAnnotations gives an option to override or set additional annotations
	SetAnnotations(cert *crypto.TLSCertificateConfig, annotations map[string]string) map[string]string
	// CertificateType returns the category of certificate this creator produces.
	CertificateType() pki.CertificateType
}

// TargetCertRechecker is an optional interface to be implemented by the TargetCertCreator to enforce
// a controller run.
type TargetCertRechecker interface {
	RecheckChannel() <-chan struct{}
}

func (c RotatedSelfSignedCertKeySecret) EnsureTargetCertKeyPair(ctx context.Context, signingCertKeyPair *crypto.CA, caBundleCerts []*x509.Certificate) (*corev1.Secret, error) {
	// at this point our trust bundle has been updated.  We don't know for sure that consumers have updated, but that's why we have a second
	// validity percentage.  We always check to see if we need to sign.  Often we are signing with an old key or we have no target
	// and need to mint one

	creationRequired := false
	updateRequired := false
	originalTargetCertKeyPairSecret, err := c.lister.Secrets(c.namespace).Get(c.name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	targetCertKeyPairSecret := originalTargetCertKeyPairSecret.DeepCopy()
	if apierrors.IsNotFound(err) {
		// create an empty one
		targetCertKeyPairSecret = &corev1.Secret{
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
		needsMetadataUpdate := ensureOwnerRefAndTLSAnnotations(targetCertKeyPairSecret, c.owner, c.additionalAnnotations)
		needsTypeChange := ensureSecretTLSTypeSet(targetCertKeyPairSecret)
		updateRequired = needsMetadataUpdate || needsTypeChange
	}

	var keyGen crypto.KeyPairGenerator
	if c.pkiProfileProvider != nil {
		var err error
		keyGen, err = c.resolveKeyPairGenerator()
		if err != nil {
			return nil, err
		}
	}

	if reason := c.certCreator.NeedNewTargetCertKeyPair(targetCertKeyPairSecret, signingCertKeyPair, caBundleCerts, c.refresh, c.refreshOnlyWhenExpired, creationRequired, keyGen); len(reason) > 0 {
		c.eventRecorder.Eventf("TargetUpdateRequired", "%q in %q requires a new target cert/key pair: %v", c.name, c.namespace, reason)
		if err = c.setTargetCertKeyPairSecretAndTLSAnnotations(targetCertKeyPairSecret, signingCertKeyPair, keyGen); err != nil {
			return nil, err
		}

		LabelAsManagedSecret(targetCertKeyPairSecret, CertificateTypeTarget)

		updateRequired = true
	}
	if creationRequired {
		actualTargetCertKeyPairSecret, err := c.client.Secrets(c.namespace).Create(ctx, targetCertKeyPairSecret, metav1.CreateOptions{})
		resourcehelper.ReportCreateEvent(c.eventRecorder, actualTargetCertKeyPairSecret, err)
		if err != nil {
			return nil, err
		}
		klog.V(2).Infof("Created secret %s/%s", actualTargetCertKeyPairSecret.Namespace, actualTargetCertKeyPairSecret.Name)
		targetCertKeyPairSecret = actualTargetCertKeyPairSecret
	} else if updateRequired {
		actualTargetCertKeyPairSecret, err := c.client.Secrets(c.namespace).Update(ctx, targetCertKeyPairSecret, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			// ignore error if its attempting to update outdated version of the secret
			return nil, nil
		}
		resourcehelper.ReportUpdateEvent(c.eventRecorder, actualTargetCertKeyPairSecret, err)
		if err != nil {
			return nil, err
		}
		klog.V(2).Infof("Updated secret %s/%s", actualTargetCertKeyPairSecret.Namespace, actualTargetCertKeyPairSecret.Name)
		targetCertKeyPairSecret = actualTargetCertKeyPairSecret
	}

	return targetCertKeyPairSecret, nil
}

func needNewTargetCertKeyPair(secret *corev1.Secret, signer *crypto.CA, caBundleCerts []*x509.Certificate, refresh time.Duration, refreshOnlyWhenExpired, creationRequired bool, timeClock clock.PassiveClock, keyGen crypto.KeyPairGenerator) string {
	if creationRequired {
		return "secret doesn't exist"
	}

	annotations := secret.Annotations
	if reason := needNewTargetCertKeyPairForTime(annotations, signer, refresh, refreshOnlyWhenExpired, timeClock); len(reason) > 0 {
		return reason
	}

	// Exit early if we're only refreshing when expired and the certificate does not need an update
	if refreshOnlyWhenExpired {
		return ""
	}

	// check the signer common name against all the common names in our ca bundle so we don't refresh early
	signerCommonName := annotations[CertificateIssuer]
	if len(signerCommonName) == 0 {
		return "missing issuer name"
	}

	if signerCommonName != signer.Config.Certs[0].Subject.CommonName {
		return fmt.Sprintf("issuer %q does not match desired signer subject %q", signerCommonName, signer.Config.Certs[0].Subject.CommonName)
	}

	if keyGen != nil {
		if keyGen.Algorithm() != annotations[CertificateKeyAlgorithm] {
			return fmt.Sprintf("key algorithm changed from %q to %q", annotations[CertificateKeyAlgorithm], keyGen.Algorithm())
		}
	}

	return ""
}

// needNewTargetCertKeyPairForTime returns true when
//  1. when notAfter or notBefore is missing in the annotation
//  2. when notAfter or notBefore is malformed
//  3. when now is after the notAfter
//  4. when now is after notAfter+refresh AND the signer has been valid
//     for more than 5% of the "extra" time we renew the target
//
// in other words, we rotate if
//
// our old CA is gone from the bundle (then we are pretty late to the renewal party)
// or the cert expired (then we are also pretty late)
// or we are over the renewal percentage of the validity, but only if the new CA at least 10% into its age.
// Maybe worth a go doc.
//
// So in general we need to see a signing CA at least aged 10% within 1-percentage of the cert validity.
//
// Hence, if the CAs are rotated too fast (like CA percentage around 10% or smaller), we will not hit the time to make use of the CA. Or if the cert renewal percentage is at 90%, there is not much time either.
//
// So with a cert percentage of 75% and equally long CA and cert validities at the worst case we start at 85% of the cert to renew, trying again every minute.
func needNewTargetCertKeyPairForTime(annotations map[string]string, signer *crypto.CA, refresh time.Duration, refreshOnlyWhenExpired bool, timeClock clock.PassiveClock) string {
	notBefore, notAfter, reason := getValidityFromAnnotations(annotations)
	if len(reason) > 0 {
		return reason
	}

	// Is cert expired?
	if timeClock.Now().After(notAfter) {
		return "already expired"
	}

	if refreshOnlyWhenExpired {
		return ""
	}

	// Are we at 80% of validity?
	validity := notAfter.Sub(notBefore)
	at80Percent := notAfter.Add(-validity / 5)
	if timeClock.Now().After(at80Percent) {
		return fmt.Sprintf("past refresh time (80%% of validity): %v", at80Percent)
	}

	// If Certificate is past its refresh time, we may have action to take. We only do this if the signer is old enough.
	refreshTime := notBefore.Add(refresh)
	if timeClock.Now().After(refreshTime) {
		// With cross-signing, we no longer need to wait for trust rotation before rotating, so we can rotate immediately upon hitting the refresh time.
		return fmt.Sprintf("past its refresh time %v", refreshTime)
	}

	return ""
}

// setTargetCertKeyPairSecretAndTLSAnnotations generates a new cert/key pair,
// stores them in the specified secret, and adds predefined TLS annotations to that secret.
func (c RotatedSelfSignedCertKeySecret) setTargetCertKeyPairSecretAndTLSAnnotations(targetCertKeyPairSecret *corev1.Secret, signer *crypto.CA, keyGen crypto.KeyPairGenerator) error {
	certKeyPair, err := c.setTargetCertKeyPairSecret(targetCertKeyPairSecret, signer, keyGen)
	if err != nil {
		return err
	}

	c.setTLSAnnotationsOnTargetCertKeyPairSecret(targetCertKeyPairSecret, certKeyPair)
	return nil
}

// setTargetCertKeyPairSecret creates a new cert/key pair and sets them in the secret.  Only one of client, serving, or signer rotation may be specified.
// TODO refactor with an interface for actually signing and move the one-of check higher in the stack.
func (c RotatedSelfSignedCertKeySecret) setTargetCertKeyPairSecret(targetCertKeyPairSecret *corev1.Secret, signer *crypto.CA, keyGen crypto.KeyPairGenerator) (*crypto.TLSCertificateConfig, error) {
	if targetCertKeyPairSecret.Annotations == nil {
		targetCertKeyPairSecret.Annotations = map[string]string{}
	}
	if targetCertKeyPairSecret.Data == nil {
		targetCertKeyPairSecret.Data = map[string][]byte{}
	}

	// our annotation is based on our cert validity, so we want to make sure that we don't specify something past our signer
	targetValidity := c.validity
	remainingSignerValidity := signer.Config.Certs[0].NotAfter.Sub(c.clock.Now())
	if remainingSignerValidity < targetValidity {
		targetValidity = remainingSignerValidity
	}

	certKeyPair, err := c.certCreator.NewCertificate(signer, targetValidity, keyGen)
	if err != nil {
		return nil, err
	}

	targetCertKeyPairSecret.Data["tls.crt"], targetCertKeyPairSecret.Data["tls.key"], err = certKeyPair.GetPEMBytes()
	return certKeyPair, err
}

// setTLSAnnotationsOnTargetCertKeyPairSecret applies predefined TLS annotations to the given secret.
//
// This function does not perform nil checks on its parameters and assumes that the
// secret's Annotations field has already been initialized.
//
// These assumptions are safe because this function is only called after the secret
// has been initialized in setTargetCertKeyPairSecret.
func (c RotatedSelfSignedCertKeySecret) setTLSAnnotationsOnTargetCertKeyPairSecret(targetCertKeyPairSecret *corev1.Secret, certKeyPair *crypto.TLSCertificateConfig) {
	targetCertKeyPairSecret.Annotations[CertificateIssuer] = certKeyPair.Certs[0].Issuer.CommonName

	tlsAnnotations := c.additionalAnnotations
	tlsAnnotations.NotBefore = certKeyPair.Certs[0].NotBefore.Format(time.RFC3339)
	tlsAnnotations.NotAfter = certKeyPair.Certs[0].NotAfter.Format(time.RFC3339)
	tlsAnnotations.RefreshPeriod = c.refresh.String()
	tlsAnnotations.KeyAlgorithm = crypto.KeyAlgorithmForPrivateKey(certKeyPair.Key)
	_ = tlsAnnotations.EnsureTLSMetadataUpdate(&targetCertKeyPairSecret.ObjectMeta)

	c.certCreator.SetAnnotations(certKeyPair, targetCertKeyPairSecret.Annotations)
}

func (c RotatedSelfSignedCertKeySecret) resolveKeyPairGenerator() (crypto.KeyPairGenerator, error) {
	return resolveKeyPairGeneratorWithFallback(c.pkiProfileProvider, c.certCreator.CertificateType(), c.certificateName)
}

type ClientRotation struct {
	userInfo user.Info
	clock    clock.PassiveClock
}

func NewClientRotation(userInfo user.Info) *ClientRotation {
	return &ClientRotation{
		userInfo: userInfo,
		clock:    clock.RealClock{},
	}
}

func (r *ClientRotation) CertificateType() pki.CertificateType {
	return pki.CertificateTypeClient
}

func (r *ClientRotation) NewCertificate(signer *crypto.CA, validity time.Duration, keyGen crypto.KeyPairGenerator) (*crypto.TLSCertificateConfig, error) {
	var keyPairGenerator crypto.KeyPairGenerator = crypto.RSAKeyPairGenerator{Bits: 2048}
	if keyGen != nil {
		keyPairGenerator = keyGen
	}

	return signer.NewClientCertificate(r.userInfo, keyPairGenerator, crypto.WithLifetime(validity), crypto.WithClock(r.clock))
}

func (r *ClientRotation) NeedNewTargetCertKeyPair(currentCertSecret *corev1.Secret, signer *crypto.CA, caBundleCerts []*x509.Certificate, refresh time.Duration, refreshOnlyWhenExpired, exists bool, keyGen crypto.KeyPairGenerator) string {
	return needNewTargetCertKeyPair(currentCertSecret, signer, caBundleCerts, refresh, refreshOnlyWhenExpired, exists, r.clock, keyGen)
}

func (r *ClientRotation) SetAnnotations(cert *crypto.TLSCertificateConfig, annotations map[string]string) map[string]string {
	return annotations
}

type ServingRotation struct {
	hostnames              ServingHostnameFunc
	certificateExtensionFn []crypto.CertificateExtensionFunc
	hostnamesChanged       <-chan struct{}
	clock                  clock.PassiveClock
}

func NewServingRotation(hostnames ServingHostnameFunc, extensionFns []crypto.CertificateExtensionFunc, hostnamesChangedChannel <-chan struct{}) *ServingRotation {
	return &ServingRotation{
		hostnames:              hostnames,
		certificateExtensionFn: extensionFns,
		hostnamesChanged:       hostnamesChangedChannel,
		clock:                  clock.RealClock{},
	}
}

func (r *ServingRotation) CertificateType() pki.CertificateType {
	return pki.CertificateTypeServing
}

func (r *ServingRotation) NewCertificate(signer *crypto.CA, validity time.Duration, keyGen crypto.KeyPairGenerator) (*crypto.TLSCertificateConfig, error) {
	hostnames := r.hostnames()
	if len(hostnames) == 0 {
		return nil, fmt.Errorf("no hostnames set")
	}

	var keyPairGenerator crypto.KeyPairGenerator = crypto.RSAKeyPairGenerator{Bits: 2048}
	if keyGen != nil {
		keyPairGenerator = keyGen
	}

	return signer.NewServerCertificate(sets.New(hostnames...), keyPairGenerator,
		crypto.WithLifetime(validity),
		crypto.WithExtensions(r.certificateExtensionFn...),
		crypto.WithClock(r.clock),
	)
}

func (r *ServingRotation) RecheckChannel() <-chan struct{} {
	return r.hostnamesChanged
}

func (r *ServingRotation) NeedNewTargetCertKeyPair(currentCertSecret *corev1.Secret, signer *crypto.CA, caBundleCerts []*x509.Certificate, refresh time.Duration, refreshOnlyWhenExpired, creationRequired bool, keyGen crypto.KeyPairGenerator) string {
	reason := needNewTargetCertKeyPair(currentCertSecret, signer, caBundleCerts, refresh, refreshOnlyWhenExpired, creationRequired, r.clock, keyGen)
	if len(reason) > 0 {
		return reason
	}

	return r.missingHostnames(currentCertSecret.Annotations)
}

func (r *ServingRotation) missingHostnames(annotations map[string]string) string {
	return missingHostnames(annotations, r.hostnames())
}

func (r *ServingRotation) SetAnnotations(cert *crypto.TLSCertificateConfig, annotations map[string]string) map[string]string {
	return setHostnameAnnotations(cert, annotations)
}

func missingHostnames(annotations map[string]string, hostnames []string) string {
	existingHostnames := sets.New(strings.Split(annotations[CertificateHostnames], ",")...)
	requiredHostnames := sets.New(hostnames...)
	if !existingHostnames.Equal(requiredHostnames) {
		existingNotRequired := existingHostnames.Difference(requiredHostnames)
		requiredNotExisting := requiredHostnames.Difference(existingHostnames)
		return fmt.Sprintf("%q are existing and not required, %q are required and not existing", strings.Join(sets.List(existingNotRequired), ","), strings.Join(sets.List(requiredNotExisting), ","))
	}
	return ""
}

func setHostnameAnnotations(cert *crypto.TLSCertificateConfig, annotations map[string]string) map[string]string {
	hostnames := sets.Set[string]{}
	for _, ip := range cert.Certs[0].IPAddresses {
		hostnames.Insert(ip.String())
	}
	for _, dnsName := range cert.Certs[0].DNSNames {
		hostnames.Insert(dnsName)
	}
	// List does a sort so that we have a consistent representation
	annotations[CertificateHostnames] = strings.Join(sets.List(hostnames), ",")
	return annotations
}

type ServingHostnameFunc func() []string

type SignerRotation struct {
	signerName string
	clock      clock.PassiveClock
}

func NewSignerRotation(signerName string) *SignerRotation {
	return &SignerRotation{
		signerName: signerName,
		clock:      clock.RealClock{},
	}
}

func (r *SignerRotation) CertificateType() pki.CertificateType {
	return pki.CertificateTypeSigner
}

func (r *SignerRotation) NewCertificate(signer *crypto.CA, validity time.Duration, keyGen crypto.KeyPairGenerator) (*crypto.TLSCertificateConfig, error) {
	signerName := fmt.Sprintf("%s_@%d", r.signerName, r.clock.Now().Unix())

	var keyPairGenerator crypto.KeyPairGenerator = crypto.RSAKeyPairGenerator{Bits: 2048}
	if keyGen != nil {
		keyPairGenerator = keyGen
	}

	return crypto.NewSigningCertificate(signerName, keyPairGenerator,
		crypto.WithSigner(signer),
		crypto.WithLifetime(validity),
		crypto.WithClock(r.clock),
	)
}

// PeerRotation creates certificates used for both server and client authentication
// (e.g., etcd peer certificates). It uses CertificateTypePeer for PKI profile
// resolution, which selects the stronger of the serving and client key configurations.
//
// UserInfo is always required. When keyGen is non-nil (ConfigurablePKI enabled),
// NewPeerCertificate encodes UserInfo as the client identity in the certificate
// subject. When keyGen is nil (legacy), UserInfo.Name must match the sorted first
// hostname (used as CN by MakeServerCertForDuration), and the certificate falls
// back to MakeServerCertForDuration with an extension function that sets both
// ClientAuth and ServerAuth ExtKeyUsages.
type PeerRotation struct {
	hostnames              ServingHostnameFunc
	userInfo               user.Info
	certificateExtensionFn []crypto.CertificateExtensionFunc
	hostnamesChanged       <-chan struct{}
	clock                  clock.PassiveClock
}

func NewPeerRotation(hostnames ServingHostnameFunc, extensionFns []crypto.CertificateExtensionFunc, hostnamesChangedChannel <-chan struct{}, userInfo user.Info) *PeerRotation {
	return &PeerRotation{
		hostnames:              hostnames,
		certificateExtensionFn: extensionFns,
		hostnamesChanged:       hostnamesChangedChannel,
		userInfo:               userInfo,
		clock:                  clock.RealClock{},
	}
}

func (r *PeerRotation) CertificateType() pki.CertificateType {
	return pki.CertificateTypePeer
}

func (r *PeerRotation) NewCertificate(signer *crypto.CA, validity time.Duration, keyGen crypto.KeyPairGenerator) (*crypto.TLSCertificateConfig, error) {
	hostnames := r.hostnames()
	if len(hostnames) == 0 {
		return nil, fmt.Errorf("no hostnames set")
	}
	if r.userInfo == nil {
		return nil, fmt.Errorf("PeerRotation requires UserInfo")
	}

	var keyPairGenerator crypto.KeyPairGenerator = crypto.RSAKeyPairGenerator{Bits: 2048}
	if keyGen != nil {
		keyPairGenerator = keyGen
	}

	return signer.NewPeerCertificate(
		sets.New(hostnames...), r.userInfo, keyPairGenerator,
		crypto.WithLifetime(validity),
		crypto.WithExtensions(r.certificateExtensionFn...),
		crypto.WithClock(r.clock),
	)
}

func (r *PeerRotation) RecheckChannel() <-chan struct{} {
	return r.hostnamesChanged
}

func (r *PeerRotation) NeedNewTargetCertKeyPair(currentCertSecret *corev1.Secret, signer *crypto.CA, caBundleCerts []*x509.Certificate, refresh time.Duration, refreshOnlyWhenExpired, creationRequired bool, keyGen crypto.KeyPairGenerator) string {
	reason := needNewTargetCertKeyPair(currentCertSecret, signer, caBundleCerts, refresh, refreshOnlyWhenExpired, creationRequired, r.clock, keyGen)
	if len(reason) > 0 {
		return reason
	}
	return missingHostnames(currentCertSecret.Annotations, r.hostnames())
}

func (r *PeerRotation) SetAnnotations(cert *crypto.TLSCertificateConfig, annotations map[string]string) map[string]string {
	return setHostnameAnnotations(cert, annotations)
}

func (r *SignerRotation) NeedNewTargetCertKeyPair(currentCertSecret *corev1.Secret, signer *crypto.CA, caBundleCerts []*x509.Certificate, refresh time.Duration, refreshOnlyWhenExpired, exists bool, keyGen crypto.KeyPairGenerator) string {
	return needNewTargetCertKeyPair(currentCertSecret, signer, caBundleCerts, refresh, refreshOnlyWhenExpired, exists, r.clock, keyGen)
}

func (r *SignerRotation) SetAnnotations(cert *crypto.TLSCertificateConfig, annotations map[string]string) map[string]string {
	return annotations
}
