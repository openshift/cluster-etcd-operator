package ceohelpers

import (
	"context"
	configv1 "github.com/openshift/api/config/v1"
	v1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
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
		Status: configv1.ClusterVersionStatus{
			Capabilities: configv1.ClusterVersionCapabilitiesStatus{
				EnabledCapabilities: []configv1.ClusterVersionCapability{
					configv1.ClusterVersionCapabilityMachineAPI,
				},
			},
		},
	}

	scenarios := []struct {
		name                           string
		expectMachineAPIToBeFunctional bool
		expectMachineAPIToBeEnabled    bool
		expectMachineAPIToBeAvailable  bool
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
			name: "capabilities = None, machine API not enabled and not available",
			initialObjects: []runtime.Object{
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: "version"},
					Spec: configv1.ClusterVersionSpec{
						Capabilities: &configv1.ClusterVersionCapabilitiesSpec{
							BaselineCapabilitySet: "None",
						},
					},
					Status: configv1.ClusterVersionStatus{
						Capabilities: configv1.ClusterVersionCapabilitiesStatus{
							EnabledCapabilities: []configv1.ClusterVersionCapability{},
						},
					},
				},
			},
			expectMachineAPIToBeFunctional: false,
			expectMachineAPIToBeEnabled:    false,
			expectMachineAPIToBeAvailable:  false,
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
						},
					},
					Status: configv1.ClusterVersionStatus{
						Capabilities: configv1.ClusterVersionCapabilitiesStatus{
							EnabledCapabilities: []configv1.ClusterVersionCapability{"MachineAPI"},
						},
					},
				},
				masterMachineFor("m-0", "Running"),
			},
			expectMachineAPIToBeFunctional: true,
			expectMachineAPIToBeEnabled:    true,
			expectMachineAPIToBeAvailable:  true,
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
						},
					},
					Status: configv1.ClusterVersionStatus{
						Capabilities: configv1.ClusterVersionCapabilitiesStatus{
							EnabledCapabilities: []configv1.ClusterVersionCapability{"MachineAPI"},
						},
					},
				},
			},
			expectMachineAPIToBeFunctional: false,
			expectMachineAPIToBeEnabled:    true,
			expectMachineAPIToBeAvailable:  true,
		},

		// scenario 10
		{
			name: "machine API disabled, but API exists",
			initialObjects: []runtime.Object{
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: "version"},
					Spec: configv1.ClusterVersionSpec{
						Capabilities: &configv1.ClusterVersionCapabilitiesSpec{
							BaselineCapabilitySet: "None",
						},
					},
					Status: configv1.ClusterVersionStatus{
						Capabilities: configv1.ClusterVersionCapabilitiesStatus{
							EnabledCapabilities: []configv1.ClusterVersionCapability{},
						},
					},
				},
				masterMachineFor("m-0", "Running"),
			},
			expectMachineAPIToBeFunctional: true,
			expectMachineAPIToBeEnabled:    false,
			expectMachineAPIToBeAvailable:  true,
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
			dynamicClient := &fakeDynamic{scenario.expectMachineAPIToBeAvailable}

			// act
			target := &MachineAPI{
				func() bool { return true },
				func() bool { return true },
				machineLister,
				machineLabelSelector,
				versionLister,
				dynamicClient,
			}
			isMachineAPIFunctional, err := target.IsFunctional()
			require.NoError(t, err)
			require.Equalf(t, scenario.expectMachineAPIToBeFunctional, isMachineAPIFunctional,
				"Functional: function returned = %v, whereas the test expected to get = %v", isMachineAPIFunctional, scenario.expectMachineAPIToBeFunctional)

			// isFunctional already tests isEnabled() implicitly, this here is to test for potential inconsistencies
			isEnabled, err := target.IsEnabled()
			require.NoError(t, err)
			require.Equalf(t, scenario.expectMachineAPIToBeEnabled, isEnabled,
				"Enabled: function returned = %v, whereas the test expected to get = %v", isEnabled, scenario.expectMachineAPIToBeEnabled)

			// isAvailable already tests isEnabled() implicitly, this here is to test for potential inconsistencies
			isAvailable, err := target.IsAvailable()
			require.NoError(t, err)
			require.Equalf(t, scenario.expectMachineAPIToBeAvailable, isAvailable,
				"Available: function returned = %v, whereas the test expected to get = %v", isAvailable, scenario.expectMachineAPIToBeAvailable)
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

func createUnstructuredMachine(apiVersion, name, namespace, ip, nodeName string) unstructured.Unstructured {
	return unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       "Machine",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{},
			"status": map[string]interface{}{
				"addresses": []interface{}{
					map[string]interface{}{
						"address": nodeName,
						"type":    "InternalDNS",
					},
					map[string]interface{}{
						"address": ip,
						"type":    "InternalIP",
					},
				},
				"nodeRef": map[string]interface{}{
					"kind": "Node",
					"name": nodeName,
				},
			},
		},
	}
}

type fakeDynamic struct {
	returnResult bool
}

func (f fakeDynamic) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return fakeDynamicResourceInterface{f.returnResult}
}

type fakeDynamicResourceInterface struct {
	returnResult bool
}

// we're only using the List call, remainder can stay unimplemented

func (f fakeDynamicResourceInterface) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if f.returnResult {
		return &unstructured.UnstructuredList{
			Items: []unstructured.Unstructured{
				createUnstructuredMachine("cluster.x-k8s.io/v1alpha4", "capi-machine1", "capi-machine1", "10.0.128.123", "ip-10-0-128-123.ec2.internal"),
			},
		}, nil
	}

	return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{}}, nil
}

func (f fakeDynamicResourceInterface) Namespace(s string) dynamic.ResourceInterface {
	return fakeDynamicResourceInterface{f.returnResult}
}

func (f fakeDynamicResourceInterface) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	panic("implement me")
}

func (f fakeDynamicResourceInterface) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	panic("implement me")
}
