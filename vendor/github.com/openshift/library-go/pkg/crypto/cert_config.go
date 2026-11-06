package crypto

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/utils/clock"
)

// KeyPairGenerator generates a cryptographic key pair.
type KeyPairGenerator interface {
	GenerateKeyPair() (crypto.PublicKey, crypto.PrivateKey, error)
	Algorithm() string
}

func NewStaticKeyPairGenerator(privateKey crypto.PrivateKey) KeyPairGenerator {
	skpg := &StaticKeyPairGenerator{}

	switch pk := privateKey.(type) {
	case *rsa.PrivateKey:
		skpg.privateKey = pk
		skpg.publicKey = pk.Public()
	case *ecdsa.PrivateKey:
		skpg.privateKey = pk
		skpg.publicKey = pk.Public()
	default:
		panic(fmt.Sprintf("unsupported private key type %T", privateKey))
	}

	return skpg
}

type StaticKeyPairGenerator struct {
	privateKey crypto.PrivateKey
	publicKey  crypto.PublicKey
}

func (skpg *StaticKeyPairGenerator) GenerateKeyPair() (crypto.PublicKey, crypto.PrivateKey, error) {
	return skpg.publicKey, skpg.privateKey, nil
}

func (skpg *StaticKeyPairGenerator) Algorithm() string {
	return KeyAlgorithmForPrivateKey(skpg.privateKey)
}

// NewSigningCertificate creates a CA certificate.
// By default it creates a self-signed root CA. Use WithSigner to create an
// intermediate CA signed by a parent CA.
// The name parameter is used as the CommonName unless overridden with WithSubject.
// Optional: WithSigner, WithSubject, WithLifetime (defaults to DefaultCACertificateLifetimeDuration).
func NewSigningCertificate(name string, keyGen KeyPairGenerator, opts ...CertificateOption) (*TLSCertificateConfig, error) {
	o := &CertificateOptions{
		lifetime: DefaultCACertificateLifetimeDuration,
		clock:    clock.RealClock{},
	}
	for _, opt := range opts {
		opt(o)
	}

	subject := pkix.Name{CommonName: name}
	if o.subject != nil {
		subject = *o.subject
	}

	publicKey, privateKey, err := keyGen.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}
	subjectKeyId, err := SubjectKeyIDFromPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to compute subject key ID: %w", err)
	}

	if o.signer != nil {
		// Intermediate CA signed by the provided signer.
		authorityKeyId := o.signer.Config.Certs[0].SubjectKeyId
		template := newSigningCertificateTemplateForDuration(subject, o.lifetime, o.clock.Now, authorityKeyId, subjectKeyId)
		template.SignatureAlgorithm = 0
		template.KeyUsage = KeyUsageForPublicKey(publicKey) | x509.KeyUsageCertSign

		cert, err := o.signer.SignCertificate(template, publicKey)
		if err != nil {
			return nil, fmt.Errorf("failed to sign certificate: %w", err)
		}

		return &TLSCertificateConfig{
			Certs: append([]*x509.Certificate{cert}, o.signer.Config.Certs...),
			Key:   privateKey,
		}, nil
	}

	// Self-signed root CA. AuthorityKeyId and SubjectKeyId match.
	template := newSigningCertificateTemplateForDuration(subject, o.lifetime, o.clock.Now, subjectKeyId, subjectKeyId)
	template.SignatureAlgorithm = 0
	template.KeyUsage = KeyUsageForPublicKey(publicKey) | x509.KeyUsageCertSign

	cert, err := signCertificate(template, publicKey, template, privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign certificate: %w", err)
	}

	return &TLSCertificateConfig{
		Certs: []*x509.Certificate{cert},
		Key:   privateKey,
	}, nil
}

// NewServerCertificate creates a server/serving certificate signed by this CA.
// Optional: WithLifetime (defaults to DefaultCertificateLifetimeDuration), WithExtensions.
func (ca *CA) NewServerCertificate(hostnames sets.Set[string], keyGen KeyPairGenerator, opts ...CertificateOption) (*TLSCertificateConfig, error) {
	o := &CertificateOptions{
		lifetime: DefaultCertificateLifetimeDuration,
		clock:    clock.RealClock{},
	}
	for _, opt := range opts {
		opt(o)
	}

	publicKey, privateKey, err := keyGen.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}
	subjectKeyId, err := SubjectKeyIDFromPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to compute subject key ID: %w", err)
	}

	sortedHostnames := sets.List(hostnames)
	authorityKeyId := ca.Config.Certs[0].SubjectKeyId
	template := newServerCertificateTemplateForDuration(
		pkix.Name{CommonName: sortedHostnames[0]},
		sortedHostnames,
		o.lifetime,
		o.clock.Now,
		authorityKeyId,
		subjectKeyId,
	)
	// Let x509.CreateCertificate auto-detect the signature algorithm from the CA's key.
	template.SignatureAlgorithm = 0
	template.KeyUsage = KeyUsageForPublicKey(publicKey)

	for _, fn := range o.extensionFns {
		if err := fn(template); err != nil {
			return nil, fmt.Errorf("failed to apply certificate extension: %w", err)
		}
	}

	cert, err := ca.SignCertificate(template, publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign certificate: %w", err)
	}

	return &TLSCertificateConfig{
		Certs: append([]*x509.Certificate{cert}, ca.Config.Certs...),
		Key:   privateKey,
	}, nil
}

// NewClientCertificate creates a client certificate signed by this CA.
// Optional: WithLifetime (defaults to DefaultCertificateLifetimeDuration).
func (ca *CA) NewClientCertificate(u user.Info, keyGen KeyPairGenerator, opts ...CertificateOption) (*TLSCertificateConfig, error) {
	o := &CertificateOptions{
		lifetime: DefaultCertificateLifetimeDuration,
		clock:    clock.RealClock{},
	}
	for _, opt := range opts {
		opt(o)
	}

	publicKey, privateKey, err := keyGen.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}
	subjectKeyId, err := SubjectKeyIDFromPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to compute subject key ID: %w", err)
	}

	authorityKeyId := ca.Config.Certs[0].SubjectKeyId
	template := NewClientCertificateTemplateForDuration(UserToSubject(u), o.lifetime, o.clock.Now)
	template.AuthorityKeyId = authorityKeyId
	template.SubjectKeyId = subjectKeyId
	// Let x509.CreateCertificate auto-detect the signature algorithm from the CA's key.
	template.SignatureAlgorithm = 0
	template.KeyUsage = KeyUsageForPublicKey(publicKey)

	cert, err := ca.SignCertificate(template, publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign certificate: %w", err)
	}

	// client certificates do not need to contain the entire certificate chain
	// like serving certificates do. That being said, to support the cross-signing
	// behaviors we do need to include any intermediate certificates beyond
	// the root CA certificate for the client certificate.
	// Verifiers of client certificates should already have the root CA certificate for the old
	// signing certificate in their trust store.
	// "intermediate certificates" in this case are any non-self-signed CA certificates.

	certs := []*x509.Certificate{cert}

	for _, c := range ca.Config.Certs {
		if c.IsCA == true && c.CheckSignatureFrom(c) == nil {
			continue
		}

		certs = append(certs, c)
	}

	return &TLSCertificateConfig{
		Certs: certs,
		Key:   privateKey,
	}, nil
}

// NewPeerCertificate creates a peer certificate (both server and client auth)
// signed by this CA.
// Optional: WithLifetime (defaults to DefaultCertificateLifetimeDuration), WithExtensions.
func (ca *CA) NewPeerCertificate(hostnames sets.Set[string], u user.Info, keyGen KeyPairGenerator, opts ...CertificateOption) (*TLSCertificateConfig, error) {
	o := &CertificateOptions{
		lifetime: DefaultCertificateLifetimeDuration,
		clock:    clock.RealClock{},
	}
	for _, opt := range opts {
		opt(o)
	}

	publicKey, privateKey, err := keyGen.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}
	subjectKeyId, err := SubjectKeyIDFromPublicKey(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to compute subject key ID: %w", err)
	}

	sortedHostnames := sets.List(hostnames)
	authorityKeyId := ca.Config.Certs[0].SubjectKeyId

	// Start from a server certificate template for the hostnames.
	template := newServerCertificateTemplateForDuration(
		pkix.Name{CommonName: sortedHostnames[0]},
		sortedHostnames,
		o.lifetime,
		o.clock.Now,
		authorityKeyId,
		subjectKeyId,
	)
	// Let x509.CreateCertificate auto-detect the signature algorithm from the CA's key.
	template.SignatureAlgorithm = 0
	template.KeyUsage = KeyUsageForPublicKey(publicKey)

	// Set subject from user info for client authentication.
	template.Subject = UserToSubject(u)

	// Enable both server and client authentication.
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}

	for _, fn := range o.extensionFns {
		if err := fn(template); err != nil {
			return nil, fmt.Errorf("failed to apply certificate extension: %w", err)
		}
	}

	cert, err := ca.SignCertificate(template, publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign certificate: %w", err)
	}

	return &TLSCertificateConfig{
		Certs: append([]*x509.Certificate{cert}, ca.Config.Certs...),
		Key:   privateKey,
	}, nil
}
