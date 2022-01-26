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
)

const masterMachineLabelSelector = "machine.openshift.io/cluster-api-machine-role" + "=" + "master"

// TestScalingUpSingleNode adds a new master node and checks if it will result in an increase in etcd cluster.
// The test also verifies if the newly added member is healthy.
func TestScalingUpSingleNode(t *testing.T) {
	// set up
	ctx := context.TODO()
	kubeConfig, err := framework.NewClientConfigForTest("")
	kubeConfig.Timeout = 60 * time.Second
	require.NoError(t, err)
	machineClientSet, err := machineclient.NewForConfig(kubeConfig)
	require.NoError(t, err)
	machineClient := machineClientSet.MachineV1beta1().Machines("openshift-machine-api")
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	etcdClientFactory := newEtcdClientFactory(kubeClient)

	// assert the cluster state before we run the test
	ensureRunningMachines(ctx, t, machineClient)
	ensureMembersCount(t, etcdClientFactory, 3)

	// step 1: add a new master node and wait until it is in Running state
	machineName := createNewMasterMachine(ctx, t, machineClient)
	ensureMasterMachineRunning(ctx, t, machineName, machineClient)

	// step 2: wait until a new member shows up and check if it is healthy
	ensureMembersCount(t, etcdClientFactory, 4)
	memberName := machineNameToEtcdMemberName(ctx, t, kubeClient, machineClient, machineName)
	ensureHealthyMember(t, etcdClientFactory, memberName)

	// TODO: step 3: clean-up: until we can scale down
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

func ensureMasterMachineRunning(ctx context.Context, t testing.TB, machineName string, machineClient machinev1beta1client.MachineInterface) {
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
		if machinePhase == "Running" {
			return true, nil
		}
		return false, nil
	}); err != nil {
		newErr := fmt.Errorf("failed to check if %q is Running state, err: %v", machineName, err)
		require.NoError(t, newErr)
	}
}

// asserts the cluster has only 3 master machines and all are in the Running state
func ensureRunningMachines(ctx context.Context, t testing.TB, machineClient machinev1beta1client.MachineInterface) {
	machineList, err := machineClient.List(ctx, metav1.ListOptions{LabelSelector: masterMachineLabelSelector})
	require.NoError(t, err)

	if len(machineList.Items) != 3 {
		machineNames := []string{}
		for _, machine := range machineList.Items {
			machineNames = append(machineNames, machine.Name)
		}
		t.Fatalf("incorrect cluster size, expected exactly 3 master machines, got: %v, found: %v", len(machineList.Items), machineNames)
	}

	for _, machine := range machineList.Items {
		machinePhase := pointer.StringDeref(machine.Status.Phase, "")
		if machinePhase != "Running" {
			t.Fatalf("%q machine is in unexpected %q state, expected Running", machine.Name, machinePhase)
		}
	}
}

// ensureMembersCount simply counts the current etcd members, it doesn't evaluate health conditions or any other attributes (i.e. name) of individual members
func ensureMembersCount(t testing.TB, etcdClientFactory etcdClientCreator, expectedMembersCount int) {
	waitPollInterval := 15 * time.Second
	waitPollTimeout := 5 * time.Minute
	t.Logf("Waiting up to %s for a new etcd member to join the cluster", waitPollTimeout.String())

	if err := wait.Poll(waitPollInterval, waitPollTimeout, func() (bool, error) {
		etcdClient, close, err := etcdClientFactory.newEtcdClient()
		require.NoError(t, err)
		defer close()

		ctx, cancel := context.WithTimeout(context.TODO(), 15*time.Second)
		defer cancel()
		memberList, err := etcdClient.MemberList(ctx)
		require.NoError(t, err)

		memberNames := []string{}
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
		newErr := fmt.Errorf("failed on waiting for a new etcd member to join the cluster, err %v", err)
		require.NoError(t, newErr)
	}
}

func ensureHealthyMember(t testing.TB, etcdClientFactory etcdClientCreator, memberName string) {
	etcdClient, close, err := etcdClientFactory.newEtcdClientForMember(memberName)
	require.NoError(t, err)
	defer close()

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
