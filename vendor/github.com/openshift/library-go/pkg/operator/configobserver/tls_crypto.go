package configobserver

import (
	"crypto/tls"
	"fmt"
	"strconv"

	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/openshift/library-go/pkg/crypto"
)

// CurvePreferencesToTLSGroups reads curve preference IDs from sourcePath in config,
// converts them to TLS group names using the reverse mapping, and writes them to targetPath.
// Returns a list of unrecognized curve IDs that were skipped during conversion.
// Returns error if source path contains invalid data or target path cannot be set.
func CurvePreferencesToTLSGroups[T int64 | string](
	config map[string]interface{},
	sourcePath []string,
	targetPath []string,
) ([]tls.CurveID, error) {
	curvePrefsRaw, found, err := unstructured.NestedSlice(config, sourcePath...)
	if err != nil {
		return nil, fmt.Errorf("failed to read curve preferences from %v: %w", sourcePath, err)
	}
	if !found {
		return nil, nil
	}

	var unrecognizedCurveIDs []tls.CurveID
	groups := make([]string, 0, len(curvePrefsRaw))
	var t T
	for _, cpRaw := range curvePrefsRaw {
		var cpInt64 int64
		switch any(t).(type) {
		case int64:
			// May be float64 when unmarshalled from YAML into an interface{} target
			if fv, ok := cpRaw.(float64); ok {
				if fv != float64(int64(fv)) {
					return nil, fmt.Errorf("invalid curve preference at %v: non-integral float64 value %v", sourcePath, fv)
				}
				cpInt64 = int64(fv)
			} else if v, ok := cpRaw.(int64); ok {
				cpInt64 = v
			} else {
				return nil, fmt.Errorf("invalid curve preference type at %v: expected int64, got %T", sourcePath, cpRaw)
			}
		case string:
			v, ok := cpRaw.(string)
			if !ok {
				return nil, fmt.Errorf("invalid curve preference type at %v: expected string, got %T", sourcePath, cpRaw)
			}
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid curve preference string at %v: %w", sourcePath, err)
			}
			cpInt64 = parsed
		}

		if cpInt64 < 0 || cpInt64 > 65535 {
			return nil, fmt.Errorf("curve preference value %d at %v is out of range for uint16 (0-65535)", cpInt64, sourcePath)
		}

		curveID := tls.CurveID(cpInt64)
		groupName, found := crypto.CurveIDToTLSGroup(curveID)
		if !found {
			unrecognizedCurveIDs = append(unrecognizedCurveIDs, curveID)
			continue
		}
		groups = append(groups, string(groupName))
	}

	if len(groups) > 0 {
		if err := unstructured.SetNestedStringSlice(config, groups, targetPath...); err != nil {
			return nil, fmt.Errorf("failed to set groups at %v: %w", targetPath, err)
		}
	}

	return unrecognizedCurveIDs, nil
}

// TLSGroupsToCurvePreferences reads TLS group names from sourcePath in config,
// converts them to curve preference IDs, and writes them to targetPath.
// Returns a list of unrecognized group names that were skipped during conversion.
// Returns error if paths cannot be accessed.
func TLSGroupsToCurvePreferences[T int64 | string](
	config map[string]interface{},
	sourcePath []string,
	targetPath []string,
) ([]configv1.TLSGroup, error) {
	groups, found, err := unstructured.NestedStringSlice(config, sourcePath...)
	if err != nil {
		return nil, fmt.Errorf("failed to read groups from %v: %w", sourcePath, err)
	}
	if !found {
		return nil, nil
	}

	tlsGroups := make([]configv1.TLSGroup, len(groups))
	for i, g := range groups {
		tlsGroups[i] = configv1.TLSGroup(g)
	}

	curvePreferences, unrecognizedGroups := crypto.TLSGroupsToCurvePreferences(tlsGroups)

	if len(curvePreferences) == 0 {
		return unrecognizedGroups, nil
	}

	// Normalize existing values to int64 for comparison (they may be float64 from YAML unmarshaling).
	existingPrefs, existingFound, _ := unstructured.NestedSlice(config, targetPath...)
	if existingFound && len(existingPrefs) == len(curvePreferences) {
		allMatch := true
		for i, cp := range curvePreferences {
			var existingInt64 int64
			switch v := existingPrefs[i].(type) {
			case int64:
				existingInt64 = v
			case float64:
				if v != float64(int64(v)) {
					allMatch = false
					break
				}
				existingInt64 = int64(v)
			default:
				allMatch = false
				break
			}
			if existingInt64 != int64(cp) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return unrecognizedGroups, nil
		}
	}

	curvePrefsInterface := make([]interface{}, len(curvePreferences))
	var t T
	switch any(t).(type) {
	case int64:
		for i, cp := range curvePreferences {
			curvePrefsInterface[i] = int64(cp)
		}
	case string:
		for i, cp := range curvePreferences {
			curvePrefsInterface[i] = strconv.Itoa(int(cp))
		}
	}

	if err := unstructured.SetNestedSlice(config, curvePrefsInterface, targetPath...); err != nil {
		return nil, fmt.Errorf("failed to set curve preferences at %v: %w", targetPath, err)
	}

	return unrecognizedGroups, nil
}
