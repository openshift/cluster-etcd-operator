package tlshelpers

import (
	"crypto/tls"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"k8s.io/klog/v2"
)

// etcdCurveIDs are the curve names accepted by etcd --listen-curve-preferences.
// Names are crypto/tls CurveID.String() values. etcd rejects anything else.
//
// The set matches openshift/etcd tlsutil.GetCurveID:
// https://github.com/openshift/etcd/blob/7dd2f94050a119e655e9df02e868fb2d2a3de747/client/pkg/tlsutil/curves.go
// That helper is not in the upstream etcd module this repo vendors
// (go.etcd.io/etcd v3.7.0), so the map stays local until the fork is vendored.
var etcdCurveIDs = map[string]tls.CurveID{
	tls.CurveP256.String():          tls.CurveP256,
	tls.CurveP384.String():          tls.CurveP384,
	tls.CurveP521.String():          tls.CurveP521,
	tls.X25519.String():             tls.X25519,
	tls.X25519MLKEM768.String():     tls.X25519MLKEM768,
	tls.SecP256r1MLKEM768.String():  tls.SecP256r1MLKEM768,
	tls.SecP384r1MLKEM1024.String(): tls.SecP384r1MLKEM1024,
}

// TLSGroupPreferencesEnabled reports whether etcd should honor cluster TLS
// group preferences. The gate is off for the default feature set.
//
// A nil accessor is treated as disabled. A non-nil accessor must register the
// gate: FeatureGate.Enabled panics when the gate is in neither the enabled
// nor the disabled list.
func TLSGroupPreferencesEnabled(accessor featuregates.FeatureGateAccess) (bool, error) {
	if accessor == nil {
		return false, nil
	}
	gates, err := accessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates: %w", err)
	}
	return gates.Enabled(features.FeatureGateTLSGroupPreferences), nil
}

// SupportedEtcdCurves maps OpenShift TLS group names to the curve names
// accepted by etcd --listen-curve-preferences, preserving order. Unrecognized groups
// and groups etcd does not implement are dropped. In FIPS mode, groups outside
// the NIST P-curve allowlist are dropped as well.
func SupportedEtcdCurves(groups []string) []string {
	return filterEtcdCurves(groups, crypto.IsFIPSEnabled())
}

func filterEtcdCurves(groups []string, fips bool) []string {
	tlsGroups := make([]configv1.TLSGroup, 0, len(groups))
	for _, name := range groups {
		tlsGroups = append(tlsGroups, configv1.TLSGroup(name))
	}
	if fips {
		approved := crypto.FIPSApprovedTLSGroups(tlsGroups)
		kept := make(map[configv1.TLSGroup]struct{}, len(approved))
		for _, group := range approved {
			kept[group] = struct{}{}
		}
		for _, group := range tlsGroups {
			if _, ok := kept[group]; !ok {
				klog.Warningf("TLS group %q is not FIPS-approved for etcd, skipping", group)
			}
		}
		tlsGroups = approved
	}
	ids, unrecognized := crypto.TLSGroupsToCurveIDs(tlsGroups)
	for _, group := range unrecognized {
		klog.Warningf("TLS group %q is not recognized, skipping for etcd", group)
	}
	curves := make([]string, 0, len(ids))
	for _, id := range ids {
		curveName := id.String()
		if _, supported := etcdCurveIDs[curveName]; !supported {
			klog.Warningf("TLS curve %s is not supported by etcd --listen-curve-preferences, skipping", curveName)
			continue
		}
		curves = append(curves, curveName)
	}
	return curves
}

// ParseEtcdCurveNames converts etcd --listen-curve-preferences names to tls.CurveID
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
