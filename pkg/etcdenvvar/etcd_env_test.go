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
// complete an empty observedConfig is a genuine failure.
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
