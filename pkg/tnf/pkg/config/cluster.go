package config

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type ClusterConfig struct {
	NodeName1 string
	NodeName2 string
	NodeIP1   string
	NodeIP2   string
}

// GetClusterConfig creates an operator specific view of the config
func GetClusterConfig(ctx context.Context, kubeClient kubernetes.Interface) (ClusterConfig, error) {

	klog.Info("Creating HA Cluster Config")
	clusterCfg := ClusterConfig{}

	// Get nodes
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return clusterCfg, err
	}

	sort.Slice(nodes.Items, func(i, j int) bool {
		return nodes.Items[i].Name < nodes.Items[j].Name
	})

	for i, node := range nodes.Items {
		switch i {
		case 0:
			clusterCfg.NodeName1 = node.Name
			clusterCfg.NodeIP1 = getInternalIP(node.Status.Addresses)
		case 1:
			clusterCfg.NodeName2 = node.Name
			clusterCfg.NodeIP2 = getInternalIP(node.Status.Addresses)
		}
	}

	return clusterCfg, nil
}

func getInternalIP(addresses []corev1.NodeAddress) string {
	for _, addr := range addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	// fallback...
	return addresses[0].Address
}
