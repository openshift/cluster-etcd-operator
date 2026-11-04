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

// AlertAgentStatusEnabled is shared by the operator and the collector. An older
// payload that does not know this gate retains the pre-feature behavior.
func AlertAgentStatusEnabled(gates featuregates.FeatureGate) bool {
	return slices.Contains(gates.KnownFeatures(), features.FeatureGateTNFObservability) &&
		gates.Enabled(features.FeatureGateTNFObservability)
}

// alertAgentStatusEnabledForRelease reads resolved status, rather than deriving
// gates from the feature-set name. During an upgrade, an old Job must use its own
// release's entry even when ClusterVersion already desires the next release.
// Only development/manual runs without a release version use the desired version.
func alertAgentStatusEnabledForRelease(ctx context.Context, client configclient.Interface, releaseVersion string) (bool, error) {
	if releaseVersion == "" || releaseVersion == "0.0.1-snapshot" {
		version, err := client.ConfigV1().ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get cluster version for feature gates: %w", err)
		}
		releaseVersion = version.Status.Desired.Version
		if releaseVersion == "" && len(version.Status.History) > 0 {
			releaseVersion = version.Status.History[0].Version
		}
		if releaseVersion == "" {
			return false, fmt.Errorf("cluster version has no release version for feature gates")
		}
	}

	gate, err := client.ConfigV1().FeatureGates().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get cluster feature gates: %w", err)
	}
	accessor, err := featuregates.NewHardcodedFeatureGateAccessFromFeatureGate(gate, releaseVersion)
	if err != nil {
		return false, fmt.Errorf("resolve feature gates for collector release %q: %w", releaseVersion, err)
	}
	gates, err := accessor.CurrentFeatureGates()
	if err != nil {
		return false, err
	}
	return AlertAgentStatusEnabled(gates), nil
}
