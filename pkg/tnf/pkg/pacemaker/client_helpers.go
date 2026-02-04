package pacemaker

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// getKubeConfig returns a Kubernetes REST config for in-cluster use.
// This function is designed for code running inside a Kubernetes pod (e.g., CronJob).
// It does not support kubeconfig file fallback - if you need that, use the
// clientcmd package directly with NewDefaultClientConfigLoadingRules().
func getKubeConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config (is this running inside a pod?): %w", err)
	}
	return config, nil
}

// createPacemakerRESTClient creates a REST client configured for PacemakerStatus CRs.
// It takes a base Kubernetes REST config and configures it with the necessary
// scheme, group version, and serializer for the PacemakerStatus CRD.
func createPacemakerRESTClient(baseConfig *rest.Config) (rest.Interface, error) {
	if baseConfig == nil {
		return nil, fmt.Errorf("baseConfig cannot be nil")
	}

	// Set up the scheme for PacemakerStatus CRD
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add PacemakerStatus scheme: %w", err)
	}

	// Configure the REST client for the PacemakerStatus CRD
	pacemakerConfig := rest.CopyConfig(baseConfig)
	pacemakerConfig.GroupVersion = &v1alpha1.SchemeGroupVersion
	pacemakerConfig.APIPath = KubernetesAPIPath
	pacemakerConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme)
	pacemakerConfig.ContentConfig.ContentType = "application/json"

	restClient, err := rest.RESTClientFor(pacemakerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST client for PacemakerStatus: %w", err)
	}

	return restClient, nil
}
