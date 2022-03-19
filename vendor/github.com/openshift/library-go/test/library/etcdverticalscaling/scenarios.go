package etcdverticalscaling

import (
	"context"

	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestScalingUpAndDownSingleNode tests basic vertical scaling scenario.
// This scenario starts by adding a new master machine to the cluster
// next it validates the size of etcd cluster and makes sure the new member is healthy.
// The test ends by removing the newly added machine and validating the size of the cluster
// and asserting the member was removed from the etcd cluster by contacting MemberList API.
func TestScalingUpAndDownSingleNode(t TestingT) {
	// set up
	ctx := context.TODO()
	clientSet := getClients(t)

	// assert the cluster state before we run the test
	ensureInitialClusterState(ctx, t, clientSet)

	// step 1: add a new master node and wait until it is in Running state
	machineName := createNewMasterMachine(ctx, t, clientSet)
	ensureMasterMachineRunning(ctx, t, machineName, clientSet)

	// step 2: wait until a new member shows up and check if it is healthy
	ensureMembersCount(t, clientSet, 4)
	memberName := machineNameToEtcdMemberName(ctx, t, clientSet, machineName)
	ensureHealthyMember(t, clientSet, memberName)

	// step 3: clean-up: delete the machine and wait until etcd member is removed from the etcd cluster
	err := clientSet.Machine.Delete(ctx, machineName, metav1.DeleteOptions{})
	require.NoError(t, err)
	t.Logf("successfully deleted the machine %q from the API", machineName)
	ensureMembersCount(t, clientSet, 3)
	ensureMemberRemoved(t, clientSet, memberName)
	ensureRunningMachinesAndCount(ctx, t, clientSet)
}
