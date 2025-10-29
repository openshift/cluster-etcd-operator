package config

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
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
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/master",
	})
	if err != nil {
		return clusterCfg, err
	}
	if len(nodes.Items) != 2 {
		return clusterCfg, fmt.Errorf("expected 2 nodes, got %d", len(nodes.Items))
	}

	sort.Slice(nodes.Items, func(i, j int) bool {
		return nodes.Items[i].Name < nodes.Items[j].Name
	})

	for i, node := range nodes.Items {
		switch i {
		case 0:
			ip, err := tools.GetNodeIPForPacemaker(node)
			if err != nil {
				return clusterCfg, err
			}
			clusterCfg.NodeName1 = node.Name
			clusterCfg.NodeIP1 = ip
		case 1:
			ip, err := tools.GetNodeIPForPacemaker(node)
			if err != nil {
				return clusterCfg, err
			}
			clusterCfg.NodeName2 = node.Name
			clusterCfg.NodeIP2 = ip
		}
	}

	return clusterCfg, nil
}
