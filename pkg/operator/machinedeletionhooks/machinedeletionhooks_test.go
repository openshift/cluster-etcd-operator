package machinedeletionhooks

import (
	"context"
	"fmt"
	"reflect"
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

	kubefakeclient "k8s.io/client-go/kubernetes/fake"
	corelistersv1 "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
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
			name: "one excessive machine pending deletion without a member ipv6",
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "fe80::901a:317c:fbc5:fba9")
				m4.DeletionTimestamp = &metav1.Time{}
				return []runtime.Object{
					machineWithHooksFor("m-1", "fe80::901a:317c:fbc5:fba6"),
					machineWithHooksFor("m-2", "fe80::901a:317c:fbc5:fba7"),
					machineWithHooksFor("m-3", "fe80::901a:317c:fbc5:fba8"),
					m4,
				}
			}(),
			initialEtcdMemberList: []*etcdserverpb.Member{
				{
					Name:     "m-1",
					ID:       1,
					PeerURLs: []string{"https://[fe80::901a:317c:fbc5:fba6]:1234"},
				},
				{
					Name:     "m-2",
					ID:       2,
					PeerURLs: []string{"https://[fe80::901a:317c:fbc5:fba7]:1234"},
				},
				{
					Name:     "m-3",
					ID:       3,
					PeerURLs: []string{"https://[fe80::901a:317c:fbc5:fba8]:1234"},
				},
			},
			expectedActions: []string{"update:machines:"},
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
			name: "machine with dualstack network pending deletion with an excessive member",
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := addHook(dualStackMachineFor("m-4", "10.0.139.81", "fe80::901a:317c:fbc5:fba9"))
				m4.DeletionTimestamp = &metav1.Time{}

				return []runtime.Object{
					addHook(dualStackMachineFor("m-1", "10.0.139.78", "fe80::901a:317c:fbc5:fba6")),
					addHook(dualStackMachineFor("m-2", "10.0.139.79", "fe80::901a:317c:fbc5:fba7")),
					addHook(dualStackMachineFor("m-3", "10.0.139.80", "fe80::901a:317c:fbc5:fba8")),
					m4,
				}
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
			name: "machine with dualstack network pending deletion with no excessive member",
			initialObjectsForMachineLister: func() []runtime.Object {
				m1 := addHook(dualStackMachineFor("m-1", "10.0.139.78", "fe80::901a:317c:fbc5:fba6"))
				m1.DeletionTimestamp = &metav1.Time{}

				return []runtime.Object{
					m1,
					addHook(dualStackMachineFor("m-2", "10.0.139.79", "fe80::901a:317c:fbc5:fba7")),
					addHook(dualStackMachineFor("m-3", "10.0.139.80", "fe80::901a:317c:fbc5:fba8")),
				}
			}(),
			initialEtcdMemberList: wellKnownEtcdMemberList(),
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
			t.Fatalf("[%s] incorrect actions from the fake machine client: %v", scenario.name, err)
		}
		if scenario.validateFunc != nil {
			scenario.validateFunc(t, fakeMachineClient.Actions())
		}
	}
}

func TestAttemptToDeleteQuorumGuard(t *testing.T) {
	scenarios := []struct {
		name                           string
		initialObjectsForKubeClient    []runtime.Object
		initialObjectsForMachineLister []runtime.Object
		initialObjectsForPodLister     []runtime.Object
		expectedKubeClientActions      []string
		expectedEvents                 []string
		validateFunc                   func(ts *testing.T, kubeClientActions []clientgotesting.Action)
	}{
		{
			name: "no machines",
		},
		{
			name: "no machines pending deletion",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				addNode(machineWithHooksFor("m2", "10.0.139.80"), "n2"),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
				guardPodForNode("n2"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
				nodeFor("n2"),
			},
		},
		{
			name: "a machine pending deletion with a hook",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				setDeletionTimestamp(addNode(machineWithHooksFor("m2", "10.0.139.80"), "n2")),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
				guardPodForNode("n2"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
				nodeFor("n2"),
			},
		},
		{
			name: "a machine pending deletion without a hook, without a node",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				setDeletionTimestamp(machineFor("m2", "10.0.139.80")),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
				guardPodForNode("n2"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
				nodeFor("n2"),
			},
		},
		{
			name: "a machine pending deletion without a hook, with a node and a guard pod",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				setDeletionTimestamp(addNode(machineFor("m2", "10.0.139.80"), "n2")),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
				guardPodForNode("n2"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
				nodeFor("n2"),
			},
			expectedKubeClientActions: []string{
				"get:nodes:",
			},
			validateFunc: func(ts *testing.T, kubeClientActions []clientgotesting.Action) {
				if len(kubeClientActions) != 1 {
					t.Fatalf("Expected 1 action, got %d actions", len(kubeClientActions))
				}

				action := kubeClientActions[0]
				validateGetNodeAction(t, action, "n2")
			},
		},
		{
			name: "a machine pending deletion without a hook, with a missing node",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				setDeletionTimestamp(addNode(machineFor("m2", "10.0.139.80"), "n2")),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
				guardPodForNode("n2"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
			},
			expectedKubeClientActions: []string{
				"get:nodes:",
			},
			validateFunc: func(ts *testing.T, kubeClientActions []clientgotesting.Action) {
				if len(kubeClientActions) != 1 {
					t.Fatalf("Expected 1 action, got %d actions", len(kubeClientActions))
				}

				action := kubeClientActions[0]
				validateGetNodeAction(t, action, "n2")
			},
		},
		{
			name: "a machine pending deletion without a hook, with a cordoned node and a guard pod",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				setDeletionTimestamp(addNode(machineFor("m2", "10.0.139.80"), "n2")),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
				guardPodForNode("n2"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
				cordonNode(nodeFor("n2")),
			},
			expectedKubeClientActions: []string{
				"get:nodes:",
				"delete:pods:test",
			},
			validateFunc: func(ts *testing.T, kubeClientActions []clientgotesting.Action) {
				if len(kubeClientActions) != 2 {
					t.Fatalf("Expected 2 action, got %d actions", len(kubeClientActions))
				}

				getNodeAction := kubeClientActions[0]
				validateGetNodeAction(t, getNodeAction, "n2")

				deletePodAction := kubeClientActions[1]
				validateDeletePodAction(t, deletePodAction, "guard-n2")
			},
			expectedEvents: []string{"QuorumGuardRemoved"},
		},
		{
			name: "a machine pending deletion without a hook, with a cordoned node without a guard pod",
			initialObjectsForMachineLister: []runtime.Object{
				addNode(machineWithHooksFor("m0", "10.0.139.78"), "n0"),
				addNode(machineWithHooksFor("m1", "10.0.139.79"), "n1"),
				setDeletionTimestamp(addNode(machineFor("m2", "10.0.139.80"), "n2")),
			},
			initialObjectsForPodLister: []runtime.Object{
				guardPodForNode("n0"),
				guardPodForNode("n1"),
			},
			initialObjectsForKubeClient: []runtime.Object{
				nodeFor("n0"),
				nodeFor("n1"),
				cordonNode(nodeFor("n2")),
			},
			expectedKubeClientActions: []string{
				"get:nodes:",
			},
			validateFunc: func(ts *testing.T, kubeClientActions []clientgotesting.Action) {
				if len(kubeClientActions) != 1 {
					t.Fatalf("Expected 1 action, got %d actions", len(kubeClientActions))
				}

				action := kubeClientActions[0]
				validateGetNodeAction(t, action, "n2")
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// We only expect a single event.
			eventRecorder := events.NewInMemoryRecorder("test-cluster-machine-deletion-hooks-controller")

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)

			podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForPodLister {
				podIndexer.Add(initialObj)
			}
			podLister := corelistersv1.NewPodLister(podIndexer)

			fakeKubeClient := kubefakeclient.NewSimpleClientset(append(scenario.initialObjectsForPodLister, scenario.initialObjectsForKubeClient...)...)

			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}

			// act
			target := machineDeletionHooksController{
				masterMachineSelector:              machineSelector,
				masterMachineLister:                machineLister,
				kubeClient:                         fakeKubeClient,
				podListerForOpenShiftEtcdNamespace: podLister.Pods(""),
			}

			err = target.attemptToDeleteQuorumGuard(context.TODO(), eventRecorder)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateActionsVerbs(fakeKubeClient.Actions(), scenario.expectedKubeClientActions); err != nil {
				t.Fatalf("[%s] incorrect actions from the fake kube client: %v", scenario.name, err)
			}
			if scenario.validateFunc != nil {
				scenario.validateFunc(t, fakeKubeClient.Actions())
			}

			receivedEvents := []string{}
			for _, ev := range eventRecorder.Events() {
				receivedEvents = append(receivedEvents, ev.Reason)
			}

			if len(scenario.expectedEvents) > 0 && !reflect.DeepEqual(receivedEvents, scenario.expectedEvents) {
				t.Fatalf("expected events %v, got events %v", scenario.expectedEvents, receivedEvents)
			}
			if len(scenario.expectedEvents) == 0 && len(receivedEvents) > 0 {
				t.Fatalf("unexpected events: %v", receivedEvents)
			}
		})
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

func dualStackMachineFor(name, internalIPv4, internalIPv6 string) *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status: machinev1beta1.MachineStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: internalIPv4,
			},
			{
				Type:    corev1.NodeInternalIP,
				Address: internalIPv6,
			},
		}},
	}
}

func machineWithHooksFor(name, internalIP string) *machinev1beta1.Machine {
	m := machineFor(name, internalIP)
	return addHook(m)
}

func addHook(m *machinev1beta1.Machine) *machinev1beta1.Machine {
	m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})
	return m
}

func setDeletionTimestamp(m *machinev1beta1.Machine) *machinev1beta1.Machine {
	now := metav1.Now()
	m.DeletionTimestamp = &now

	return m
}

func addNode(m *machinev1beta1.Machine, nodeName string) *machinev1beta1.Machine {
	m.Status.NodeRef = &corev1.ObjectReference{
		Name: nodeName,
	}

	return m
}

func guardPodForNode(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("guard-%s", name), Namespace: "test", Labels: map[string]string{"app": "guard"}},
		Spec: corev1.PodSpec{
			NodeName: name,
		},
	}
}

func nodeFor(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func cordonNode(n *corev1.Node) *corev1.Node {
	n.Spec.Unschedulable = true

	return n
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

func validateGetNodeAction(t *testing.T, action clientgotesting.Action, nodeName string) {
	if !action.Matches("get", "nodes") {
		t.Fatalf("expected action to match get:nodes:, got %s", actionString(action))
	}

	getAction := action.(clientgotesting.GetAction)
	if getAction.GetName() != nodeName {
		t.Fatalf("expected get action to get %q, get action got %q", nodeName, getAction.GetName())
	}
}

func validateDeletePodAction(t *testing.T, action clientgotesting.Action, podName string) {
	if !action.Matches("delete", "pods") {
		t.Fatalf("expected action to match delete:pods:test, got: %s", actionString(action))
	}

	deleteAction := action.(clientgotesting.DeleteAction)
	if deleteAction.GetName() != podName {
		t.Fatalf("expected delete action to delete %q, delete action deleted %q", podName, deleteAction.GetName())
	}

	if deleteAction.GetNamespace() != "test" {
		t.Fatalf("expected delete action to delete pod in namespace \"test\", delete action deleted pod in namespace %q", deleteAction.GetNamespace())
	}
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

func (dm *fakeMachineAPI) IsEnabled() (bool, error) {
	return true, nil
}
