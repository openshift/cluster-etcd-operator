package tlshelpers

import (
	"crypto/fips140"
	"crypto/tls"
	"fmt"
	"slices"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"k8s.io/klog/v2"
)

// etcdCurveIDs are the curve names accepted by etcd --curve-preferences.
// Names are crypto/tls CurveID.String() values. etcd rejects anything else,
// including OpenShift groups such as SecP256r1MLKEM768.
// Keep this aligned with openshift/etcd client/pkg/tlsutil/curves.go.
var etcdCurveIDs = map[string]tls.CurveID{
	tls.X25519MLKEM768.String(): tls.X25519MLKEM768,
	tls.X25519.String():         tls.X25519,
	tls.CurveP256.String():      tls.CurveP256,
	tls.CurveP384.String():      tls.CurveP384,
	tls.CurveP521.String():      tls.CurveP521,
}

// fipsApprovedTLSGroups is the FIPS 186-5 allowlist used by library-go
// (crypto.FIPSApprovedTLSGroups). That helper is not in the vendored
// library-go yet, so the same set is applied here before the list is handed
// to etcd. X25519 and the ML-KEM hybrids are omitted: an OpenSSL-FIPS etcd
// rejects them instead of skipping them.
var fipsApprovedTLSGroups = map[configv1.TLSGroup]struct{}{
	configv1.TLSGroupSecP256r1: {},
	configv1.TLSGroupSecP384r1: {},
	configv1.TLSGroupSecP521r1: {},
}

// TLSGroupPreferencesEnabled reports whether etcd should honor cluster TLS
// group preferences. The gate is off for the default feature set.
//
// An accessor that does not list the gate (partial test accessors) is treated
// as disabled. The production accessor always registers it.
func TLSGroupPreferencesEnabled(accessor featuregates.FeatureGateAccess) (bool, error) {
	if accessor == nil {
		return false, nil
	}
	gates, err := accessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates: %w", err)
	}
	if !slices.Contains(gates.KnownFeatures(), features.FeatureGateTLSGroupPreferences) {
		return false, nil
	}
	return gates.Enabled(features.FeatureGateTLSGroupPreferences), nil
}

// SupportedEtcdCurves maps OpenShift TLS group names to the curve names
// accepted by etcd --curve-preferences, preserving order. Unrecognized groups
// and groups etcd does not implement are dropped. In FIPS mode, groups outside
// the NIST P-curve allowlist are dropped as well.
func SupportedEtcdCurves(groups []string) []string {
	return filterEtcdCurves(groups, fips140.Enabled())
}

func filterEtcdCurves(groups []string, fips bool) []string {
	curves := make([]string, 0, len(groups))
	for _, name := range groups {
		group := configv1.TLSGroup(name)
		if fips {
			if _, ok := fipsApprovedTLSGroups[group]; !ok {
				klog.Warningf("TLS group %q is not FIPS-approved for etcd, skipping", name)
				continue
			}
		}
		id, ok := crypto.TLSGroupToCurveID(group)
		if !ok {
			klog.Warningf("TLS group %q is not recognized, skipping for etcd", name)
			continue
		}
		curveName := id.String()
		if _, supported := etcdCurveIDs[curveName]; !supported {
			klog.Warningf("TLS group %q (%s) is not supported by etcd --curve-preferences, skipping", name, curveName)
			continue
		}
		curves = append(curves, curveName)
	}
	return curves
}

// ParseEtcdCurveNames converts etcd --curve-preferences names to tls.CurveID
// values. An empty input returns a nil slice and a nil error so callers leave
// Go's default curve selection in place.
func ParseEtcdCurveNames(names []string) ([]tls.CurveID, error) {
	if len(names) == 0 {
		return nil, nil
	}
	curves := make([]tls.CurveID, 0, len(names))
	for _, name := range names {
		id, ok := etcdCurveIDs[name]
		if !ok {
			return nil, fmt.Errorf("unsupported etcd TLS curve %q", name)
		}
		curves = append(curves, id)
	}
	return curves, nil
}
