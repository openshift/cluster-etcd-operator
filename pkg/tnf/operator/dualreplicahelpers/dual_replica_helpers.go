package dualreplicahelpers

import (
	"fmt"

	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
)

const DualReplicaFeatureGateName = "DualReplica"

func DualReplicaFeatureGateEnabled(featureGateAccessor featuregates.FeatureGateAccess) (bool, error) {
	gates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates, error was: %w", err)
	}

	return gates.Enabled(DualReplicaFeatureGateName), nil
}
