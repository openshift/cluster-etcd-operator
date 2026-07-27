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
			name:           "empty observedConfig returns error",
			observedConfig: map[string]any{},
			expectErr:      true,
			errContains:    "no supported cipherSuites found",
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

func TestGetTLSMinVersion(t *testing.T) {
	testCases := []struct {
		name             string
		observedConfig   map[string]any
		expectErr        bool
		errContains      string
		expectMinVersion string
	}{
		{
			name: "populated observedConfig with TLS 1.2 returns TLS1.2",
			observedConfig: map[string]any{
				"servingInfo": map[string]any{
					"minTLSVersion": "VersionTLS12",
				},
			},
			expectMinVersion: "TLS1.2",
		},
		{
			name: "populated observedConfig with TLS 1.3 returns TLS1.3",
			observedConfig: map[string]any{
				"servingInfo": map[string]any{
					"minTLSVersion": "VersionTLS13",
				},
			},
			expectMinVersion: "TLS1.3",
		},
		{
			name:             "empty observedConfig falls back to TLS1.2",
			observedConfig:   map[string]any{},
			expectMinVersion: "TLS1.2",
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

			result, err := getTLSMinVersion(ctx)

			if tc.expectErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)

			minVersion, ok := result["ETCD_TLS_MIN_VERSION"]
			require.True(t, ok, "expected key ETCD_TLS_MIN_VERSION in result")
			assert.Equal(t, tc.expectMinVersion, minVersion)
		})
	}
}

func TestObservedConfigHasCipherSuites(t *testing.T) {
	testCases := []struct {
		name     string
		raw      []byte
		expected bool
	}{
		{
			name:     "nil raw returns false",
			raw:      nil,
			expected: false,
		},
		{
			name:     "empty raw returns false",
			raw:      []byte{},
			expected: false,
		},
		{
			name:     "empty object returns false",
			raw:      []byte("{}"),
			expected: false,
		},
		{
			name:     "servingInfo without cipherSuites returns false",
			raw:      []byte(`{"servingInfo": {"minTLSVersion": "VersionTLS12"}}`),
			expected: false,
		},
		{
			name:     "servingInfo with empty cipherSuites returns false",
			raw:      []byte(`{"servingInfo": {"cipherSuites": []}}`),
			expected: false,
		},
		{
			name:     "servingInfo with cipherSuites returns true",
			raw:      []byte(`{"servingInfo": {"cipherSuites": ["TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"]}}`),
			expected: true,
		},
		{
			name:     "invalid yaml returns false",
			raw:      []byte("not valid yaml: ["),
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, observedConfigHasCipherSuites(tc.raw))
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
