package etcdenvvar

import (
	"strings"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestGetCipherSuites(t *testing.T) {
	testCases := []struct {
		name           string
		observedConfig map[string]any
		expectErr      bool
		errContains    string
		expectEnvKey   string
		expectCiphers  []string
	}{
		{
			name: "populated observedConfig returns expected ciphers",
			observedConfig: map[string]any{
				"servingInfo": map[string]any{
					"cipherSuites": []string{
						"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
						"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
						"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
					},
					"minTLSVersion": "VersionTLS12",
				},
			},
			expectEnvKey: "ETCD_CIPHER_SUITES",
			expectCiphers: []string{
				"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
		},
		{
			name:           "empty observedConfig falls back to TLSProfileIntermediateType defaults",
			observedConfig: map[string]any{},
			expectEnvKey:   "ETCD_CIPHER_SUITES",
			expectCiphers: []string{
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
				"TLS_CHACHA20_POLY1305_SHA256",
				"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
				"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
				"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
				"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
			},
		},
		{
			name: "observedConfig with only unsupported ciphers returns error",
			observedConfig: map[string]any{
				"servingInfo": map[string]any{
					"cipherSuites": []string{
						"TLS_UNSUPPORTED_CIPHER_1",
						"TLS_UNSUPPORTED_CIPHER_2",
					},
					"minTLSVersion": "VersionTLS12",
				},
			},
			expectErr:   true,
			errContains: "no supported cipherSuites found",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			observedConfigYaml, err := yaml.Marshal(tc.observedConfig)
			require.NoError(t, err)

			ctx := envVarContext{
				spec: operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ObservedConfig: runtime.RawExtension{Raw: observedConfigYaml},
					},
				},
			}

			result, err := getCipherSuites(ctx)

			if tc.expectErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)

			cipherValue, ok := result[tc.expectEnvKey]
			require.True(t, ok, "expected key %q in result", tc.expectEnvKey)

			actualCiphers := strings.Split(cipherValue, ",")
			assert.Equal(t, tc.expectCiphers, actualCiphers)
		})
	}
}

func TestConvertDBSize(t *testing.T) {
	testCases := []struct {
		name  string
		input int64
		exp   string
	}{
		{
			name:  "1 GB",
			input: 1,
			exp:   "1073741824",
		},
		{
			name:  "8 GB",
			input: 8,
			exp:   "8589934592",
		},
		{
			name:  "16 GB",
			input: 16,
			exp:   "17179869184",
		},
		{
			name:  "32 GB",
			input: 32,
			exp:   "34359738368",
		},
		{
			name:  "64 GB",
			input: 64,
			exp:   "68719476736",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.exp, GibibytesToBytesString(tc.input))
		})
	}
}
