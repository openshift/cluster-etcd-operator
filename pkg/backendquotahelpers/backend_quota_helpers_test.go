package backendquotahelpers

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/require"
)

func TestBackendQuotaFeatureGateEnabled(t *testing.T) {
	backendQuotaFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{BackendQuotaFeatureGateName},
		[]configv1.FeatureGateName{})
	enabled, err := IsBackendQuotaFeatureGateEnabled(backendQuotaFeatureGateAccessor)
	require.True(t, enabled)
	require.NoError(t, err)
}

func TestBackendQuotaFeatureGateDisabled(t *testing.T) {
	backendQuotaFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{},
		[]configv1.FeatureGateName{BackendQuotaFeatureGateName})
	enabled, err := IsBackendQuotaFeatureGateEnabled(backendQuotaFeatureGateAccessor)
	require.False(t, enabled)
	require.NoError(t, err)
}
