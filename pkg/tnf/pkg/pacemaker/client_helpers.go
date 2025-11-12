package pacemaker

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	v1alpha1 "github.com/openshift/api/etcd/v1alpha1"
)

// getKubeConfig returns a Kubernetes REST config, trying in-cluster config first,
// then falling back to kubeconfig file from environment or default location.
func getKubeConfig() (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		kubeconfig := os.Getenv(envKubeconfig)
		if kubeconfig == "" {
			kubeconfig = os.Getenv(envHome) + defaultKubeconfigPath
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create kubernetes config: %w", err)
		}
	}
	return config, nil
}

// createPacemakerRESTClient creates a REST client configured for PacemakerStatus CRs.
// It takes a base Kubernetes REST config and configures it with the necessary
// scheme, group version, and serializer for the PacemakerStatus CRD.
func createPacemakerRESTClient(baseConfig *rest.Config) (rest.Interface, error) {
	// Set up the scheme for PacemakerStatus CRD
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add PacemakerStatus scheme: %w", err)
	}

	// Configure the REST client for the PacemakerStatus CRD
	pacemakerConfig := rest.CopyConfig(baseConfig)
	pacemakerConfig.GroupVersion = &v1alpha1.SchemeGroupVersion
	pacemakerConfig.APIPath = kubernetesAPIPath
	pacemakerConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme)
	pacemakerConfig.ContentConfig.ContentType = "application/json"

	restClient, err := rest.RESTClientFor(pacemakerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST client for PacemakerStatus: %w", err)
	}

	return restClient, nil
}
