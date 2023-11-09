package ceohelpers

import (
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

// MachineAPIChecker captures a set of functions for working with the Machine API
type MachineAPIChecker interface {
	// IsFunctional checks if the Machine API is functional
	IsFunctional() (bool, error)
}

// MachineAPI a simple struct that helps determine if the Machine API is functional
//
// Note:
//
//	since this type needs to take into account only master machine objects
//	make sure machineInformer contain only filtered data
//	otherwise it might be expensive to react to every node update in larger installations
//
//	as a safety net this type requires masterMachineSelector, just in case a caller won't provide a filtered informer
type MachineAPI struct {
	hasMasterMachineInformerSyncedFn func() bool
	masterMachineLister              machinelistersv1beta1.MachineLister
	masterMachineSelector            labels.Selector
}

var _ MachineAPIChecker = &MachineAPI{}

func NewMachineAPI(masterMachineInformer cache.SharedIndexInformer, masterMachineLister machinelistersv1beta1.MachineLister, masterMachineSelector labels.Selector) *MachineAPI {
	return &MachineAPI{
		hasMasterMachineInformerSyncedFn: masterMachineInformer.HasSynced,
		masterMachineLister:              masterMachineLister,
		masterMachineSelector:            masterMachineSelector,
	}
}

// IsFunctional checks if the Machine API is functional.
// As of today Machine API is functional when we find Machines resources in the Running state.
func (m *MachineAPI) IsFunctional() (bool, error) {
	if !m.hasMasterMachineInformerSyncedFn() {
		return false, nil
	}
	machines, err := m.masterMachineLister.List(m.masterMachineSelector)
	if err != nil {
		return false, err
	}
	if len(machines) == 0 {
		return false, nil
	}

	// we expect just a single machine to be in the Running state
	for _, machine := range machines {
		phase := pointer.StringDeref(machine.Status.Phase, "")
		if phase == "Running" {
			return true, nil
		}
	}

	return false, nil
}
