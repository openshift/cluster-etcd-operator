package etcdenvvar

import (
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/crypto"
	"go.etcd.io/etcd/client/pkg/v3/tlsutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime"
)

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

// TestGetCipherSuites covers the OCPBUGS-94106 bootstrap fallback: during
// bootstrap (revision 0) an empty observedConfig must not degrade the operator;
// instead getCipherSuites falls back to the same TLSProfileIntermediateType
// ciphers the render path bakes into the bootstrap etcd member. Past bootstrap
// (revision > 0) an empty observedConfig is still a genuine failure.
func TestGetCipherSuites(t *testing.T) {
	intermediate := tlshelpers.SupportedEtcdCiphers(
		crypto.OpenSSLToIANACipherSuites(configv1.TLSProfiles[configv1.TLSProfileIntermediateType].Ciphers),
	)
	require.NotEmpty(t, intermediate, "intermediate profile must yield etcd-supported ciphers")

	testCases := []struct {
		name           string
		observedConfig []byte
		revision       int32
		wantErr        bool
		wantCiphers    []string
	}{
		{
			name:           "bootstrap revision 0 with nil observedConfig falls back to intermediate ciphers",
			observedConfig: nil,
			revision:       0,
			wantCiphers:    intermediate,
		},
		{
			name:           "bootstrap revision 0 with empty observedConfig falls back to intermediate ciphers",
			observedConfig: []byte("{}"),
			revision:       0,
			wantCiphers:    intermediate,
		},
		{
			name:           "post-bootstrap revision >0 with empty observedConfig errors",
			observedConfig: []byte("{}"),
			revision:       3,
			wantErr:        true,
		},
		{
			name:           "observed cipherSuites are used verbatim when present",
			observedConfig: []byte(`{"servingInfo":{"cipherSuites":["TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"],"minTLSVersion":"VersionTLS12"}}`),
			revision:       3,
			wantCiphers:    []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := envVarContext{
				spec: operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ObservedConfig: runtime.RawExtension{Raw: tc.observedConfig},
					},
				},
				status: operatorv1.StaticPodOperatorStatus{
					OperatorStatus: operatorv1.OperatorStatus{
						LatestAvailableRevision: tc.revision,
					},
				},
			}

			got, err := getCipherSuites(ctx)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, strings.Join(tc.wantCiphers, ","), got["ETCD_CIPHER_SUITES"])
		})
	}
}

// TestGetObservedTLSMinVersionEmptyObservedConfig is a regression guard: an
// empty observedConfig (e.g. during bootstrap) must not error - crypto.TLSVersion("")
// returns the default (TLS 1.2), matching the bootstrap etcd member.
func TestGetObservedTLSMinVersionEmptyObservedConfig(t *testing.T) {
	ctx := envVarContext{
		spec: operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ObservedConfig: runtime.RawExtension{Raw: nil},
			},
		},
	}
	v, err := getObservedTLSMinVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, tlsutil.TLSVersion12, v)
}
