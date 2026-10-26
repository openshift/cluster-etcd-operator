package tlssecurityprofile

import (
	"errors"
	"slices"

	configv1 "github.com/openshift/api/config/v1"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

var groupsPath = []string{"servingInfo", "groups"}

type apiServerLister interface {
	APIServerLister() configlistersv1.APIServerLister
}

// ObserveTLSGroups observes APIServer.spec.tlsSecurityProfile groups into
// observedConfig.servingInfo.groups.
//
// The TLSGroupPreferences feature gate must be enabled. Named profiles carry
// default groups, so observing them while the gate is off would change etcd
// curve selection for every cluster. While the gate is off this observer
// contributes no groups field.
func ObserveTLSGroups(accessor featuregates.FeatureGateAccess) configobserver.ObserveConfigFunc {
	return func(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]any) (map[string]any, []error) {
		return observeTLSGroups(genericListers, recorder, existingConfig, accessor)
	}
}

func observeTLSGroups(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]any, accessor featuregates.FeatureGateAccess) (ret map[string]any, errs []error) {
	keepExisting := false
	defer func() {
		if keepExisting {
			ret = configobserver.Pruned(existingConfig, groupsPath)
			return
		}
		ret = configobserver.Pruned(ret, groupsPath)
	}()

	enabled, err := tlshelpers.TLSGroupPreferencesEnabled(accessor)
	if err != nil {
		keepExisting = true
		return nil, []error{err}
	}
	if !enabled {
		return map[string]any{}, nil
	}

	currentGroups, _, groupsErr := unstructured.NestedStringSlice(existingConfig, groupsPath...)
	if groupsErr != nil {
		errs = append(errs, groupsErr)
	}

	listers, ok := genericListers.(apiServerLister)
	if !ok {
		keepExisting = true
		return nil, append(errs, errors.New("config observer listers do not expose APIServer"))
	}

	apiServer, err := listers.APIServerLister().Get("cluster")
	if apierrors.IsNotFound(err) {
		klog.Warningf("apiserver.config.openshift.io/cluster: not found")
		apiServer = &configv1.APIServer{}
	} else if err != nil {
		keepExisting = true
		return nil, append(errs, err)
	}

	observedGroups := groupNames(resolveProfileSpec(apiServer.Spec.TLSSecurityProfile))
	observedConfig := map[string]any{}
	if err := unstructured.SetNestedStringSlice(observedConfig, observedGroups, groupsPath...); err != nil {
		keepExisting = true
		return nil, append(errs, err)
	}
	// slices.Equal treats nil and empty as equal, so the first reconcile of an
	// omitted group list is not reported as a change.
	if !slices.Equal(observedGroups, currentGroups) {
		recorder.Eventf("ObserveTLSSecurityProfile", "groups changed to %q", observedGroups)
	}
	return observedConfig, errs
}

// resolveProfileSpec matches library-go's TLS profile resolution. Named
// profiles come from the openshift/api table. A Custom profile uses its inline
// spec. A nil profile, or Custom without a spec, falls back to Intermediate.
func resolveProfileSpec(profile *configv1.TLSSecurityProfile) *configv1.TLSProfileSpec {
	profileType := crypto.DefaultTLSProfileType
	if profile != nil {
		profileType = profile.Type
	}

	var profileSpec *configv1.TLSProfileSpec
	if profileType == configv1.TLSProfileCustomType {
		if profile != nil && profile.Custom != nil {
			profileSpec = &profile.Custom.TLSProfileSpec
		}
	} else {
		profileSpec = configv1.TLSProfiles[profileType]
	}
	if profileSpec == nil {
		profileSpec = configv1.TLSProfiles[crypto.DefaultTLSProfileType]
	}
	return profileSpec
}

func groupNames(profileSpec *configv1.TLSProfileSpec) []string {
	names := make([]string, 0, len(profileSpec.Groups))
	for _, group := range profileSpec.Groups {
		names = append(names, string(group))
	}
	return names
}
