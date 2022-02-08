package machinedeletionhooks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	machinev1beta1fakeclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
)

func TestSyncMethod(t *testing.T) {
	eventRecorder := events.NewRecorder(k8sfakeclient.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-machine-deletion-hooks-controller", &corev1.ObjectReference{})
	fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: func() (bool, error) {
		return false, nil
	}}
	target := machineDeletionHooksController{
		machineAPIChecker: fakeMachineAPIChecker,
	}
	err := target.sync(context.TODO(), factory.NewSyncContext("test-cluster-member-removal-controller", eventRecorder))
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddDeletionHookToVotingMembers(t *testing.T) {
	scenarios := []struct {
		name                           string
		initialObjectsForMachineLister []runtime.Object
		// expectedActions holds actions to be verified in the form of "verb:resource:namespace"
		expectedActions []string
		expectedError   error
		validateFunc    func(ts *testing.T, machineClientActions []clientgotesting.Action)
	}{
		{
			name: "no machines",
		},

		{
			name: "a machine missing the hooks",
			initialObjectsForMachineLister: func() []runtime.Object {
				return []runtime.Object{
					machineFor("m-0", "10.0.139.78"),
				}
			}(),
			expectedActions: []string{"update:machines:"},
			validateFunc: func(t *testing.T, machineClientActions []clientgotesting.Action) {
				wasMachineUpdated := false
				for _, action := range machineClientActions {
					if action.Matches("update", "machines") {
						updateAction := action.(clientgotesting.UpdateAction)
						updatedMachine := updateAction.GetObject().(*machinev1beta1.Machine)
						if !hasMachineDeletionHook(updatedMachine) {
							t.Fatalf("machine %v is missing required machine deletion hooks", updatedMachine)
						}
						wasMachineUpdated = true
						break
					}
				}
				if !wasMachineUpdated {
					t.Errorf("expected to see an updated machine but didn't get any, fake machine client actions: %v", machineClientActions)
				}
			},
		},

		{
			name: "a machine with broken hook",
			initialObjectsForMachineLister: func() []runtime.Object {
				m := machineFor("m-0", "10.0.139.78")
				m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "incorrect-owner"})
				m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "TestHook", Owner: "TestValue"})
				return []runtime.Object{m}
			}(),
			expectedActions: []string{"update:machines:"},
			validateFunc: func(t *testing.T, machineClientActions []clientgotesting.Action) {
				wasMachineUpdated := false
				for _, action := range machineClientActions {
					if action.Matches("update", "machines") {
						updateAction := action.(clientgotesting.UpdateAction)
						updatedMachine := updateAction.GetObject().(*machinev1beta1.Machine)
						if !hasMachineDeletionHook(updatedMachine) {
							t.Fatalf("machine %v is missing required machine deletion hooks", updatedMachine)
						}
						foundTestHook := false
						for _, hook := range updatedMachine.Spec.LifecycleHooks.PreDrain {
							if hook.Name == "TestHook" && hook.Owner == "TestValue" {
								foundTestHook = true
							}
						}
						if !foundTestHook {
							t.Fatalf("didn't find TestHook LifecycleHook for machine %v", updatedMachine)
						}
						wasMachineUpdated = true
						break
					}
				}
				if !wasMachineUpdated {
					t.Errorf("expected to see an updated machine but didn't get any, fake machine client actions: %v", machineClientActions)
				}
			},
		},

		{
			name: "a machine with valid hooks",
			initialObjectsForMachineLister: func() []runtime.Object {
				m := machineFor("m-0", "10.0.139.78")
				m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})
				return []runtime.Object{m}
			}(),
		},

		{
			name: "multiple machines with valid hooks",
			initialObjectsForMachineLister: func() []runtime.Object {
				m0 := machineFor("m-0", "10.0.139.78")
				m0.Spec.LifecycleHooks.PreDrain = append(m0.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})

				m1 := machineFor("m-1", "10.0.139.79")
				m1.Spec.LifecycleHooks.PreDrain = append(m1.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})
				return []runtime.Object{m0, m1}
			}(),
		},

		{
			name: "multiple machines with missing hooks",
			initialObjectsForMachineLister: func() []runtime.Object {
				m0 := machineFor("m-0", "10.0.139.78")
				m1 := machineFor("m-1", "10.0.139.79")
				return []runtime.Object{m0, m1}
			}(),
			expectedActions: []string{"update:machines:", "update:machines:"},
			validateFunc: func(t *testing.T, machineClientActions []clientgotesting.Action) {
				wasMachineUpdated := false
				for _, action := range machineClientActions {
					if action.Matches("update", "machines") {
						updateAction := action.(clientgotesting.UpdateAction)
						updatedMachine := updateAction.GetObject().(*machinev1beta1.Machine)
						if !hasMachineDeletionHook(updatedMachine) {
							t.Fatalf("machine %v is missing required machine deletion hooks", updatedMachine)
						}
						wasMachineUpdated = true
					}
				}
				if !wasMachineUpdated {
					t.Errorf("expected to see an updated machine but didn't get any, fake machine client actions: %v", machineClientActions)
				}
			},
		},

		{
			name: "mixed machines",
			initialObjectsForMachineLister: func() []runtime.Object {
				m0 := machineFor("m-0", "10.0.139.78")
				m1 := machineFor("m-1", "10.0.139.79")
				m1.Spec.LifecycleHooks.PreDrain = append(m1.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})
				return []runtime.Object{m0, m1}
			}(),
			expectedActions: []string{"update:machines:"},
			validateFunc: func(t *testing.T, machineClientActions []clientgotesting.Action) {
				wasMachineUpdated := false
				for _, action := range machineClientActions {
					if action.Matches("update", "machines") {
						updateAction := action.(clientgotesting.UpdateAction)
						updatedMachine := updateAction.GetObject().(*machinev1beta1.Machine)
						if !hasMachineDeletionHook(updatedMachine) {
							t.Fatalf("machine %v is missing required machine deletion hooks", updatedMachine)
						}
						wasMachineUpdated = true
					}
				}
				if !wasMachineUpdated {
					t.Errorf("expected to see an updated machine but didn't get any, fake machine client actions: %v", machineClientActions)
				}
			},
		},

		{
			name: "a machine with deletions ts set",
			initialObjectsForMachineLister: func() []runtime.Object {
				m := machineFor("m-0", "10.0.139.78")
				ds := metav1.NewTime(time.Now())
				m.DeletionTimestamp = &ds
				return []runtime.Object{m}
			}(),
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}
			fakeMachineClient := machinev1beta1fakeclient.NewSimpleClientset(scenario.initialObjectsForMachineLister...)

			// act
			target := machineDeletionHooksController{
				machineClient:         fakeMachineClient.MachineV1beta1().Machines(""),
				masterMachineSelector: machineSelector,
				masterMachineLister:   machineLister,
			}

			err = target.addDeletionHookToMasterMachines(context.TODO())
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from sync() method")
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatal(err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned: %v, expected: %v", err, scenario.expectedError)
			}
			if err := validateActionsVerbs(fakeMachineClient.Actions(), scenario.expectedActions); err != nil {
				t.Fatalf("incorrect actions from the fake machine client: %v", err)
			}
			if scenario.validateFunc != nil {
				scenario.validateFunc(t, fakeMachineClient.Actions())
			}
		})
	}
}

func TestAttemptToDeleteMachineDeletionHook(t *testing.T) {
	scenarios := []struct {
		name                           string
		initialObjectsForMachineLister []runtime.Object
		initialEtcdMemberList          []*etcdserverpb.Member
		expectedActions                []string
		validateFunc                   func(ts *testing.T, machineClientActions []clientgotesting.Action)
	}{
		{
			name: "no-op",
			initialObjectsForMachineLister: func() []runtime.Object {
				return wellKnownMasterMachines()
			}(),
		},

		{
			name: "no-op: one excessive machine",
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				machines = append(machines, machineWithHooksFor("m-4", "10.0.139.81"))
				return machines
			}(),
		},

		{
			name: "one excessive machine pending deletion without a member",
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialEtcdMemberList: wellKnownEtcdMemberList(),
			expectedActions:       []string{"update:machines:"},
			validateFunc: func(t *testing.T, machineClientActions []clientgotesting.Action) {
				wasMachineUpdated := false
				for _, action := range machineClientActions {
					if action.Matches("update", "machines") {
						updateAction := action.(clientgotesting.UpdateAction)
						updatedMachine := updateAction.GetObject().(*machinev1beta1.Machine)
						if hasMachineDeletionHook(updatedMachine) {
							t.Fatalf("machine %v has unexpected machine deletion hooks", updatedMachine)
						}
						wasMachineUpdated = true
					}
				}
				if !wasMachineUpdated {
					t.Errorf("expected to see an updated machine but didn't get any, fake machine client actions: %v", machineClientActions)
				}
			},
		},

		{
			name: "two excessive, the first one with pending deletion and without the hooks, the second pending deletion without a member",
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				m4 := machineFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				m5 := machineWithHooksFor("m-5", "10.0.139.82")
				m5.DeletionTimestamp = &metav1.Time{}
				machines = append(machines, m4, m5)
				return machines
			}(),
			initialEtcdMemberList: wellKnownEtcdMemberList(),
			expectedActions:       []string{"update:machines:"},
			validateFunc: func(t *testing.T, machineClientActions []clientgotesting.Action) {
				wasMachineUpdated := false
				for _, action := range machineClientActions {
					if action.Matches("update", "machines") {
						updateAction := action.(clientgotesting.UpdateAction)
						updatedMachine := updateAction.GetObject().(*machinev1beta1.Machine)
						if hasMachineDeletionHook(updatedMachine) {
							t.Fatalf("machine %v has unexpected machine deletion hooks", updatedMachine)
						}
						if updatedMachine.Name != "m-5" {
							t.Fatalf("unexpected machine updated %v, expected m-5 to be updated", updatedMachine.Name)
						}
						wasMachineUpdated = true
					}
				}
				if !wasMachineUpdated {
					t.Errorf("expected to see an updated machine but didn't get any, fake machine client actions: %v", machineClientActions)
				}
			},
		},
	}

	for _, scenario := range scenarios {
		// test data
		eventRecorder := events.NewRecorder(k8sfakeclient.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-machine-deletion-hooks-controller", &corev1.ObjectReference{})
		machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, initialObj := range scenario.initialObjectsForMachineLister {
			machineIndexer.Add(initialObj)
		}
		machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
		machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
		if err != nil {
			t.Fatal(err)
		}
		fakeMachineClient := machinev1beta1fakeclient.NewSimpleClientset(scenario.initialObjectsForMachineLister...)

		fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
		if err != nil {
			t.Fatal(err)
		}

		// act
		target := machineDeletionHooksController{
			masterMachineSelector: machineSelector,
			masterMachineLister:   machineLister,
			machineClient:         fakeMachineClient.MachineV1beta1().Machines(""),
			etcdClient:            fakeEtcdClient,
		}

		err = target.attemptToDeleteMachineDeletionHook(context.TODO(), eventRecorder)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateActionsVerbs(fakeMachineClient.Actions(), scenario.expectedActions); err != nil {
			t.Fatalf("incorrect actions from the fake machine client: %v", err)
		}
		if scenario.validateFunc != nil {
			scenario.validateFunc(t, fakeMachineClient.Actions())
		}
	}
}

func machineFor(name, internalIP string) *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status: machinev1beta1.MachineStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: internalIP,
			},
		}},
	}
}

func machineWithHooksFor(name, internalIP string) *machinev1beta1.Machine {
	m := machineFor(name, internalIP)
	m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})
	return m
}

func wellKnownMasterMachines() []runtime.Object {
	return []runtime.Object{
		machineWithHooksFor("m-1", "10.0.139.78"),
		machineWithHooksFor("m-2", "10.0.139.79"),
		machineWithHooksFor("m-3", "10.0.139.80"),
	}
}

func wellKnownEtcdMemberList() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{
			Name:     "m-1",
			ID:       1,
			PeerURLs: []string{"https://10.0.139.78:1234"},
		},
		{
			Name:     "m-2",
			ID:       2,
			PeerURLs: []string{"https://10.0.139.79:1234"},
		},
		{
			Name:     "m-3",
			ID:       3,
			PeerURLs: []string{"https://10.0.139.80:1234"},
		},
	}
}

func validateActionsVerbs(actualActions []clientgotesting.Action, expectedActions []string) error {
	if len(actualActions) != len(expectedActions) {
		return fmt.Errorf("expected to get %d actions but got %d\nexpected=%v \n got=%v", len(expectedActions), len(actualActions), expectedActions, actionStrings(actualActions))
	}
	for i, a := range actualActions {
		if got, expected := actionString(a), expectedActions[i]; got != expected {
			return fmt.Errorf("at %d got %s, expected %s", i, got, expected)
		}
	}
	return nil
}

func actionString(a clientgotesting.Action) string {
	return a.GetVerb() + ":" + a.GetResource().Resource + ":" + a.GetNamespace()
}

func actionStrings(actions []clientgotesting.Action) []string {
	res := make([]string, 0, len(actions))
	for _, a := range actions {
		res = append(res, actionString(a))
	}
	return res
}

func hasMachineDeletionHook(machine *machinev1beta1.Machine) bool {
	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == "EtcdQuorumOperator" && hook.Owner == "clusteroperator/etcd" {
			return true
		}
	}
	return false
}

type fakeMachineAPI struct {
	isMachineAPIFunctional func() (bool, error)
}

func (dm *fakeMachineAPI) IsFunctional() (bool, error) {
	return dm.isMachineAPIFunctional()
}
