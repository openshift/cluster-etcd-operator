package ceohelpers

import (
	"fmt"
	"strconv"

	"github.com/ghodss/yaml"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

const (
	clusterConfigName = "cluster-config-v1"
	clusterConfigKey  = "install-config"
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

// GetMastersReplicaCount get number of expected masters statically defined by the controlPlane replicas in the install-config.
func GetMastersReplicaCount(configMapLister v1.ConfigMapLister) (int32, error) {
	klog.V(4).Infof("Getting number of expected masters from %s", clusterConfigName)
	clusterConfig, err := configMapLister.ConfigMaps(operatorclient.TargetNamespace).Get(clusterConfigName)
	if err != nil {
		klog.Errorf("failed to get ConfigMap %s, err %w", clusterConfigName, err)
		return 0, err
	}

	rcD := replicaCountDecoder{}
	if err := yaml.Unmarshal([]byte(clusterConfig.Data[clusterConfigKey]), &rcD); err != nil {
		err := fmt.Errorf("%s key doesn't exist in configmap/%s, err %w", clusterConfigKey, clusterConfigName, err)
		klog.Error(err)
		return 0, err
	}

	replicaCount, err := strconv.Atoi(rcD.ControlPlane.Replicas)
	if err != nil {
		klog.Errorf("failed to convert replica %s, err %w", rcD.ControlPlane.Replicas, err)
		return 0, err
	}
	return int32(replicaCount), nil
}
