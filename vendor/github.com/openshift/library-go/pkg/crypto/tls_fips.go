package crypto

import (
	"crypto/fips140"
	"os"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
)

const hostFIPSEnabledPath = "/proc/sys/crypto/fips_enabled"

// fipsTLSGroups is the allowlist of TLSGroup values approved under FIPS 186-5.
// Only the NIST P-curves qualify. An allowlist is used rather than a blocklist
// because new groups are non-FIPS by default until they complete formal NIST
// validation (typically years), so any future group absent from this set is
// correctly excluded without requiring a library-go update.
//
// KNOWN DIVERGENCE (revisit as part of native Go FIPS adoption, HPSTRAT-723).
// Three sources disagree about the FIPS status of some groups, and this
// allowlist deliberately takes the strictest position:
//
//		group           openshift/api godoc     this allowlist   Go native FIPS (fips140=on)
//		X25519          "keep" (only            drops            REFUSES
//		                 X25519MLKEM768 should
//		                 be ignored in FIPS)
//		X25519MLKEM768  "ignore in FIPS"        drops            NEGOTIATES it
//
//	  - On plain X25519 this allowlist matches the runtime (both exclude it); the
//	    openshift/api godoc is the outdated one (corrected in OCPBUGS-109794).
//	  - On X25519MLKEM768 this allowlist is stricter than Go: Go's module will
//	    negotiate the hybrid, but we drop it because the hybrid embeds X25519 and
//	    the FIPS status of ML-KEM hybrids is still settling (SP 800-56C key
//	    derivation guidance).
//
// TODO: remove this file once Go 1.27 without OpenSSL FIPS is supported.
// The filtering exists because operands linked against an OpenSSL FIPS backend
// (via cgo) hard-reject non-approved curves rather than silently skipping them,
// which causes crash-loops. When all operands use Go's native FIPS mode (Go
// 1.27+, no OpenSSL FIPS), crypto/tls filters non-approved curves at
// negotiation time without crashing, making the pre-filtering here redundant.
var fipsTLSGroups = map[configv1.TLSGroup]struct{}{
	configv1.TLSGroupSecP256r1: {},
	configv1.TLSGroupSecP384r1: {},
	configv1.TLSGroupSecP521r1: {},
}

// IsFIPSEnabled reports whether TLS groups should be treated as FIPS-restricted.
//
// The native Go FIPS 140 mode is controlled by crypto/fips140. Red Hat Go
// builds can use a host FIPS crypto backend whose mode is not exposed through
// a public Go API. The Linux kernel exposes its FIPS policy state through
// /proc/sys/crypto/fips_enabled, so a host FIPS signal also restricts groups.
// This policy-level signal does not identify a particular process's crypto
// backend. If it is unavailable or malformed, this function preserves the
// native Go result.
func IsFIPSEnabled() bool {
	return isFIPSEnabled(fips140.Enabled(), os.ReadFile)
}

func isFIPSEnabled(nativeFIPSEnabled bool, readFile func(string) ([]byte, error)) bool {
	return nativeFIPSEnabled || isHostFIPSEnabled(readFile)
}

func isHostFIPSEnabled(readFile func(string) ([]byte, error)) bool {
	data, err := readFile(hostFIPSEnabledPath)
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(data)) == "1"
}

// FIPSApprovedTLSGroups returns the subset of groups that are FIPS-approved,
// preserving order and dropping the rest.
func FIPSApprovedTLSGroups(groups []configv1.TLSGroup) []configv1.TLSGroup {
	approved := make([]configv1.TLSGroup, 0, len(groups))
	for _, g := range groups {
		if _, ok := fipsTLSGroups[g]; !ok {
			continue
		}
		approved = append(approved, g)
	}
	return approved
}
