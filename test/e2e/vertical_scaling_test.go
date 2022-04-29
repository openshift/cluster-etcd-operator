package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/utils/pointer"

	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	machinev1beta1client "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	testlibraryapi "github.com/openshift/library-go/test/library/apiserver"
)

const masterMachineLabelSelector = "machine.openshift.io/cluster-api-machine-role" + "=" + "master"
const machineDeletionHookName = "EtcdQuorumOperator"
const machineDeletionHookOwner = "clusteroperator/etcd"

// TestScalingUpAndDownSingleNode tests basic vertical scaling scenario.
// This scenario starts by adding a new master machine to the cluster
// next it validates the size of etcd cluster and makes sure the new member is healthy.
// The test ends by removing the newly added machine and validating the size of the cluster
// and asserting the member was removed from the etcd cluster by contacting MemberList API.
func TestScalingUpAndDownSingleNode(t *testing.T) {
	// set up
	ctx := context.TODO()
	kubeConfig, err := framework.NewClientConfigForTest("")
	require.NoError(t, err)
	kubeConfig.Timeout = 60 * time.Second
	require.NoError(t, err)
	machineClientSet, err := machineclient.NewForConfig(kubeConfig)
	require.NoError(t, err)
	machineClient := machineClientSet.MachineV1beta1().Machines("openshift-machine-api")
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	etcdClientFactory := newEtcdClientFactory(kubeClient)

	// assert the cluster state before we run the test
	ensureInitialClusterState(ctx, t, etcdClientFactory, machineClient)

	// step 1: add a new master node and wait until it is in Running state
	machineName := createNewMasterMachine(ctx, t, machineClient)
	ensureMasterMachine(ctx, t, machineName, machineClient)

	// step 2: wait until a new member shows up and check if it is healthy
	//         and until all kube-api servers have reached the same revision
	//         this additional step is the best-effort of ensuring they
	//         have observed the new member before disruption
	ensureVotingMembersCount(t, etcdClientFactory, 4)
	memberName := machineNameToEtcdMemberName(ctx, t, kubeClient, machineClient, machineName)
	ensureHealthyMember(t, etcdClientFactory, memberName)
	t.Log("waiting for kube api servers to stabilize on the same revision")
	testlibraryapi.WaitForAPIServerToStabilizeOnTheSameRevision(t, kubeClient.CoreV1().Pods("openshift-kube-apiserver"))

	// step 3: clean-up: delete the machine and wait until etcd member is removed from the etcd cluster
	err = machineClient.Delete(ctx, machineName, metav1.DeleteOptions{})
	require.NoError(t, err)
	t.Logf("successfully deleted the machine %q from the API", machineName)
	ensureVotingMembersCount(t, etcdClientFactory, 3)
	ensureMemberRemoved(t, etcdClientFactory, memberName)
	ensureRunningMachinesAndCount(ctx, t, machineClient)
}

// createNewMasterMachine creates a new master node by cloning an existing Machine resource
func createNewMasterMachine(ctx context.Context, t testing.TB, machineClient machinev1beta1client.MachineInterface) string {
	machineList, err := machineClient.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
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

	clonedMachine, err := machineClient.Create(context.TODO(), machineToClone, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Logf("Created a new master machine/node %q", clonedMachine.Name)
	return clonedMachine.Name
}

func ensureMasterMachine(ctx context.Context, t testing.TB, machineName string, machineClient machinev1beta1client.MachineInterface) {
	waitPollInterval := 15 * time.Second
	waitPollTimeout := 5 * time.Minute
	t.Logf("Waiting up to %s for %q machine to be in the Running state", waitPollTimeout.String(), machineName)

	if err := wait.Poll(waitPollInterval, waitPollTimeout, func() (bool, error) {
		machine, err := machineClient.Get(ctx, machineName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		machinePhase := pointer.StringDeref(machine.Status.Phase, "Unknown")
		t.Logf("%q machine is in %q state", machineName, machinePhase)
		if machinePhase != "Running" {
			return false, nil
		}
		if !hasMachineDeletionHook(machine) {
			// it takes some time to add the hook
			t.Logf("%q machine doesn't have required deletion hooks", machine.Name)
			return false, nil
		}
		return true, nil
	}); err != nil {
		newErr := fmt.Errorf("failed to check if %q is Running state, err: %v", machineName, err)
		require.NoError(t, newErr)
	}
}

// ensureInitialClusterState makes sure the cluster state is expected, that is, has only 3 running machines and exactly 3 voting members
// otherwise it attempts to recover the cluster by removing any excessive machines
func ensureInitialClusterState(ctx context.Context, t testing.TB, etcdClientFactory etcdClientCreator, machineClient machinev1beta1client.MachineInterface) {
	require.NoError(t, recoverClusterToInitialStateIfNeeded(ctx, t, machineClient))
	require.NoError(t, checkVotingMembersCount(t, etcdClientFactory, 3))
	require.NoError(t, checkMasterMachinesAndCount(ctx, t, machineClient))
}

// ensureRunningMachinesAndCount asserts there are only 3 running master machines
func ensureRunningMachinesAndCount(ctx context.Context, t testing.TB, machineClient machinev1beta1client.MachineInterface) {
	err := checkMasterMachinesAndCount(ctx, t, machineClient)
	require.NoError(t, err)
}

// checkMasterMachinesAndCount checks if there are only 3 running master machines otherwise it returns an error
func checkMasterMachinesAndCount(ctx context.Context, t testing.TB, machineClient machinev1beta1client.MachineInterface) error {
	waitPollInterval := 15 * time.Second
	waitPollTimeout := 10 * time.Minute
	t.Logf("Waiting up to %s for the cluster to reach the expected machines count of 3", waitPollTimeout.String())

	return wait.Poll(waitPollInterval, waitPollTimeout, func() (bool, error) {
		machineList, err := machineClient.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
		if err != nil {
			return false, err
		}

		if len(machineList.Items) != 3 {
			var machineNames []string
			for _, machine := range machineList.Items {
				machineNames = append(machineNames, machine.Name)
			}
			t.Logf("expected exactly 3 master machines, got %d, machines are: %v", len(machineList.Items), machineNames)
			return false, nil
		}

		for _, machine := range machineList.Items {
			machinePhase := pointer.StringDeref(machine.Status.Phase, "")
			if machinePhase != "Running" {
				return false, fmt.Errorf("%q machine is in unexpected %q state, expected Running", machine.Name, machinePhase)
			}
			if !hasMachineDeletionHook(&machine) {
				return false, fmt.Errorf("%q machine doesn't have required deletion hooks", machine.Name)
			}
		}
		return true, nil
	})
}

func recoverClusterToInitialStateIfNeeded(ctx context.Context, t testing.TB, machineClient machinev1beta1client.MachineInterface) error {
	machineList, err := machineClient.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
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
			err := machineClient.Delete(ctx, machine.Name, metav1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("failed removing the machine: %q, err: %v", machine.Name, err)
			}
			t.Logf("successfully deleted an excessive machine %q from the API (perhaps, created by a previous test)", machine.Name)
		}
	}

	return nil
}

// ensureVotingMembersCount same as checkVotingMembersCount but will fail on error
func ensureVotingMembersCount(t testing.TB, etcdClientFactory etcdClientCreator, expectedMembersCount int) {
	require.NoError(t, checkVotingMembersCount(t, etcdClientFactory, expectedMembersCount))
}

// checkVotingMembersCount counts the number of voting etcd members, it doesn't evaluate health conditions or any other attributes (i.e. name) of individual members
// this method won't fail immediately on errors, this is useful during scaling down operation until the feature can ensure this operation to be graceful
func checkVotingMembersCount(t testing.TB, etcdClientFactory etcdClientCreator, expectedMembersCount int) error {
	waitPollInterval := 15 * time.Second
	waitPollTimeout := 10 * time.Minute
	t.Logf("Waiting up to %s for the cluster to reach the expected member count of %v", waitPollTimeout.String(), expectedMembersCount)

	if err := wait.Poll(waitPollInterval, waitPollTimeout, func() (bool, error) {
		etcdClient, closeFn, err := etcdClientFactory.newEtcdClient()
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

		var votingMemberNames []string
		for _, member := range memberList.Members {
			if !member.IsLearner {
				votingMemberNames = append(votingMemberNames, member.Name)
			}
		}
		if len(votingMemberNames) != expectedMembersCount {
			t.Logf("unexpected number of voting etcd members, expected exactly %d, got: %v, current members are: %v", expectedMembersCount, len(votingMemberNames), votingMemberNames)
			return false, nil
		}

		t.Logf("cluster has reached the expected number of %v voting members, the members are: %v", expectedMembersCount, votingMemberNames)
		return true, nil
	}); err != nil {
		newErr := fmt.Errorf("failed on waiting for the cluster to reach the expected member count of %v, err %v", expectedMembersCount, err)
		return newErr
	}
	return nil
}

func ensureMemberRemoved(t testing.TB, etcdClientFactory etcdClientCreator, memberName string) {
	etcdClient, closeFn, err := etcdClientFactory.newEtcdClient()
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

func ensureHealthyMember(t testing.TB, etcdClientFactory etcdClientCreator, memberName string) {
	etcdClient, closeFn, err := etcdClientFactory.newEtcdClientForMember(memberName)
	require.NoError(t, err)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.TODO(), 15*time.Second)
	defer cancel()

	// We know it's a voting member so lineared read is fine
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
func machineNameToEtcdMemberName(ctx context.Context, t testing.TB, kubeClient kubernetes.Interface, machineClient machinev1beta1client.MachineInterface, machineName string) string {
	machine, err := machineClient.Get(ctx, machineName, metav1.GetOptions{})
	require.NoError(t, err)
	machineProviderID := pointer.StringDeref(machine.Spec.ProviderID, "")
	if len(machineProviderID) == 0 {
		t.Fatalf("failed to get the providerID for %q machine", machineName)
	}

	// find corresponding node, match on providerID
	masterNodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/master"})
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

func hasMachineDeletionHook(machine *machinev1beta1.Machine) bool {
	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == machineDeletionHookName && hook.Owner == machineDeletionHookOwner {
			return true
		}
	}
	return false
}

type etcdClientCreator interface {
	newEtcdClient() (*clientv3.Client, func(), error)
	newEtcdClientForMember(memberName string) (*clientv3.Client, func(), error)
}

type etcdClientFactoryImpl struct {
	kubeClient kubernetes.Interface
}

func newEtcdClientFactory(kubeClient kubernetes.Interface) *etcdClientFactoryImpl {
	return &etcdClientFactoryImpl{kubeClient: kubeClient}
}

func (e *etcdClientFactoryImpl) newEtcdClient() (*clientv3.Client, func(), error) {
	return e.newEtcdClientForTarget("service/etcd")
}

func (e *etcdClientFactoryImpl) newEtcdClientForMember(memberName string) (*clientv3.Client, func(), error) {
	return e.newEtcdClientForTarget(fmt.Sprintf("pod/etcd-%v", memberName))
}

func (e *etcdClientFactoryImpl) newEtcdClientForTarget(target string) (*clientv3.Client, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "oc", "port-forward", target, ":2379", "-n", "openshift-etcd")

	done := func() {
		cancel()
		_ = cmd.Wait() // wait to clean up resources but ignore returned error since cancel kills the process
	}

	var err error // so we can clean up on error
	defer func() {
		if err != nil {
			done()
		}
	}()

	stdOut, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}

	scanner := bufio.NewScanner(stdOut)
	if !scanner.Scan() {
		return nil, nil, fmt.Errorf("failed to scan port forward std out")
	}
	if err = scanner.Err(); err != nil {
		return nil, nil, err
	}
	output := scanner.Text()

	port := strings.TrimSuffix(strings.TrimPrefix(output, "Forwarding from 127.0.0.1:"), " -> 2379")
	_, err = strconv.Atoi(port)
	if err != nil {
		return nil, nil, fmt.Errorf("port forward output not in expected format: %s", output)
	}

	coreV1 := e.kubeClient.CoreV1()
	etcdConfigMap, err := coreV1.ConfigMaps("openshift-config").Get(ctx, "etcd-ca-bundle", metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	etcdSecret, err := coreV1.Secrets("openshift-config").Get(ctx, "etcd-client", metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	tlsConfig, err := restclient.TLSConfigFor(&restclient.Config{
		TLSClientConfig: restclient.TLSClientConfig{
			CertData: etcdSecret.Data[corev1.TLSCertKey],
			KeyData:  etcdSecret.Data[corev1.TLSPrivateKeyKey],
			CAData:   []byte(etcdConfigMap.Data["ca-bundle.crt"]),
		},
	})
	if err != nil {
		return nil, nil, err
	}

	etcdClient3, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"https://127.0.0.1:" + port},
		DialTimeout: 30 * time.Second,
		TLS:         tlsConfig,
	})
	if err != nil {
		return nil, nil, err
	}

	return etcdClient3, done, nil
}
