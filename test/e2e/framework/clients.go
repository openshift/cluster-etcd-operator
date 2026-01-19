package framework

import (
	configversionedclientv1alpha1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/stretchr/testify/require"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"testing"
)

func NewOperatorClient(t *testing.T) *operatorversionedclient.Clientset {
	kubeConfig, err := NewClientConfigForTest("")
	require.NoError(t, err)

	operatorConfigClient, err := operatorversionedclient.NewForConfig(kubeConfig)
	require.NoError(t, err)

	return operatorConfigClient
}

func NewConfigClient(t *testing.T) *configversionedclientv1alpha1.ConfigV1alpha1Client {
	kubeConfig, err := NewClientConfigForTest("")
	require.NoError(t, err)

	c, err := configversionedclientv1alpha1.NewForConfig(kubeConfig)
	require.NoError(t, err)

	return c
}

func NewBatchClient(t *testing.T) *batchv1client.BatchV1Client {
	kubeConfig, err := NewClientConfigForTest("")
	require.NoError(t, err)

	c, err := batchv1client.NewForConfig(kubeConfig)
	require.NoError(t, err)

	return c
}

func NewCoreClient(t *testing.T) *corev1client.CoreV1Client {
	kubeConfig, err := NewClientConfigForTest("")
	require.NoError(t, err)

	c, err := corev1client.NewForConfig(kubeConfig)
	require.NoError(t, err)

	return c
}
