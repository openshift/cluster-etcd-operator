package dualreplicahelpers

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/require"
)

func TestDualReplicaFeatureGateEnabled(t *testing.T) {
	dualReplicaFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{DualReplicaFeatureGateName},
		[]configv1.FeatureGateName{})
	enabled, err := DualReplicaFeatureGateEnabled(dualReplicaFeatureGateAccessor)
	require.True(t, enabled)
	require.NoError(t, err)
}

func TestDualReplicaFeatureGateDisabled(t *testing.T) {
	dualReplicaFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{},
		[]configv1.FeatureGateName{DualReplicaFeatureGateName})
	enabled, err := DualReplicaFeatureGateEnabled(dualReplicaFeatureGateAccessor)
	require.False(t, enabled)
	require.NoError(t, err)
}
