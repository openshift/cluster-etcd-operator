package tlssecurityprofile

import (
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	libgoapiserver "github.com/openshift/library-go/pkg/operator/configobserver/apiserver"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
)

var (
	minTLSVersionPath = []string{"servingInfo", "minTLSVersion"}
	cipherSuitesPath  = []string{"servingInfo", "cipherSuites"}
	groupsPath        = []string{"servingInfo", "groups"}
)

// ObserveTLSSecurityProfile observes the cluster TLS profile into observedConfig.
//
// Cipher suites and the minimum TLS version always come from library-go.
// Groups are included only while TLSGroupPreferences is enabled. Named profiles
// carry default groups, so observing them while the gate is off would change
// etcd curve selection for every cluster. library-go applies its FIPS group
// allowlist when groups are observed.
func ObserveTLSSecurityProfile(accessor featuregates.FeatureGateAccess) configobserver.ObserveConfigFunc {
	return func(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]any) (map[string]any, []error) {
		enabled, err := tlshelpers.TLSGroupPreferencesEnabled(accessor)
		if err != nil {
			return configobserver.Pruned(existingConfig, minTLSVersionPath, cipherSuitesPath, groupsPath), []error{err}
		}
		if !enabled {
			return libgoapiserver.ObserveTLSSecurityProfile(genericListers, recorder, existingConfig)
		}
		return libgoapiserver.ObserveTLSSecurityProfileWithGroupPaths(
			genericListers,
			recorder,
			existingConfig,
			minTLSVersionPath,
			cipherSuitesPath,
			groupsPath,
		)
	}
}
