package tools

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	fencingSecretNamePrefix = "fencing-credentials-"
)

var (
	fencingSecretNamePattern = fmt.Sprintf("%s%s", fencingSecretNamePrefix, "%s")
)

func GetFencingSecret(ctx context.Context, kubeClient kubernetes.Interface, dyClient dynamic.Interface, nodeName string) (*corev1.Secret, error) {
	secretName := fmt.Sprintf(fencingSecretNamePattern, nodeName)
	secret, err := kubeClient.CoreV1().Secrets("openshift-etcd").Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		return secret, nil
	}
	if !errors.IsNotFound(err) {
		klog.Errorf("Failed to get secret %s: %v", secretName, err)
		return nil, err
	}

	klog.Infof("Fencing secret %s not found, trying MAC-based lookup for node %s", secretName, nodeName)

	macs, err := GetNodeMACAddresses(ctx, kubeClient, dyClient, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve MAC addresses for node %s: %w", nodeName, err)
	}

	for _, mac := range macs {
		macHash, err := HashMAC(mac)
		if err != nil {
			klog.Warningf("Skipping invalid MAC %s for node %s: %v", mac, nodeName, err)
			continue
		}
		secretName = fmt.Sprintf(fencingSecretNamePattern, macHash)
		klog.Infof("Looking up fencing secret %s for node %s (MAC %s)", secretName, nodeName, mac)

		secret, err = kubeClient.CoreV1().Secrets("openshift-etcd").Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			return secret, nil
		}
		if !errors.IsNotFound(err) {
			klog.Errorf("Failed to get secret %s: %v", secretName, err)
			return nil, err
		}
	}

	return nil, fmt.Errorf("no fencing secret found for node %s after trying %d MAC address(es)", nodeName, len(macs))
}

func IsFencingSecret(secretName string) bool {
	return strings.HasPrefix(secretName, fencingSecretNamePrefix)
}
