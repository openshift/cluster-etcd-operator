package ceohelpers

import (
	"fmt"
	"net"
	"net/url"

	"github.com/ghodss/yaml"
	"go.etcd.io/etcd/api/v3/etcdserverpb"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// MachineDeletionHookName holds a name of the Machine Deletion Hook
const MachineDeletionHookName = "EtcdQuorumOperator"

// MachineDeletionHookOwner holds an owner of the Machine Deletion Hook
const MachineDeletionHookOwner = "clusteroperator/etcd"

// ReadDesiredControlPlaneReplicasCount simply reds the desired Control Plane replica count from the cluster-config-v1 configmap in the kube-system namespace
func ReadDesiredControlPlaneReplicasCount(configMapListerForKubeSystemNamespace corev1listers.ConfigMapNamespaceLister) (int, error) {
	const clusterConfigConfigMapName = "cluster-config-v1"
	const installConfigKeyName = "install-config"

	clusterConfig, err := configMapListerForKubeSystemNamespace.Get(clusterConfigConfigMapName)
	if err != nil {
		return 0, err
	}

	rawInstallConfig, hasInstallConfig := clusterConfig.Data[installConfigKeyName]
	if !hasInstallConfig {
		return 0, fmt.Errorf("missing required key: %s for cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}

	var unstructuredInstallConfig map[string]interface{}
	if err := yaml.Unmarshal([]byte(rawInstallConfig), &unstructuredInstallConfig); err != nil {
		return 0, fmt.Errorf("failed to unmarshal key: %s to yaml from cm: %s/kube-system, err: %w", installConfigKeyName, clusterConfigConfigMapName, err)
	}

	unstructuredControlPlaneConfig, exists, err := unstructured.NestedMap(unstructuredInstallConfig, "controlPlane")
	if err != nil {
		return 0, fmt.Errorf("failed to extract field: %s.controlPlane from cm: %s/kube-system, err: %v", installConfigKeyName, clusterConfigConfigMapName, err)
	}
	if !exists {
		return 0, fmt.Errorf("required field: %s.controlPlane doesn't exist in cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}
	// unmarshalling JSON into an interface value always stores JSON number as a float64
	desiredReplicas, exists, err := unstructured.NestedFloat64(unstructuredControlPlaneConfig, "replicas")
	if err != nil {
		return 0, fmt.Errorf("failed to extract field: %s.controlPlane.replicas from cm: %s/kube-system, err: %v", installConfigKeyName, clusterConfigConfigMapName, err)
	}
	if !exists {
		return 0, fmt.Errorf("required field: %s.controlPlane.replicas doesn't exist in cm: %s/kube-system", installConfigKeyName, clusterConfigConfigMapName)
	}

	return int(desiredReplicas), nil
}

// MemberToNodeInternalIP extracts assigned IP address from the given member
func MemberToNodeInternalIP(member *etcdserverpb.Member) (string, error) {
	memberURLAsString, err := memberToURL(member)
	if err != nil {
		return "", err
	}
	memberURL, err := url.Parse(memberURLAsString)
	if err != nil {
		return "", err
	}

	host, _, err := net.SplitHostPort(memberURL.Host)
	if err != nil {
		return "", err
	}
	return host, nil
}

// FilterMachinesWithMachineDeletionHook a convenience function for filtering only machines with the machine deletion hook present
func FilterMachinesWithMachineDeletionHook(machines []*machinev1beta1.Machine) []*machinev1beta1.Machine {
	var filteredMachines []*machinev1beta1.Machine
	for _, machine := range machines {
		if HasMachineDeletionHook(machine) {
			filteredMachines = append(filteredMachines, machine)
		}
	}
	return filteredMachines
}

// FilterMachinesPendingDeletion a convenience function for filtering machines pending deletion
func FilterMachinesPendingDeletion(machines []*machinev1beta1.Machine) []*machinev1beta1.Machine {
	var filteredMachines []*machinev1beta1.Machine
	for _, machine := range machines {
		if machine.DeletionTimestamp != nil {
			filteredMachines = append(filteredMachines, machine)
		}
	}
	return filteredMachines
}

// HasMachineDeletionHook simply checks if the given machine has the machine deletion hook present
func HasMachineDeletionHook(machine *machinev1beta1.Machine) bool {
	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == MachineDeletionHookName && hook.Owner == MachineDeletionHookOwner {
			return true
		}
	}
	return false
}

func memberToURL(member *etcdserverpb.Member) (string, error) {
	if len(member.PeerURLs) == 0 {
		return "", fmt.Errorf("unable to extract member's URL address, it has an empty PeerURLs field, member name: %v, id: %v", member.Name, member.ID)
	}
	return member.PeerURLs[0], nil
}
