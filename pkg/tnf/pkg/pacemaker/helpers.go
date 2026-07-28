package pacemaker

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PacemakerListWatch wraps cache.ListWatch to opt out of WatchList semantics.
// The OCP apiextensions-apiserver does not enable the server-side WatchList gate,
// so the reflector's sendInitialEvents request is rejected. This wrapper tells
// the reflector to skip watchList() and use list+watch directly.
type PacemakerListWatch struct {
	cache.ListWatch
}

func (PacemakerListWatch) IsWatchListSemanticsUnSupported() bool { return true }

// SanitizeListOptions removes fields not supported by older Kubernetes versions.
// sendInitialEvents and resourceVersionMatch were added in 1.27+ and cause errors on older clusters.
// This helper ensures informer ListWatch functions work across all supported Kubernetes versions.
func SanitizeListOptions(options metav1.ListOptions) metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector:       options.LabelSelector,
		FieldSelector:       options.FieldSelector,
		Watch:               options.Watch,
		AllowWatchBookmarks: options.AllowWatchBookmarks,
		ResourceVersion:     options.ResourceVersion,
		TimeoutSeconds:      options.TimeoutSeconds,
		Limit:               options.Limit,
		Continue:            options.Continue,
	}
}

// getKubeConfig returns in-cluster Kubernetes REST config.
// No kubeconfig file fallback - use clientcmd package if needed.
func getKubeConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config (is this running inside a pod?): %w", err)
	}
	return config, nil
}

// CreatePacemakerRESTClient creates REST client for PacemakerStatus CRs.
func CreatePacemakerRESTClient(baseConfig *rest.Config) (rest.Interface, error) {
	if baseConfig == nil {
		return nil, fmt.Errorf("baseConfig cannot be nil")
	}

	scheme := runtime.NewScheme()
	if err := pacmkrv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add PacemakerStatus scheme: %w", err)
	}

	pacemakerConfig := rest.CopyConfig(baseConfig)
	pacemakerConfig.GroupVersion = &pacmkrv1.SchemeGroupVersion
	pacemakerConfig.APIPath = KubernetesAPIPath
	pacemakerConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme)
	pacemakerConfig.ContentConfig.ContentType = "application/json"

	restClient, err := rest.RESTClientFor(pacemakerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST client for PacemakerStatus: %w", err)
	}

	return restClient, nil
}
