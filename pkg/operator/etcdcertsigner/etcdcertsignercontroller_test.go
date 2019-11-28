package etcdcertsigner

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	v1 "k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
)

type getCertArgs struct {
	caCert        []byte
	caKey         []byte
	podFQDN       string
	org           string
	peerHostNames []string
}

func Test_getCerts(t *testing.T) {
	caPEM, caPrivKeyPEM := generateCACert("etcd-signer")
	metriccaPEM, metriccaPrivKeyPEM := generateCACert("etcd-metric-signer")
	tests := []struct {
		name    string
		args    getCertArgs
		wantErr bool
	}{
		{
			name: "peer test",
			args: getCertArgs{
				caCert:        caPEM.Bytes(),
				caKey:         caPrivKeyPEM.Bytes(),
				podFQDN:       "etcd-0.trial.io",
				org:           "system:peers",
				peerHostNames: []string{"etcd-0.trial.io", "localhost", "*.etcd-0.trial.io", "127.0.0.1", "10.9.10.6"},
			},
		},
		{
			name: "server test",
			args: getCertArgs{
				caCert:        caPEM.Bytes(),
				caKey:         caPrivKeyPEM.Bytes(),
				podFQDN:       "etcd-0.trial.io",
				org:           "system:servers",
				peerHostNames: []string{"etcd-0.trial.io", "localhost", "*.etcd-0.trial.io", "127.0.0.1", "10.9.10.6"},
			},
		},
		{
			name: "metric test",
			args: getCertArgs{
				caCert:        metriccaPEM.Bytes(),
				caKey:         metriccaPrivKeyPEM.Bytes(),
				podFQDN:       "etcd-0.trial.io",
				org:           "system:metrics",
				peerHostNames: []string{"etcd-0.trial.io", "localhost", "*.etcd-0.trial.io", "127.0.0.1", "10.9.10.6"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := getCerts(tt.args.caCert, tt.args.caKey, tt.args.podFQDN, tt.args.org, tt.args.peerHostNames)
			if (err != nil) != tt.wantErr {
				t.Errorf("getCerts() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			err = verify(tt.args, got)
			if err != nil {
				t.Errorf("invalid certs %#v", err)
			}
		})
	}
}

func generateCACert(issuer string) (*bytes.Buffer, *bytes.Buffer) {
	serial, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64))
	if err != nil {
		return nil, nil
	}
	ca := &x509.Certificate{
		SerialNumber: serial,
		Issuer: pkix.Name{
			OrganizationalUnit: []string{"openshift"},
			CommonName:         issuer,
		},
		Subject: pkix.Name{
			OrganizationalUnit: []string{"openshift"},
			CommonName:         issuer,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil
	}

	caPEM := new(bytes.Buffer)
	pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})

	caPrivKeyPEM := new(bytes.Buffer)
	pem.Encode(caPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey),
	})
	return caPEM, caPrivKeyPEM
}

func verify(a getCertArgs, cert *bytes.Buffer) error {
	roots := x509.NewCertPool()
	ok := roots.AppendCertsFromPEM(a.caCert)
	if !ok {
		panic("failed to parse root certificate")
	}

	block, _ := pem.Decode(cert.Bytes())
	if block == nil {
		return errors.New("unable to decode the cert")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}

	if c.Issuer.OrganizationalUnit[0] != "openshift" {
		return errors.New("invalid OU")
	}

	if strings.Contains(a.org, "peer") || strings.Contains(a.org, "server") {
		if c.Issuer.CommonName != "etcd-signer" {
			return errors.New("invalid CN")
		}
	}
	if strings.Contains(a.org, "metric") {
		if c.Issuer.CommonName != "etcd-metric-signer" {
			return errors.New("invalid CN")
		}
	}

	if c.Subject.Organization[0] != a.org {
		return errors.New("invalid Subject O")
	}

	if c.Subject.CommonName != (strings.TrimSuffix(a.org, "s") + ":" + (a.podFQDN)) {
		return errors.New("invalid Subject CN")
	}

	var e error

	for _, hostname := range a.peerHostNames {
		opts := x509.VerifyOptions{
			DNSName:   hostname,
			Roots:     roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		}
		if _, err := c.Verify(opts); err != nil {
			e = err
		}
	}

	return e
}

func Test_getSecretName(t *testing.T) {
	type args struct {
		org     string
		podFQDN string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		// TODO: Add test cases.
		{
			name: "server test case",
			args: args{
				org:     serverOrg,
				podFQDN: "etcd-0.foo.bar",
			},
			want: "server-etcd-0.foo.bar",
		},
		{
			name: "peer test case",
			args: args{
				org:     peerOrg,
				podFQDN: "etcd-0.foo.bar",
			},
			want: "peer-etcd-0.foo.bar",
		},
		{
			name: "metric test case",
			args: args{
				org:     metricOrg,
				podFQDN: "etcd-0.foo.bar",
			},
			want: "metric-etcd-0.foo.bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getSecretName(tt.args.org, tt.args.podFQDN); got != tt.want {
				t.Errorf("getSecretName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func getTLSSecret(name, namespace, crt, key string) *v1.Secret {
	data := map[string][]byte{}
	if crt != "" {
		data["tls.crt"] = []byte(crt)
	}
	if key != "" {
		data["tls.key"] = []byte(key)
	}
	return &v1.Secret{
		ObjectMeta: v12.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
}

func TestEtcdCertSignerController_populateSecret(t *testing.T) {
	type fields struct {
		clientset kubernetes.Interface
	}
	type args struct {
		secretName      string
		secretNamespace string
		cert            *bytes.Buffer
		key             *bytes.Buffer
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
		secret  *v1.Secret
	}{
		// TODO: Add test cases.
		{
			name:   "test valid secret",
			fields: fields{clientset: fake.NewSimpleClientset(getTLSSecret("foo", "bar", "secure_crt_data", "secure_key_data"))},
			args: args{
				secretName:      "foo",
				secretNamespace: "bar",
				cert:            bytes.NewBufferString("secure_crt_data"),
				key:             bytes.NewBufferString("secure_key_data"),
			},
			wantErr: false,
		},
		{
			name:   "test invalid secret",
			fields: fields{clientset: fake.NewSimpleClientset(getTLSSecret("foo", "bar", "", ""))},
			args: args{
				secretName:      "foo",
				secretNamespace: "bar",
				cert:            bytes.NewBufferString("secure_crt_data"),
				key:             bytes.NewBufferString("secure_key_data"),
			},
			wantErr: false,
		},
		{
			name:   "test secret with valid certs",
			fields: fields{clientset: fake.NewSimpleClientset(getTLSSecret("foo", "bar", "secure_crt_data", "secure_key_data"))},
			args: args{
				secretName:      "foo",
				secretNamespace: "bar",
				cert:            bytes.NewBufferString("insecure_crt_data"),
				key:             bytes.NewBufferString("insecure_key_data"),
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &EtcdCertSignerController{
				clientset: tt.fields.clientset,
			}
			err := c.populateSecret(tt.args.secretName, tt.args.secretNamespace, tt.args.cert, tt.args.key)
			if !tt.wantErr {
				s, _ := tt.fields.clientset.CoreV1().Secrets(tt.args.secretNamespace).Get(tt.args.secretName, v12.GetOptions{})
				if tlsErr := ensureTLSData(s); tlsErr != nil {
					t.Errorf("populateSecret should always populate empty secrets")
				}
				if !bytes.Equal(s.Data["tls.crt"], []byte("secure_crt_data")) ||
					!bytes.Equal(s.Data["tls.key"], []byte("secure_key_data")) {
					t.Errorf("populateSecret should not update existing valid data")
				}
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("populateSecret() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
