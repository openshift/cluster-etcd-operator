package etcdenvvar

import (
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"go.etcd.io/etcd/client/pkg/v3/tlsutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// bootstrapConfigMapLister returns a ConfigMapLister containing (or omitting)
// the kube-system/bootstrap configmap that bootstrap.IsBootstrapComplete reads.
func bootstrapConfigMapLister(complete bool) corev1listers.ConfigMapLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if complete {
		_ = indexer.Add(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "bootstrap"},
			Data:       map[string]string{"status": "complete"},
		})
	}
	return corev1listers.NewConfigMapLister(indexer)
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

// TestGetCipherSuites covers the OCPBUGS-94106 bootstrap fallback: while bootstrap
// is in progress an empty observedConfig falls back to the render path's
// TLSProfileIntermediateType ciphers instead of degrading; once bootstrap is
// complete an empty TLS 1.2 or older configuration is a genuine failure.
func TestGetCipherSuites(t *testing.T) {
	intermediate := tlshelpers.SupportedEtcdCiphers(
		crypto.OpenSSLToIANACipherSuites(configv1.TLSProfiles[configv1.TLSProfileIntermediateType].Ciphers),
	)
	require.NotEmpty(t, intermediate, "intermediate profile must yield etcd-supported ciphers")

	testCases := []struct {
		name              string
		observedConfig    []byte
		bootstrapComplete bool
		wantErr           bool
		wantCiphers       []string
		// wantInformationalCiphers, when set, asserts that ETCD_CIPHER_SUITES is not
		// emitted (etcd rejects cipher suites under TLS 1.3) and that the informational
		// CIPHER_SUITES env var carries the expected values instead.
		wantInformationalCiphers []string
	}{
		{
			name:           "bootstrap in progress with nil observedConfig falls back to intermediate ciphers",
			observedConfig: nil,
			wantCiphers:    intermediate,
		},
		{
			name:           "bootstrap in progress with empty observedConfig falls back to intermediate ciphers",
			observedConfig: []byte("{}"),
			wantCiphers:    intermediate,
		},
		{
			name:              "bootstrap complete with empty observedConfig errors",
			observedConfig:    []byte("{}"),
			bootstrapComplete: true,
			wantErr:           true,
		},
		{
			name:                     "bootstrap complete with TLS 1.3 and empty cipherSuites emits an empty informational env var",
			observedConfig:           []byte(`{"servingInfo":{"cipherSuites":[],"minTLSVersion":"VersionTLS13"}}`),
			bootstrapComplete:        true,
			wantInformationalCiphers: []string{},
		},
		{
			name:              "bootstrap complete with TLS 1.2 and empty cipherSuites errors",
			observedConfig:    []byte(`{"servingInfo":{"cipherSuites":[],"minTLSVersion":"VersionTLS12"}}`),
			bootstrapComplete: true,
			wantErr:           true,
		},
		{
			name:              "observed cipherSuites are used verbatim when present",
			observedConfig:    []byte(`{"servingInfo":{"cipherSuites":["TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"],"minTLSVersion":"VersionTLS12"}}`),
			bootstrapComplete: true,
			wantCiphers:       []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
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
				bootstrapConfigMapLister: bootstrapConfigMapLister(tc.bootstrapComplete),
			}

			got, err := getCipherSuites(ctx)
			if tc.wantErr {
				require.Error(t, err)
				// guard the symptom-matched error string (EtcdBootstrapRev0CipherSuitesOCPBUGS94106)
				assert.Contains(t, err.Error(), "no supported cipherSuites not found in observedConfig")
				return
			}
			require.NoError(t, err)
			if tc.wantInformationalCiphers != nil {
				assert.NotContains(t, got, "ETCD_CIPHER_SUITES")
				assert.Equal(t, strings.Join(tc.wantInformationalCiphers, ","), got["CIPHER_SUITES"])
				return
			}
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

func TestGetCurvePreferences(t *testing.T) {
	enabled := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences}, nil)
	disabled := featuregates.NewHardcodedFeatureGateAccess(
		nil, []configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences})
	intermediate := tlshelpers.SupportedEtcdCurves(intermediateTLSGroupNames())
	require.NotEmpty(t, intermediate)

	testCases := []struct {
		name              string
		accessor          featuregates.FeatureGateAccess
		observedConfig    []byte
		bootstrapComplete bool
		wantErr           string
		wantCurves        []string
	}{
		{
			name:           "feature gate disabled leaves etcd curve selection alone",
			accessor:       disabled,
			observedConfig: []byte(`{"servingInfo":{"groups":["X25519"]}}`),
		},
		{
			name:           "bootstrap in progress falls back to intermediate groups",
			accessor:       enabled,
			observedConfig: []byte(`{}`),
			wantCurves:     intermediate,
		},
		{
			name:              "bootstrap complete without groups is an error",
			accessor:          enabled,
			observedConfig:    []byte(`{}`),
			bootstrapComplete: true,
			wantErr:           "servingInfo.groups not found in observedConfig",
		},
		{
			name:              "observed groups are translated to etcd curve names",
			accessor:          enabled,
			observedConfig:    []byte(`{"servingInfo":{"groups":["secp384r1","X25519","SecP256r1MLKEM768"]}}`),
			bootstrapComplete: true,
			wantCurves:        tlshelpers.SupportedEtcdCurves([]string{"secp384r1", "X25519", "SecP256r1MLKEM768"}),
		},
		{
			name:              "omitted custom groups leave the env var unset",
			accessor:          enabled,
			observedConfig:    []byte(`{"servingInfo":{"groups":[]}}`),
			bootstrapComplete: true,
		},
		{
			name:              "hybrid groups etcd accepts are rendered",
			accessor:          enabled,
			observedConfig:    []byte(`{"servingInfo":{"groups":["SecP256r1MLKEM768","SecP384r1MLKEM1024"]}}`),
			bootstrapComplete: true,
			wantCurves:        []string{"SecP256r1MLKEM768", "SecP384r1MLKEM1024"},
		},
		{
			name:              "groups with no etcd-supported curve fail closed",
			accessor:          enabled,
			observedConfig:    []byte(`{"servingInfo":{"groups":["not-a-group"]}}`),
			bootstrapComplete: true,
			wantErr:           "no etcd-supported TLS curves",
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
				bootstrapConfigMapLister: bootstrapConfigMapLister(tc.bootstrapComplete),
				featureGateAccessor:      tc.accessor,
			}
			got, err := getCurvePreferences(ctx)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if len(tc.wantCurves) == 0 {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, strings.Join(tc.wantCurves, ","), got[curvePreferencesEnvName])
		})
	}
}
