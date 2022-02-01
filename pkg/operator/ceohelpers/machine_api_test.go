package ceohelpers

import (
	"testing"

	"github.com/openshift/api/machine/v1beta1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

func TestIsMachineAPIFunctionalWrapper(t *testing.T) {
	machineLabelSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
	if err != nil {
		t.Fatal(err)
	}

	scenarios := []struct {
		name                           string
		expectMachineAPIToBeFunctional bool
		initialObjects                 []runtime.Object
	}{
		// scenario 1
		{
			name:                           "a single master machine in Running state denotes active Machine API",
			initialObjects:                 []runtime.Object{masterMachineFor("m-0", "Running")},
			expectMachineAPIToBeFunctional: true,
		},

		// scenario 2
		{
			name:                           "a single master machine in Running state denotes active Machine API even when machines in different phase are present",
			initialObjects:                 []runtime.Object{masterMachineFor("m-0", "Running"), masterMachineFor("m-1", "Provisioning")},
			expectMachineAPIToBeFunctional: true,
		},

		// scenario 3
		{
			name:                           "a single master machine in Running state denotes active Machine API even when the other machines are in the same phase",
			initialObjects:                 []runtime.Object{masterMachineFor("m-0", "Running"), masterMachineFor("m-1", "Running")},
			expectMachineAPIToBeFunctional: true,
		},

		// scenario 4
		{
			name:           "no machines in Running state denote inactive Machine API",
			initialObjects: []runtime.Object{masterMachineFor("m-0", "Unknown"), masterMachineFor("m-1", "Provisioning")},
		},

		// scenario 5
		{
			name: "non-master machines in Running state denote inactive Machine API",
			initialObjects: []runtime.Object{
				func() *v1beta1.Machine {
					m := masterMachineFor("m-0", "Running")
					m.Labels = map[string]string{}
					return m
				}(),
				func() *v1beta1.Machine {
					m := masterMachineFor("m-1", "Running")
					m.Labels = map[string]string{}
					return m
				}(),
			},
		},

		// scenario 6
		{
			name: "no machines denote inactive Machine API",
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjects {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)

			// act
			target := &MachineAPI{func() bool { return true }, machineLister, machineLabelSelector}
			isMachineAPIFunctional, err := target.IsFunctional()
			if err != nil {
				t.Fatal(err)
			}
			if isMachineAPIFunctional != scenario.expectMachineAPIToBeFunctional {
				t.Errorf("IsMachineAPIFunctionalWrapper function returned = %v, whereas the test expected to get = %v", isMachineAPIFunctional, scenario.expectMachineAPIToBeFunctional)
			}
		})
	}
}

func masterMachineFor(name, phase string) *v1beta1.Machine {
	return &v1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status:     v1beta1.MachineStatus{Phase: &phase},
	}
}
