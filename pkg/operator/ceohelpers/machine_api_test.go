package ceohelpers

import (
	configv1 "github.com/openshift/api/config/v1"
	v1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)

	clusterVersion := &configv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "version"},
		Spec:       configv1.ClusterVersionSpec{},
	}

	scenarios := []struct {
		name                           string
		expectMachineAPIToBeFunctional bool
		expectMachineAPIToBeEnabled    bool
		initialObjects                 []runtime.Object
	}{
		// scenario 1
		{
			name:                           "a single master machine in Running state denotes active Machine API",
			initialObjects:                 []runtime.Object{clusterVersion, masterMachineFor("m-0", "Running")},
			expectMachineAPIToBeFunctional: true,
			expectMachineAPIToBeEnabled:    true,
		},

		// scenario 2
		{
			name:                           "a single master machine in Running state denotes active Machine API even when machines in different phase are present",
			initialObjects:                 []runtime.Object{clusterVersion, masterMachineFor("m-0", "Running"), masterMachineFor("m-1", "Provisioning")},
			expectMachineAPIToBeFunctional: true,
			expectMachineAPIToBeEnabled:    true,
		},

		// scenario 3
		{
			name:                           "a single master machine in Running state denotes active Machine API even when the other machines are in the same phase",
			initialObjects:                 []runtime.Object{clusterVersion, masterMachineFor("m-0", "Running"), masterMachineFor("m-1", "Running")},
			expectMachineAPIToBeFunctional: true,
			expectMachineAPIToBeEnabled:    true,
		},

		// scenario 4
		{
			name:                        "no machines in Running state denote inactive Machine API",
			initialObjects:              []runtime.Object{clusterVersion, masterMachineFor("m-0", "Unknown"), masterMachineFor("m-1", "Provisioning")},
			expectMachineAPIToBeEnabled: true,
		},

		// scenario 5
		{
			name: "non-master machines in Running state denote inactive Machine API",
			initialObjects: []runtime.Object{
				clusterVersion,
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
			expectMachineAPIToBeEnabled: true,
		},

		// scenario 6
		{
			name:                        "no machines denote inactive Machine API",
			initialObjects:              []runtime.Object{clusterVersion},
			expectMachineAPIToBeEnabled: true,
		},

		// scenario 7
		{
			name: "capabilities = None, machine API not enabled",
			initialObjects: []runtime.Object{
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: "version"},
					Spec: configv1.ClusterVersionSpec{
						Capabilities: &configv1.ClusterVersionCapabilitiesSpec{
							BaselineCapabilitySet:         "None",
							AdditionalEnabledCapabilities: []configv1.ClusterVersionCapability{},
						},
					},
				},
			},
			expectMachineAPIToBeFunctional: false,
			expectMachineAPIToBeEnabled:    false,
		},

		// scenario 8
		{
			name: "capabilities = None, machine API enabled with running machine",
			initialObjects: []runtime.Object{
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: "version"},
					Spec: configv1.ClusterVersionSpec{
						Capabilities: &configv1.ClusterVersionCapabilitiesSpec{
							BaselineCapabilitySet: "None",
							AdditionalEnabledCapabilities: []configv1.ClusterVersionCapability{
								"MachineAPI",
							},
						},
					},
				},
				masterMachineFor("m-0", "Running"),
			},
			expectMachineAPIToBeFunctional: true,
			expectMachineAPIToBeEnabled:    true,
		},

		// scenario 9
		{
			name: "capabilities = None, machine API enabled, no running machine",
			initialObjects: []runtime.Object{
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: "version"},
					Spec: configv1.ClusterVersionSpec{
						Capabilities: &configv1.ClusterVersionCapabilitiesSpec{
							BaselineCapabilitySet: "None",
							AdditionalEnabledCapabilities: []configv1.ClusterVersionCapability{
								"MachineAPI",
							},
						},
					},
				},
			},
			expectMachineAPIToBeFunctional: false,
			expectMachineAPIToBeEnabled:    true,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjects {
				require.NoError(t, indexer.Add(initialObj))
			}
			machineLister := machinelistersv1beta1.NewMachineLister(indexer)
			versionLister := v1.NewClusterVersionLister(indexer)

			// act
			target := &MachineAPI{
				func() bool { return true },
				func() bool { return true },
				machineLister,
				machineLabelSelector,
				versionLister}
			isMachineAPIFunctional, err := target.IsFunctional()
			require.NoError(t, err)
			require.Equalf(t, scenario.expectMachineAPIToBeFunctional, isMachineAPIFunctional,
				"function returned = %v, whereas the test expected to get = %v", isMachineAPIFunctional, scenario.expectMachineAPIToBeFunctional)

			// isFunctional already tests isEnabled() implicitly, this here is to test for potential inconsistencies
			isEnabled, err := target.IsEnabled()
			require.NoError(t, err)
			require.Equalf(t, scenario.expectMachineAPIToBeEnabled, isEnabled,
				"function returned = %v, whereas the test expected to get = %v", isEnabled, scenario.expectMachineAPIToBeEnabled)
		})
	}
}

// TestMachineSyncErrorsOnEnableCheck should ensure we never make a decision without synchronized CVO. This is most important for starter.go.
func TestMachineSyncErrorsOnEnableCheck(t *testing.T) {
	target := &MachineAPI{
		hasVersionInformerSyncedFn: func() bool { return false },
	}

	enabled, err := target.IsEnabled()
	require.False(t, enabled)
	require.EqualError(t, err, "ClusterVersionInformer is not yet synced")
}

func masterMachineFor(name, phase string) *v1beta1.Machine {
	return &v1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status:     v1beta1.MachineStatus{Phase: &phase},
	}
}
