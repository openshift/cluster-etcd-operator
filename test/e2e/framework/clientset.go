package framework

import (
	"os"
	"path"

	clientconfigv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	clientapiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

type ClientSet struct {
	corev1client.CoreV1Interface
	appsv1client.AppsV1Interface
	clientconfigv1.ConfigV1Interface
	clientapiextensionsv1beta1.ApiextensionsV1beta1Interface
}

// NewClientSet returns a *ClientBuilder with the given kubeconfig.
func NewClientSet(kubeconfig string) *ClientSet {
	config, err := NewClientConfigForTest(kubeconfig)
	if err != nil {
		panic(err)
	}

	clientSet := &ClientSet{}
	clientSet.CoreV1Interface = corev1client.NewForConfigOrDie(config)
	clientSet.ConfigV1Interface = clientconfigv1.NewForConfigOrDie(config)
	clientSet.ApiextensionsV1beta1Interface = clientapiextensionsv1beta1.NewForConfigOrDie(config)
	clientSet.AppsV1Interface = appsv1client.NewForConfigOrDie(config)

	return clientSet
}

// NewClientConfigForTest returns a REST config configured to connect to the api server
func NewClientConfigForTest(kubeconfig string) (*rest.Config, error) {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	}

	if kubeconfig == "" {
		kubeconfig = path.Join(os.Getenv("HOME"), ".kube", "config")
	}

	if kubeconfig != "" {
		klog.V(4).Infof("Loading kube client config from path %q", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		klog.V(4).Infof("Using in-cluster kube client config")
		config, err = rest.InClusterConfig()
	}
	return config, err
}
