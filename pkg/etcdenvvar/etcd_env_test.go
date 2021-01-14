package etcdenvvar

import (
	"reflect"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/crypto"
)

// TestWhitelistEtcdCipherSuites this test is intended to ensure the ciphers we support is explicitly understood overtime.
// As etcd minor versions increment golang versions will as well, so this test will need to be maintained against changes
// to the etcd whitelist[1] and or TLSProfileIntermediateType.
//[1] https://github.com/etcd-io/etcd/blob/release-3.4/pkg/tlsutil/cipher_suites.go
func TestWhitelistEtcdCipherSuites(t *testing.T) {
	tests := []struct {
		name    string
		ciphers []string
		want    []string
	}{
		{
			name:    "test TLS v1.2 (current supported runtime)",
			ciphers: crypto.OpenSSLToIANACipherSuites(configv1.TLSProfiles[configv1.TLSProfileIntermediateType].Ciphers),
			want: []string{
				"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", // https://ciphersuite.info/cs/TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256/
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",   // https://ciphersuite.info/cs/TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256/
				"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", // https://ciphersuite.info/cs/TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384/
				"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",   // https://ciphersuite.info/cs/TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384/
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := whitelistEtcdCipherSuites(test.ciphers)
			if !reflect.DeepEqual(test.want, got) {
				t.Errorf("want %v got %v", test.want, got)
			}
		})
	}
}
