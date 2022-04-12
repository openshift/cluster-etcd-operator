package ceohelpers

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/controlplanereplicascount"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// MachineDeletionHookName holds a name of the Machine Deletion Hook
const MachineDeletionHookName = "EtcdQuorumOperator"

// MachineDeletionHookOwner holds an owner of the Machine Deletion Hook
const MachineDeletionHookOwner = "clusteroperator/etcd"

// ReadDesiredControlPlaneReplicasCount reads the current Control Plane replica count
func ReadDesiredControlPlaneReplicasCount(operatorClient v1helpers.StaticPodOperatorClient) (int, error) {
	operatorSpec, _, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return 0, err
	}

	unstructuredMergedCfg, err := resourcemerge.MergeProcessConfig(
		nil,
		operatorSpec.ObservedConfig.Raw,
		operatorSpec.UnsupportedConfigOverrides.Raw,
	)
	if err != nil {
		return 0, err
	}

	var unstructuredConfig map[string]interface{}
	if err := json.Unmarshal(unstructuredMergedCfg, &unstructuredConfig); err != nil {
		return 0, fmt.Errorf("failed to unmarshal merged operator's config, err: %w", err)
	}

	// read the current value
	// unmarshalling JSON into an interface value always stores JSON number as a float64
	currentControlPlaneReplicas, _, err := unstructured.NestedFloat64(unstructuredConfig, controlplanereplicascount.ControlPlaneReplicasPath...)
	if err != nil {
		return 0, fmt.Errorf("unable to extract %q from the existing config: %w", controlplanereplicascount.ControlPlaneReplicasPath, err)
	}

	return int(currentControlPlaneReplicas), nil
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

// IndexMachinesByNodeInternalIP maps machines to IPs
//
// Note that a machine can have multiple internal IPs
func IndexMachinesByNodeInternalIP(machines []*machinev1beta1.Machine) map[string]*machinev1beta1.Machine {
	index := map[string]*machinev1beta1.Machine{}
	for _, machine := range machines {
		for _, addr := range machine.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				index[addr.Address] = machine
				// do not stop on first match
				// machines can have multiple network interfaces
			}
		}
	}
	return index
}

func memberToURL(member *etcdserverpb.Member) (string, error) {
	if len(member.PeerURLs) == 0 {
		return "", fmt.Errorf("unable to extract member's URL address, it has an empty PeerURLs field, member name: %v, id: %v", member.Name, member.ID)
	}
	return member.PeerURLs[0], nil
}