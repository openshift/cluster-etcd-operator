package backuphelpers

import (
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAutoBackupFeatureGateEnabled(t *testing.T) {
	backupFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{AutomatedEtcdBackupFeatureGateName},
		[]configv1.FeatureGateName{})
	enabled, err := AutoBackupFeatureGateEnabled(backupFeatureGateAccessor)
	require.Truef(t, enabled, "backup enabled = true")
	require.NoError(t, err)
}

func TestAutoBackupFeatureGateDisabled(t *testing.T) {
	backupFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{},
		[]configv1.FeatureGateName{AutomatedEtcdBackupFeatureGateName})
	enabled, err := AutoBackupFeatureGateEnabled(backupFeatureGateAccessor)
	require.Falsef(t, enabled, "backup enabled = false")
	require.NoError(t, err)
}
