package ceohelpers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ghodss/yaml"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	clusterConfigName      = "cluster-config-v1"
	clusterConfigKey       = "install-config"
	clusterConfigNamespace = "kube-system"
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

// GetMastersReplicaCount get number of expected masters statically defined by the controlPlane replicas in the install-config.
func GetMastersReplicaCount(ctx context.Context, kubeClient kubernetes.Interface) (int, error) {
	klog.Infof("Getting number of expected masters from %s", clusterConfigName)
	clusterConfig, err := kubeClient.CoreV1().ConfigMaps(clusterConfigNamespace).Get(ctx, clusterConfigName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get ConfigMap %s: %v", clusterConfigName, err)
		return 0, err
	}

	rcD := replicaCountDecoder{}
	installConfigData, ok := clusterConfig.Data[clusterConfigKey]
	if !ok {
		err := fmt.Errorf("%s key doesn't exist in configmap/%s: %v", clusterConfigKey, clusterConfigName, err)
		klog.Error(err)
		return 0, err
	}
	if err := yaml.Unmarshal([]byte(installConfigData), &rcD); err != nil {
		err := fmt.Errorf("failed to unmarshal install-config data from configmap/%s: %v", clusterConfigName, err)
		klog.Error(err)
		return 0, err
	}

	replicaCount, err := strconv.Atoi(rcD.ControlPlane.Replicas)
	if err != nil {
		klog.Errorf("failed to convert replica %s, err %w", rcD.ControlPlane.Replicas, err)
		return 0, err
	}
	return replicaCount, nil
}
