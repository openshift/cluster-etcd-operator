package config

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type secretConfig map[string]map[string]interface{}

type FencingConfig struct {
	FencingID            string
	FencingDeviceType    string
	FencingDeviceOptions map[string]string
}

type ClusterConfig struct {
	NodeName1      string
	NodeName2      string
	NodeIP1        string
	NodeIP2        string
	EtcdPullSpec   string
	FencingConfigs []FencingConfig
}

// GetClusterConfig creates an operator specific view of the config
func GetClusterConfig(ctx context.Context, kubeClient kubernetes.Interface, etcdImagePullSpec string) (ClusterConfig, error) {

	klog.Info("Creating HA Cluster Config")
	clusterCfg := ClusterConfig{}
	clusterCfg.EtcdPullSpec = etcdImagePullSpec

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

	fcs, err := getFencingConfigs(ctx, kubeClient)
	if err != nil {
		return clusterCfg, err
	}
	clusterCfg.FencingConfigs = fcs

	klog.V(4).Infof("FencingConfigs: %+v", clusterCfg.FencingConfigs)

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

func getFencingConfigs(ctx context.Context, kubeClient kubernetes.Interface) ([]FencingConfig, error) {
	secret, err := kubeClient.CoreV1().Secrets("openshift-tnf-operator").Get(ctx, "tnf-fencing-config", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	configYaml := string(secret.Data["config.yaml"])
	return parseConfigYaml(configYaml)
}

func parseConfigYaml(configYaml string) ([]FencingConfig, error) {

	// TODO rework once we have the final secret provided by the installer

	fcs := make([]FencingConfig, 0)

	var config secretConfig
	err := yaml.Unmarshal([]byte(configYaml), &config)
	if err != nil {
		return nil, err
	}
	for node, values := range config {

		// we need to split the redfish address, e.g.
		// redfish+https://192.168.111.1:8000/redfish/v1/Systems/b21cdcd8-53ae-4b72-a240-a8230ed74346

		re := regexp.MustCompile(`.*//(.*):(.*)/(redfish.*)`)
		address := re.FindStringSubmatch(values["address"].(string))

		fc := FencingConfig{
			FencingID:         fmt.Sprintf("%s_%s", node, "redfish"),
			FencingDeviceType: "fence_redfish",
			FencingDeviceOptions: map[string]string{
				"ip":             address[1],
				"ipport":         address[2],
				"systems_uri":    address[3],
				"username":       values["username"].(string),
				"password":       values["password"].(string),
				"pcmk_host_list": node,
			},
		}
		if values["sslInsecure"] != nil && values["sslInsecure"].(bool) {
			fc.FencingDeviceOptions["ssl_insecure"] = ""
		}
		fcs = append(fcs, fc)
	}

	return fcs, nil
}
