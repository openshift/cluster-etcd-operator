package tools

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	fencingSecretNamePrefix = "fencing-credentials-"
)

var (
	fencingSecretNamePattern = fmt.Sprintf("%s%s", fencingSecretNamePrefix, "%s")
)

func GetFencingSecret(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) (*corev1.Secret, error) {
	secretName := fmt.Sprintf(fencingSecretNamePattern, nodeName)
	secret, err := kubeClient.CoreV1().Secrets("openshift-etcd").Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get secret %s: %v", secretName, err)
		return nil, err
	}
	return secret, nil
}

func IsFencingSecret(secretName string) bool {
	return strings.HasPrefix(secretName, fencingSecretNamePrefix)
}
