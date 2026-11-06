package pacemaker

import (
	"context"
	"fmt"
	"slices"

	"github.com/openshift/api/features"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AlertAgentStatusEnabled is shared by the operator and the collector. The gate
// controls alert-agent status, its contribution to health, and periodic self healing.
// An older payload that does not know this gate retains the pre-feature behavior.
func AlertAgentStatusEnabled(gates featuregates.FeatureGate) bool {
	return slices.Contains(gates.KnownFeatures(), features.FeatureGateTNFObservability) &&
		gates.Enabled(features.FeatureGateTNFObservability)
}

// An unresolved gate permits base collection and retention of existing alert-agent
// observations, but never authorizes new observations or CIB repairs.
type alertAgentGateState string

const (
	alertAgentGateUnresolved alertAgentGateState = "Unresolved"
	alertAgentGateDisabled   alertAgentGateState = "Disabled"
	alertAgentGateEnabled    alertAgentGateState = "Enabled"
)

// alertAgentGateForRelease reads resolved status, rather than deriving
// gates from the feature-set name. During an upgrade, an old Job must use its own
// release's entry even when ClusterVersion already desires the next release.
// Only development/manual runs without a release version use the desired version.
func alertAgentGateForRelease(ctx context.Context, client configclient.Interface, releaseVersion string) (alertAgentGateState, error) {
	// VersionForOperatorFromEnv returns "" when unset; "0.0.1-snapshot" matches
	// CEO's development missingVersion sentinel in pkg/operator/starter.go.
	if releaseVersion == "" || releaseVersion == "0.0.1-snapshot" {
		version, err := client.ConfigV1().ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
		if err != nil {
			return alertAgentGateUnresolved, fmt.Errorf("get cluster version for feature gates: %w", err)
		}
		releaseVersion = version.Status.Desired.Version
		if releaseVersion == "" && len(version.Status.History) > 0 {
			releaseVersion = version.Status.History[0].Version
		}
		if releaseVersion == "" {
			return alertAgentGateUnresolved, fmt.Errorf("cluster version has no release version for feature gates")
		}
	}

	gate, err := client.ConfigV1().FeatureGates().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return alertAgentGateUnresolved, fmt.Errorf("get cluster feature gates: %w", err)
	}
	accessor, err := featuregates.NewHardcodedFeatureGateAccessFromFeatureGate(gate, releaseVersion)
	if err != nil {
		return alertAgentGateUnresolved, fmt.Errorf("resolve feature gates for collector release %q: %w", releaseVersion, err)
	}
	gates, err := accessor.CurrentFeatureGates()
	if err != nil {
		return alertAgentGateUnresolved, err
	}
	if AlertAgentStatusEnabled(gates) {
		return alertAgentGateEnabled, nil
	}
	return alertAgentGateDisabled, nil
}
