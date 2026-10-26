package ceohelpers

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculateFailureThreshold(t *testing.T) {
	scenarios := []struct {
		name          string
		baseThreshold int
		quotaGiB      int32
		expected      int
	}{
		{
			name:          "default quota 8GB",
			baseThreshold: 20,
			quotaGiB:      8,
			expected:      20,
		},
		{
			name:          "zero quota defaults to 8GB",
			baseThreshold: 20,
			quotaGiB:      0,
			expected:      20,
		},
		{
			name:          "double quota 16GB",
			baseThreshold: 20,
			quotaGiB:      16,
			expected:      40,
		},
		{
			name:          "maximum quota 32GB",
			baseThreshold: 20,
			quotaGiB:      32,
			expected:      80,
		},
		{
			name:          "readiness probe base at 8GB",
			baseThreshold: 3,
			quotaGiB:      8,
			expected:      3,
		},
		{
			name:          "readiness probe at 16GB",
			baseThreshold: 3,
			quotaGiB:      16,
			expected:      6,
		},
		{
			name:          "readiness probe at 32GB",
			baseThreshold: 3,
			quotaGiB:      32,
			expected:      12,
		},
		{
			name:          "minimum quota 8GB with small base",
			baseThreshold: 1,
			quotaGiB:      8,
			expected:      1,
		},
		{
			name:          "rounding test - 12GB quota",
			baseThreshold: 20,
			quotaGiB:      12,
			expected:      30, // 20 * (12/8) = 30
		},
		{
			name:          "rounding test - 10GB quota",
			baseThreshold: 3,
			quotaGiB:      10,
			expected:      4, // 3 * (10/8) = 3.75, rounds to 4
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := calculateFailureThreshold(scenario.baseThreshold, scenario.quotaGiB)
			require.Equal(t, scenario.expected, result)
		})
	}
}

func TestCalculateFailureThresholdMinimum(t *testing.T) {
	// Ensure that the function always returns at least 1
	result := calculateFailureThreshold(0, 8)
	require.Equal(t, 1, result, "calculateFailureThreshold should return at least 1")
}

func TestRenderCurvePreferences(t *testing.T) {
	curves := "X25519MLKEM768,X25519,CurveP256"
	rendered, err := RenderTemplate("etcd/pod.gotpl.yaml", &PodSubstitutionTemplate{
		ListenAddress:       "0.0.0.0",
		LocalhostAddress:    "127.0.0.1",
		LogLevel:            "info",
		CipherSuites:        "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		CurvePreferences:    curves,
		EnableEtcdContainer: true,
		EnvVars:             []NameValue{{Name: "ETCD_CURVE_PREFERENCES", Value: curves}},
	})
	require.NoError(t, err)
	// grpc-proxy and readyz take an explicit flag. The etcd container itself
	// reads ETCD_CURVE_PREFERENCES from the environment.
	require.Equal(t, 2, strings.Count(rendered, "--listen-curve-preferences "+curves))
	require.Contains(t, rendered, "name: \"ETCD_CURVE_PREFERENCES\"")

	withoutCurves, err := RenderTemplate("etcd/pod.gotpl.yaml", &PodSubstitutionTemplate{
		ListenAddress:       "0.0.0.0",
		LocalhostAddress:    "127.0.0.1",
		LogLevel:            "info",
		CipherSuites:        "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		EnableEtcdContainer: true,
	})
	require.NoError(t, err)
	require.NotContains(t, withoutCurves, "curve-preferences")
}
