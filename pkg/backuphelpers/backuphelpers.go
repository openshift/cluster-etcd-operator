package backuphelpers

import (
	"fmt"

	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
)

const AutomatedEtcdBackupFeatureGateName = "AutomatedEtcdBackup"

func AutoBackupFeatureGateEnabled(featureGateAccessor featuregates.FeatureGateAccess) (bool, error) {
	gates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates, error was: %w", err)
	}

	return gates.Enabled(AutomatedEtcdBackupFeatureGateName), nil
}
