package etcdverticalscaling

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
)

const masterMachineLabelSelector = "machine.openshift.io/cluster-api-machine-role" + "=" + "master"

// createNewMasterMachine creates a new master node by cloning an existing Machine resource
func createNewMasterMachine(ctx context.Context, t TestingT, clientSet *ClientSet) string {
	machineList, err := clientSet.Machine.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
	require.NoError(t, err)
	var machineToClone *machinev1beta1.Machine
	for _, machine := range machineList.Items {
		machinePhase := pointer.StringDeref(machine.Status.Phase, "Unknown")
		if machinePhase == "Running" {
			machineToClone = &machine
			break
		}
		t.Logf("%q machine is in unexpected %q state", machine.Name, machinePhase)
	}

	if machineToClone == nil {
		t.Fatal("unable to find a running master machine to clone")
	}
	// assigning a new Name and clearing ProviderID is enough
	// for MAO to pick it up and provision a new master machine/node
	machineToClone.Name = fmt.Sprintf("%s-clone", machineToClone.Name)
	machineToClone.Spec.ProviderID = nil
	machineToClone.ResourceVersion = ""

	clonedMachine, err := clientSet.Machine.Create(context.TODO(), machineToClone, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Logf("Created a new master machine/node %q", clonedMachine.Name)
	return clonedMachine.Name
}

func ensureMasterMachineRunning(ctx context.Context, t TestingT, machineName string, clientSet *ClientSet) {
	waitPollInterval := 15 * time.Second
	waitPollTimeout := 5 * time.Minute
	t.Logf("Waiting up to %s for %q machine to be in the Running state", waitPollTimeout.String(), machineName)

	if err := wait.Poll(waitPollInterval, waitPollTimeout, func() (bool, error) {
		machine, err := clientSet.Machine.Get(ctx, machineName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		machinePhase := pointer.StringDeref(machine.Status.Phase, "Unknown")
		t.Logf("%q machine is in %q state", machineName, machinePhase)
		if machinePhase == "Running" {
			return true, nil
		}
		return false, nil
	}); err != nil {
		newErr := fmt.Errorf("failed to check if %q is Running state, err: %v", machineName, err)
		require.NoError(t, newErr)
	}
}

// ensureInitialClusterState makes sure the cluster state is expected, that is, has only 3 running machines and exactly 3 voting members
// otherwise it attempts to recover the cluster by removing any excessive machines
func ensureInitialClusterState(ctx context.Context, t TestingT, clientSet *ClientSet) {
	require.NoError(t, recoverClusterToInitialStateIfNeeded(ctx, t, clientSet))
	require.NoError(t, checkMembersCount(t, clientSet, 3))
	require.NoError(t, checkRunningMachinesAndCount(ctx, clientSet))
}

// ensureRunningMachinesAndCount asserts there are only 3 running master machines
func ensureRunningMachinesAndCount(ctx context.Context, t TestingT, clientSet *ClientSet) {
	err := checkRunningMachinesAndCount(ctx, clientSet)
	require.NoError(t, err)
}

// checkRunningMachinesAndCount checks if there are only 3 running master machines otherwise it returns an error
func checkRunningMachinesAndCount(ctx context.Context, clientSet *ClientSet) error {
	machineList, err := clientSet.Machine.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
	if err != nil {
		return err
	}

	if len(machineList.Items) != 3 {
		var machineNames []string
		for _, machine := range machineList.Items {
			machineNames = append(machineNames, machine.Name)
		}
		return fmt.Errorf("expected exactly 3 master machines, got %d, machines are: %v", len(machineList.Items), machineNames)
	}

	for _, machine := range machineList.Items {
		machinePhase := pointer.StringDeref(machine.Status.Phase, "")
		if machinePhase != "Running" {
			return fmt.Errorf("%q machine is in unexpected %q state, expected Running", machine.Name, machinePhase)
		}
	}
	return nil
}

func recoverClusterToInitialStateIfNeeded(ctx context.Context, t TestingT, clientSet *ClientSet) error {
	machineList, err := clientSet.Machine.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
	if err != nil {
		return err
	}

	var machineNames []string
	for _, machine := range machineList.Items {
		machineNames = append(machineNames, machine.Name)
	}

	t.Logf("checking if there are any excessive machines in the cluster (created by a previous test), expected cluster size is 3, found %v machines: %v", len(machineList.Items), machineNames)
	for _, machine := range machineList.Items {
		if strings.HasSuffix(machine.Name, "-clone") {
			err := clientSet.Machine.Delete(ctx, machine.Name, metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("failed removing the machine: %q, err: %v", machine.Name, err)
			}
			t.Logf("successfully deleted an excessive machine %q from the API (perhaps, created by a previous test)", machine.Name)
		}
	}

	return nil
}

// ensureMembersCount same as checkMembersCount but will fail on error
func ensureMembersCount(t TestingT, clientSet *ClientSet, expectedMembersCount int) {
	require.NoError(t, checkMembersCount(t, clientSet, expectedMembersCount))
}

// checkMembersCount simply counts the current etcd members, it doesn't evaluate health conditions or any other attributes (i.e. name) of individual members
// this method won't fail immediately on errors, this is useful during scaling down operation until the feature can ensure this operation to be graceful
func checkMembersCount(t TestingT, clientSet *ClientSet, expectedMembersCount int) error {
	waitPollInterval := 15 * time.Second
	waitPollTimeout := 10 * time.Minute
	t.Logf("Waiting up to %s for the cluster to reach the expected member count of %v", waitPollTimeout.String(), expectedMembersCount)

	if err := wait.Poll(waitPollInterval, waitPollTimeout, func() (bool, error) {
		etcdClient, closeFn, err := clientSet.Etcd.NewEtcdClient()
		if err != nil {
			t.Logf("failed to get etcd client, will retry, err: %v", err)
			return false, nil
		}
		defer closeFn()

		ctx, cancel := context.WithTimeout(context.TODO(), 15*time.Second)
		defer cancel()
		memberList, err := etcdClient.MemberList(ctx)
		if err != nil {
			t.Logf("failed to get the member list, will retry, err: %v", err)
			return false, nil
		}

		var memberNames []string
		for _, member := range memberList.Members {
			memberNames = append(memberNames, member.Name)
		}
		if len(memberNames) != expectedMembersCount {
			t.Logf("unexpected number of etcd members, expected exactly %d, got: %v, current members are: %v", expectedMembersCount, len(memberNames), memberNames)
			return false, nil
		}

		t.Logf("cluster have reached the expected number of %v members, the members are: %v", expectedMembersCount, memberNames)
		return true, nil
	}); err != nil {
		newErr := fmt.Errorf("failed on waiting for the cluster to reach the expected member count of %v, err %v", expectedMembersCount, err)
		return newErr
	}
	return nil
}

func ensureMemberRemoved(t TestingT, clientSet *ClientSet, memberName string) {
	etcdClient, closeFn, err := clientSet.Etcd.NewEtcdClient()
	require.NoError(t, err)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.TODO(), 15*time.Second)
	defer cancel()
	rsp, err := etcdClient.MemberList(ctx)
	require.NoError(t, err)

	for _, member := range rsp.Members {
		if member.Name == memberName {
			t.Fatalf("member %v hasn't been removed", spew.Sdump(member))
			return // unreachable
		}
	}
}

func ensureHealthyMember(t TestingT, clientSet *ClientSet, memberName string) {
	etcdClient, closeFn, err := clientSet.Etcd.NewEtcdClientForMember(memberName)
	require.NoError(t, err)
	defer closeFn()

	// since we have a direct connection with the member
	// getting any response is a good sign of healthiness
	ctx, cancel := context.WithTimeout(context.TODO(), 15*time.Second)
	defer cancel()
	_, err = etcdClient.Get(ctx, "health")
	if err != nil {
		require.NoError(t, fmt.Errorf("failed to check healthiness condition of the %q member, err: %v", memberName, err))
	}
	t.Logf("successfully evaluated health condition of %q member", memberName)
}

// machineNameToEtcdMemberName finds an etcd member name that corresponds to the given machine name
// first it looks up a node that corresponds to the machine by comparing the ProviderID field
// next, it returns the node name as it is used to name an etcd member
//
// note:
// it will exit and report an error in case the node was not found
func machineNameToEtcdMemberName(ctx context.Context, t TestingT, clientSet *ClientSet, machineName string) string {
	machine, err := clientSet.Machine.Get(ctx, machineName, metav1.GetOptions{})
	require.NoError(t, err)
	machineProviderID := pointer.StringDeref(machine.Spec.ProviderID, "")
	if len(machineProviderID) == 0 {
		t.Fatalf("failed to get the providerID for %q machine", machineName)
	}

	// find corresponding node, match on providerID
	masterNodes, err := clientSet.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/master"})
	require.NoError(t, err)

	var nodeNames []string
	for _, masterNode := range masterNodes.Items {
		if masterNode.Spec.ProviderID == machineProviderID {
			return masterNode.Name
		}
		nodeNames = append(nodeNames, masterNode.Name)
	}

	t.Fatalf("unable to find a node for the corresponding %q machine on ProviderID: %v, checked: %v", machineName, machineProviderID, nodeNames)
	return "" // unreachable
}

type TestingT interface {
	Logf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
	Fatal(args ...interface{})
	Fatalf(format string, args ...interface{})
	FailNow()
}
