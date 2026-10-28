package tlshelpers

import (
	"crypto/tls"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterEtcdCurves(t *testing.T) {
	groups := []string{
		string(configv1.TLSGroupX25519MLKEM768),
		string(configv1.TLSGroupX25519),
		string(configv1.TLSGroupSecP256r1),
		string(configv1.TLSGroupSecP384r1),
		string(configv1.TLSGroupSecP521r1),
		string(configv1.TLSGroupSecP256r1MLKEM768),
		"not-a-group",
	}

	t.Run("non-FIPS keeps etcd-supported curves in order", func(t *testing.T) {
		assert.Equal(t, []string{
			"X25519MLKEM768",
			"X25519",
			"CurveP256",
			"CurveP384",
			"CurveP521",
		}, filterEtcdCurves(groups, false))
	})

	t.Run("FIPS keeps only NIST curves", func(t *testing.T) {
		assert.Equal(t, []string{
			"CurveP256",
			"CurveP384",
			"CurveP521",
		}, filterEtcdCurves(groups, true))
	})
}

func TestParseEtcdCurveNames(t *testing.T) {
	curves, err := ParseEtcdCurveNames([]string{"CurveP384", "X25519"})
	require.NoError(t, err)
	assert.Equal(t, []tls.CurveID{tls.CurveP384, tls.X25519}, curves)

	curves, err = ParseEtcdCurveNames(nil)
	require.NoError(t, err)
	assert.Nil(t, curves)

	_, err = ParseEtcdCurveNames([]string{"secp256r1"})
	require.Error(t, err)
}

func TestTLSGroupPreferencesEnabled(t *testing.T) {
	enabled, err := TLSGroupPreferencesEnabled(nil)
	require.NoError(t, err)
	assert.False(t, enabled)

	enabled, err = TLSGroupPreferencesEnabled(featuregates.NewHardcodedFeatureGateAccess(nil, nil))
	require.NoError(t, err)
	assert.False(t, enabled)

	enabled, err = TLSGroupPreferencesEnabled(featuregates.NewHardcodedFeatureGateAccess(
		nil, []configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences}))
	require.NoError(t, err)
	assert.False(t, enabled)

	enabled, err = TLSGroupPreferencesEnabled(featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences}, nil))
	require.NoError(t, err)
	assert.True(t, enabled)
}
