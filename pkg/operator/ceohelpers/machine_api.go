package ceohelpers

import (
	"context"
	"fmt"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

// MachineAPIChecker captures a set of functions for working with the Machine API
type MachineAPIChecker interface {
	// IsFunctional checks if the Machine API is functional (that requires it to be either enabled OR available)
	IsFunctional() (bool, error)
	// IsEnabled checks if the Machine API is enabled via CVO
	IsEnabled() (bool, error)
	// IsAvailable checks if the Machine API is available on the API server
	IsAvailable() (bool, error)
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
	hasVersionInformerSyncedFn       func() bool
	hasMasterMachineInformerSyncedFn func() bool
	masterMachineLister              machinelistersv1beta1.MachineLister
	masterMachineSelector            labels.Selector
	versionLister                    configv1listers.ClusterVersionLister
	dynamicClient                    dynamic.Interface
}

var _ MachineAPIChecker = &MachineAPI{}

func NewMachineAPI(masterMachineInformer cache.SharedIndexInformer,
	masterMachineLister machinelistersv1beta1.MachineLister,
	masterMachineSelector labels.Selector,
	versionInformer configv1informers.ClusterVersionInformer,
	dynamicClient dynamic.Interface) *MachineAPI {
	return &MachineAPI{
		hasVersionInformerSyncedFn:       versionInformer.Informer().HasSynced,
		hasMasterMachineInformerSyncedFn: masterMachineInformer.HasSynced,
		masterMachineLister:              masterMachineLister,
		masterMachineSelector:            masterMachineSelector,
		versionLister:                    versionInformer.Lister(),
		dynamicClient:                    dynamicClient,
	}
}

// IsFunctional checks if the Machine API is (enabled OR available) AND functional.
// As of today Machine API is functional when:
// * clusterVersionInformer is synced
// * clusterVersion has MachineAPI as an enabled capability OR the API enabled on the api server
// * MachineInformer is synced
// * we find at least one Machine resources in the Running state.
func (m *MachineAPI) IsFunctional() (bool, error) {
	enabled, err := m.IsEnabled()
	if err != nil {
		return false, err
	}

	if !enabled {
		available, err := m.IsAvailable()
		if err != nil {
			return false, err
		}

		if !available {
			return false, nil
		}
	}

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

// IsEnabled checks if the Machine API is enabled.
// As of today Machine API is enabled when:
// * clusterVersionInformer is synced
// * clusterVersion has "MachineAPI" as an enabled capability
// This method might not be reliable under some configuration, thus it's important to also check whether it IsAvailable
func (m *MachineAPI) IsEnabled() (bool, error) {
	if !m.hasVersionInformerSyncedFn() {
		return false, fmt.Errorf("ClusterVersionInformer is not yet synced")
	}

	clusterVersion, err := m.versionLister.Get("version")
	if err != nil {
		return false, err
	}

	// this is a special case introduced from 4.14 on, where MachineAPI can be optional with clusters installed using capabilities.baselineCapabilitySet=None
	// on upgrades from prior versions of OpenShift the status should have MachineAPI listed as an EnabledCapabilities
	if clusterVersion != nil && clusterVersion.Spec.Capabilities != nil && string(clusterVersion.Spec.Capabilities.BaselineCapabilitySet) == "None" {
		machineAPIEnabled := false
		for _, capability := range clusterVersion.Status.Capabilities.EnabledCapabilities {
			if string(capability) == "MachineAPI" {
				machineAPIEnabled = true
				break
			}
		}

		if !machineAPIEnabled {
			return false, nil
		}
	}

	return true, nil
}

// IsAvailable checks if the Machine API is available as an API on the API server.
func (m *MachineAPI) IsAvailable() (bool, error) {
	resource := m.dynamicClient.Resource(schema.GroupVersionResource{
		Group:    "machine.openshift.io",
		Version:  "v1beta1",
		Resource: "MachineList",
	})

	list, err := resource.List(context.Background(), metav1.ListOptions{LabelSelector: m.masterMachineSelector.String()})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	if len(list.Items) > 0 {
		return true, nil
	}

	return false, nil
}
