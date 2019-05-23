package auth

import (
	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
)

const (
	targetNamespaceName   = "openshift-kube-apiserver"
	oauthMetadataFilePath = "/etc/kubernetes/static-pod-resources/configmaps/oauth-metadata/oauthMetadata"
	configNamespace       = "openshift-config"
	managedNamespace      = "openshift-config-managed"
)

// ObserveAuthMetadata fills in authConfig.OauthMetadataFile with the path for a configMap referenced by the authentication
// config.
func ObserveAuthMetadata(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	listers := genericListers.(configobservation.Listers)
	errs := []error{}
	prevObservedConfig := map[string]interface{}{}

	topLevelMetadataFilePath := []string{"authConfig", "oauthMetadataFile"}
	currentMetadataFilePath, _, err := unstructured.NestedString(existingConfig, topLevelMetadataFilePath...)
	if err != nil {
		errs = append(errs, err)
	}
	if len(currentMetadataFilePath) > 0 {
		if err := unstructured.SetNestedField(prevObservedConfig, currentMetadataFilePath, topLevelMetadataFilePath...); err != nil {
			errs = append(errs, err)
		}
	}

	observedConfig := map[string]interface{}{}
	authConfigNoDefaults, err := listers.AuthConfigLister.Get("cluster")
	if errors.IsNotFound(err) {
		klog.Warningf("authentications.config.openshift.io/cluster: not found")
		return observedConfig, errs
	}
	if err != nil {
		errs = append(errs, err)
		return prevObservedConfig, errs
	}

	authConfig := defaultAuthConfig(authConfigNoDefaults)

	var (
		sourceNamespace string
		sourceConfigMap string
		statusConfigMap string
	)

	specConfigMap := authConfig.Spec.OAuthMetadata.Name

	// TODO: Add a case here for the KeyCloak type.
	switch {
	case len(authConfig.Status.IntegratedOAuthMetadata.Name) > 0 && authConfig.Spec.Type == configv1.AuthenticationTypeIntegratedOAuth:
		statusConfigMap = authConfig.Status.IntegratedOAuthMetadata.Name
	default:
		klog.V(5).Infof("no integrated oauth metadata configmap observed from status")
	}

	// Spec configMap takes precedence over Status.
	switch {
	case len(specConfigMap) > 0:
		sourceConfigMap = specConfigMap
		sourceNamespace = configNamespace
	case len(statusConfigMap) > 0:
		sourceConfigMap = statusConfigMap
		sourceNamespace = managedNamespace
	default:
		klog.V(5).Infof("no authentication config metadata specified")
	}

	// Sync the user or status-specified configMap to the well-known resting place that corresponds to the oauthMetadataFile path.
	// If neither are set, this updates the destination with an empty source, which deletes the destination resource.
	err = listers.ResourceSyncer().SyncConfigMap(
		resourcesynccontroller.ResourceLocation{
			Namespace: targetNamespaceName,
			Name:      "oauth-metadata",
		},
		resourcesynccontroller.ResourceLocation{
			Namespace: sourceNamespace,
			Name:      sourceConfigMap,
		},
	)
	if err != nil {
		errs = append(errs, err)
		return prevObservedConfig, errs
	}

	// Unsets oauthMetadataFile if we had an empty source.
	if len(sourceConfigMap) == 0 {
		return observedConfig, errs
	}

	// Set oauthMetadataFile.
	if err := unstructured.SetNestedField(observedConfig, oauthMetadataFilePath, topLevelMetadataFilePath...); err != nil {
		recorder.Eventf("ObserveAuthMetadataConfigMap", "Failed setting oauthMetadataFile: %v", err)
		errs = append(errs, err)
	}

	return observedConfig, errs
}

func defaultAuthConfig(authConfig *configv1.Authentication) *configv1.Authentication {
	out := authConfig.DeepCopy() // do not mutate informer cache

	if len(out.Spec.Type) == 0 {
		out.Spec.Type = configv1.AuthenticationTypeIntegratedOAuth
	}

	return out
}
