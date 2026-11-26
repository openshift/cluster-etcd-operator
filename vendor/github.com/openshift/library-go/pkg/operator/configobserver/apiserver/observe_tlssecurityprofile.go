package apiserver

import (
	"fmt"
	"reflect"
	"slices"

	"k8s.io/klog/v2"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	tempTLSGroupsField = "tempServingInfoGroupsIgnore"
)

// ObserveTLSSecurityProfile observes APIServer.Spec.TLSSecurityProfile field and sets
// the ServingInfo.MinTLSVersion, ServingInfo.CipherSuites fields of observed config
func ObserveTLSSecurityProfile(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	return innerTLSSecurityProfileObservations(genericListers, recorder, existingConfig, []string{"servingInfo", "minTLSVersion"}, []string{"servingInfo", "cipherSuites"}, nil)
}

// ObserveTLSSecurityProfileWithPaths is like ObserveTLSSecurityProfile, but accepts
// custom paths for ServingInfo.MinTLSVersion and ServingInfo.CipherSuites fields of observed config.
func ObserveTLSSecurityProfileWithPaths(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}, minTLSVersionPath, cipherSuitesPath []string) (map[string]interface{}, []error) {
	return innerTLSSecurityProfileObservations(genericListers, recorder, existingConfig, minTLSVersionPath, cipherSuitesPath, nil)
}

// ObserveTLSSecurityProfileWithGroupPaths is like ObserveTLSSecurityProfileWithPaths but also
// observes the TLS group (curve) preferences at groupsPath. Group names are stored as []string
// matching the TLSGroup constants from openshift/api and can be passed directly to operands
// that accept group names as CLI arguments.
func ObserveTLSSecurityProfileWithGroupPaths(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}, minTLSVersionPath, cipherSuitesPath, groupsPath []string) (map[string]interface{}, []error) {
	return innerTLSSecurityProfileObservations(genericListers, recorder, existingConfig, minTLSVersionPath, cipherSuitesPath, groupsPath)
}

// ObserveTLSSecurityProfileWithGroupsConversion observes TLS groups from APIServer config and converts them to curve preferences.
// It handles the full lifecycle: reading existing curves, observing new groups, checking for filtered groups and converting groups to curves.
// Uses servingInfo paths and int64 type for curve preferences.
func ObserveTLSSecurityProfileWithGroupsConversion(listers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	return innerObserveTLSSecurityProfileWithGroupsConversion[int64](
		listers,
		recorder,
		existingConfig,
		ObserveTLSSecurityProfileWithGroupPaths,
		[]string{"servingInfo", "minTLSVersion"},
		[]string{"servingInfo", "cipherSuites"},
		[]string{"servingInfo", "curvePreferences"},
	)
}

// ObserveTLSSecurityProfileWithGroupsConversionToArguments observes TLS groups from APIServer config and converts them to curve preferences for apiServerArguments.
// It handles the full lifecycle: reading existing curves, observing new groups, checking for filtered groups and converting groups to curves.
// Uses apiServerArguments paths and string type for curve preferences.
func ObserveTLSSecurityProfileWithGroupsConversionToArguments(listers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	return innerObserveTLSSecurityProfileWithGroupsConversion[string](
		listers,
		recorder,
		existingConfig,
		ObserveTLSSecurityProfileWithGroupPaths,
		[]string{"apiServerArguments", "tls-min-version"},
		[]string{"apiServerArguments", "tls-cipher-suites"},
		[]string{"apiServerArguments", "tls-curve-preferences"},
	)
}

// ObserveTLSSecurityProfileToArguments observes APIServer.Spec.TLSSecurityProfile field and sets
// the tls-min-version and tls-cipher-suites fileds of observedConfig.apiServerArguments
func ObserveTLSSecurityProfileToArguments(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	return innerTLSSecurityProfileObservations(genericListers, recorder, existingConfig, []string{"apiServerArguments", "tls-min-version"}, []string{"apiServerArguments", "tls-cipher-suites"}, nil)
}

// innerTLSSecurityProfileObservations is the shared implementation for all Observe* functions.
// When groupsPath is non-nil, TLS group preferences are also observed and stored at that path.
func innerTLSSecurityProfileObservations(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}, minTLSVersionPath, cipherSuitesPath, groupsPath []string) (ret map[string]interface{}, _ []error) {
	defer func() {
		paths := [][]string{minTLSVersionPath, cipherSuitesPath}
		if len(groupsPath) > 0 {
			paths = append(paths, groupsPath)
		}
		ret = configobserver.Pruned(ret, paths...)
	}()

	listers := genericListers.(APIServerLister)
	errs := []error{}

	currentMinTLSVersion, _, versionErr := unstructured.NestedString(existingConfig, minTLSVersionPath...)
	if versionErr != nil {
		errs = append(errs, fmt.Errorf("failed to retrieve %v: %v", minTLSVersionPath, versionErr))
		// keep going on read error from existing config
	}

	currentCipherSuites, _, suitesErr := unstructured.NestedStringSlice(existingConfig, cipherSuitesPath...)
	if suitesErr != nil {
		errs = append(errs, fmt.Errorf("failed to retrieve %v: %v", cipherSuitesPath, suitesErr))
		// keep going on read error from existing config
	}

	var currentGroups []string
	if len(groupsPath) > 0 {
		var groupsErr error
		currentGroups, _, groupsErr = unstructured.NestedStringSlice(existingConfig, groupsPath...)
		if groupsErr != nil {
			errs = append(errs, fmt.Errorf("failed to retrieve groups: %v", groupsErr))
		}
	}

	apiServer, err := listers.APIServerLister().Get("cluster")
	if errors.IsNotFound(err) {
		klog.Warningf("apiserver.config.openshift.io/cluster: not found")
		apiServer = &configv1.APIServer{}
	} else if err != nil {
		return existingConfig, append(errs, err)
	}

	// Resolve the effective profile spec once and share it between the cipher and
	// group extractors so the two cannot resolve different profiles.
	profileSpec := resolveProfileSpec(apiServer.Spec.TLSSecurityProfile)

	observedConfig := map[string]interface{}{}
	observedMinTLSVersion, observedCipherSuites := getSecurityProfileCiphers(profileSpec)
	if err = unstructured.SetNestedField(observedConfig, observedMinTLSVersion, minTLSVersionPath...); err != nil {
		return existingConfig, append(errs, err)
	}
	if err = unstructured.SetNestedStringSlice(observedConfig, observedCipherSuites, cipherSuitesPath...); err != nil {
		return existingConfig, append(errs, err)
	}

	if observedMinTLSVersion != currentMinTLSVersion {
		recorder.Eventf("ObserveTLSSecurityProfile", "minTLSVersion changed to %s", observedMinTLSVersion)
	}
	if !reflect.DeepEqual(observedCipherSuites, currentCipherSuites) {
		recorder.Eventf("ObserveTLSSecurityProfile", "cipherSuites changed to %q", observedCipherSuites)
	}

	if len(groupsPath) > 0 {
		observedGroups := crypto.GetSecurityProfileGroups(profileSpec)
		// Store the groups (already FIPS-filtered) verbatim, including an empty list.
		// An empty result (e.g. all groups dropped under FIPS) lets TLS fail rather
		// than negotiating unrequested curves.
		if err = unstructured.SetNestedStringSlice(observedConfig, observedGroups, groupsPath...); err != nil {
			return existingConfig, append(errs, err)
		}
		// slices.Equal treats nil and empty as equal, so a first reconcile
		// (currentGroups nil) against an empty observation is not a change.
		if !slices.Equal(observedGroups, currentGroups) {
			recorder.Eventf("ObserveTLSSecurityProfile", "groups changed to %q", observedGroups)
		}
	}

	return observedConfig, errs
}

// getFilteredOutGroups checks if any TLS groups from a Custom profile were filtered out (e.g., by FIPS)
// and returns the list of filtered groups. Returns an error if the APIServer CR cannot be retrieved (NotFound is acceptable).
func getFilteredOutGroups(listers APIServerLister, remainingGroups []string) ([]configv1.TLSGroup, error) {
	apiServer, err := listers.APIServerLister().Get("cluster")
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get apiserver/cluster: %w", err)
	}

	if apiServer == nil || apiServer.Spec.TLSSecurityProfile == nil {
		return nil, nil
	}

	if apiServer.Spec.TLSSecurityProfile.Type != configv1.TLSProfileCustomType ||
		apiServer.Spec.TLSSecurityProfile.Custom == nil ||
		len(apiServer.Spec.TLSSecurityProfile.Custom.Groups) <= len(remainingGroups) {
		return nil, nil
	}

	groupsSet := make(map[string]bool)
	for _, g := range remainingGroups {
		groupsSet[g] = true
	}

	filteredOut := []configv1.TLSGroup{}
	for _, originalGroup := range apiServer.Spec.TLSSecurityProfile.Custom.Groups {
		if !groupsSet[string(originalGroup)] {
			filteredOut = append(filteredOut, originalGroup)
		}
	}

	return filteredOut, nil
}

// observeTLSSecurityProfileWithGroupPathsFunc is the signature for observing TLS security profile with groups.
type observeTLSSecurityProfileWithGroupPathsFunc func(
	listers configobserver.Listers,
	recorder events.Recorder,
	existingConfig map[string]interface{},
	minTLSVersionPath []string,
	cipherSuitesPath []string,
	groupsPath []string,
) (map[string]interface{}, []error)

// innerObserveTLSSecurityProfileWithGroupsConversion is the internal implementation that accepts a custom
// observeTLSSecurityProfile function for testability (e.g., to mock FIPS filtering).
// It accepts custom paths for minTLSVersion, cipherSuites, and curvePreferences to support different config schemas.
// The curve type specifies whether to use int64 (for servingInfo) or string (for apiServerArguments).
func innerObserveTLSSecurityProfileWithGroupsConversion[T int64 | string](
	listers configobserver.Listers,
	recorder events.Recorder,
	existingConfig map[string]interface{},
	observeTLSSecurityProfile observeTLSSecurityProfileWithGroupPathsFunc,
	minTLSVersionPath []string,
	cipherSuitesPath []string,
	curvePreferencesPath []string,
) (map[string]interface{}, []error) {
	var errs []error
	var observedConfig map[string]interface{}

	if _, err := configobserver.CurvePreferencesToTLSGroups[T](existingConfig, curvePreferencesPath, []string{tempTLSGroupsField}); err != nil {
		// keep going on read error from existing config
		errs = append(errs, err)
	}

	defer func() {
		unstructured.RemoveNestedField(existingConfig, tempTLSGroupsField)
		unstructured.RemoveNestedField(observedConfig, tempTLSGroupsField)
	}()

	var observeErrs []error
	observedConfig, observeErrs = observeTLSSecurityProfile(
		listers,
		recorder,
		existingConfig,
		minTLSVersionPath,
		cipherSuitesPath,
		[]string{tempTLSGroupsField},
	)
	if len(observeErrs) > 0 {
		// accumulate errors but keep going - we still need to convert groups to curves
		errs = append(errs, observeErrs...)
	}

	// Copy existing curve preferences to observedConfig so TLSGroupsToCurvePreferences can compare against them
	existingCurvePreferences, found, err := unstructured.NestedSlice(existingConfig, curvePreferencesPath...)
	if err != nil {
		// keep going on read error from existing config
		errs = append(errs, err)
	} else if found {
		unstructured.SetNestedSlice(observedConfig, existingCurvePreferences, curvePreferencesPath...)
	}

	// Check if we need to set curvePreferences to empty
	groups, found, err := unstructured.NestedStringSlice(observedConfig, tempTLSGroupsField)
	if err == nil && found {
		// Check if this is a Custom profile and if any groups were filtered out (e.g., by FIPS)
		if filteredOut, err := getFilteredOutGroups(listers.(APIServerLister), groups); err != nil {
			errs = append(errs, err)
		} else if len(filteredOut) > 0 {
			errs = append(errs, fmt.Errorf("TLS groups filtered out from custom profile: %v", filteredOut))
		}

		if len(groups) == 0 {
			// Empty groups list - set curvePreferences to empty
			unstructured.SetNestedSlice(observedConfig, []interface{}{}, curvePreferencesPath...)
		} else if unrecognizedGroups, err := configobserver.TLSGroupsToCurvePreferences[T](observedConfig, []string{tempTLSGroupsField}, curvePreferencesPath); err != nil {
			errs = append(errs, err)
		} else if len(unrecognizedGroups) > 0 {
			// If all groups are unrecognized, set curvePreferences to empty
			if len(groups) == len(unrecognizedGroups) {
				unstructured.SetNestedSlice(observedConfig, []interface{}{}, curvePreferencesPath...)
			}
			errs = append(errs, fmt.Errorf("unrecognized TLS groups: %v", unrecognizedGroups))
		}
	}
	return observedConfig, errs
}

// resolveProfileSpec returns the effective TLSProfileSpec for a
// TLSSecurityProfile. Named profiles (Old/Intermediate/Modern) resolve from the
// well-known openshift/api table; a Custom profile uses its inline spec. When
// the profile is nil (e.g. APIServer/cluster not found) or is Custom-typed
// without an actual custom spec, it falls back to the Intermediate default.
//
// It is resolved once per reconcile in innerTLSSecurityProfileObservations and
// shared by the cipher and group extractors so the two cannot drift.
func resolveProfileSpec(profile *configv1.TLSSecurityProfile) *configv1.TLSProfileSpec {
	profileType := crypto.DefaultTLSProfileType
	if profile != nil {
		profileType = profile.Type
	}

	var profileSpec *configv1.TLSProfileSpec
	if profileType == configv1.TLSProfileCustomType {
		if profile.Custom != nil {
			profileSpec = &profile.Custom.TLSProfileSpec
		}
	} else {
		profileSpec = configv1.TLSProfiles[profileType]
	}

	// nothing found / custom type set but no actual custom spec / empty type
	if profileSpec == nil {
		profileSpec = configv1.TLSProfiles[crypto.DefaultTLSProfileType]
	}
	return profileSpec
}

// getSecurityProfileCiphers extracts the minimum TLS version and cipher suites
// from the resolved profile spec, remapping ciphers to the IANA names used by the
// Kube ServingInfo config.
func getSecurityProfileCiphers(profileSpec *configv1.TLSProfileSpec) (string, []string) {
	// need to remap all Ciphers to their respective IANA names used by Go
	return string(profileSpec.MinTLSVersion), crypto.OpenSSLToIANACipherSuites(profileSpec.Ciphers)
}
