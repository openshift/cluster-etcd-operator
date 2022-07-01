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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"

	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/controlplanereplicascount"
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
// Note that a machine can have multiple internal IPs with different types (v4/v6)
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

// CurrentMemberMachinesWithDeletionHooks returns machines with the deletion hooks from the lister
func CurrentMemberMachinesWithDeletionHooks(machineSelector labels.Selector, machineLister machinelistersv1beta1.MachineLister) ([]*machinev1beta1.Machine, error) {
	machines, err := machineLister.List(machineSelector)
	if err != nil {
		return nil, err
	}
	return FilterMachinesWithMachineDeletionHook(machines), nil
}

// FindMachineByNodeInternalIP finds the machine that matches the given nodeInternalIP
// is safe because the MAO:
//  syncs the addresses in the Machine with those assigned to real nodes by the cloud provider,
//  checks that the Machine and Node lists match before issuing a serving certification for the kubelet
//  when the host disappears from the cloud side, it stops updating the Machine so the addresses and information
//  should persist there as a tombstone as the Machine is marked Failed
func FindMachineByNodeInternalIP(nodeInternalIP string, machineSelector labels.Selector, machineLister machinelistersv1beta1.MachineLister) (*machinev1beta1.Machine, error) {
	machines, err := machineLister.List(machineSelector)
	if err != nil {
		return nil, err
	}

	for _, machine := range machines {
		for _, addr := range machine.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				if addr.Address == nodeInternalIP {
					return machine, nil
				}
			}
		}
	}
	return nil, nil
}

func memberToURL(member *etcdserverpb.Member) (string, error) {
	if len(member.PeerURLs) == 0 {
		return "", fmt.Errorf("unable to extract member's URL address, it has an empty PeerURLs field, member name: %v, id: %v", member.Name, member.ID)
	}
	return member.PeerURLs[0], nil
}

func VotingMemberIPListSet(configMapLister corev1listers.ConfigMapNamespaceLister) (sets.String, error) {
	etcdEndpointsConfigMap, err := configMapLister.Get("etcd-endpoints")
	if err != nil {
		return sets.NewString(), err // should not happen
	}
	currentVotingMemberIPListSet := sets.NewString()
	for _, votingMemberIP := range etcdEndpointsConfigMap.Data {
		currentVotingMemberIPListSet.Insert(votingMemberIP)
	}

	return currentVotingMemberIPListSet, nil
}
