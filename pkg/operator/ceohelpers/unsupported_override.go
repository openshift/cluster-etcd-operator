package ceohelpers

import (
	"bytes"
	"encoding/json"
	"strconv"

	operatorv1 "github.com/openshift/api/operator/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog"
)

// IsUnsupportedUnsafeEtcd returns true if
// useUnsupportedUnsafeNonHANonProductionUnstableEtcd key is set
// to any parsable value
func IsUnsupportedUnsafeEtcd(spec *operatorv1.StaticPodOperatorSpec) (bool, error) {
	unsupportedConfig := map[string]interface{}{}
	if spec.UnsupportedConfigOverrides.Raw == nil {
		return false, nil
	}

	configJson, err := kyaml.ToJSON(spec.UnsupportedConfigOverrides.Raw)
	if err != nil {
		klog.Warning(err)
		// maybe it's just json
		configJson = spec.UnsupportedConfigOverrides.Raw
	}

	if err := json.NewDecoder(bytes.NewBuffer(configJson)).Decode(&unsupportedConfig); err != nil {
		klog.V(4).Infof("decode of unsupported config failed with error: %v", err)
		return false, err
	}

	// 1. this violates operational best practices for etcd - unstable
	// 2. this allows non-HA configurations which we cannot support in
	//    production - unsafe and non-HA
	// 3. this allows a situation where we can get stuck unable to re-achieve
	//    quorum, resulting in cluster-death - unsafe, non-HA, non-production,
	//    unstable
	// 4. the combination of all these things makes the situation
	//    unsupportable.
	value, found, err := unstructured.NestedFieldNoCopy(unsupportedConfig, "useUnsupportedUnsafeNonHANonProductionUnstableEtcd")
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	switch value.(type) {
	case bool:
		return value.(bool), nil
	case string:
		return strconv.ParseBool(value.(string))
	default:
		return false, nil
	}
}
