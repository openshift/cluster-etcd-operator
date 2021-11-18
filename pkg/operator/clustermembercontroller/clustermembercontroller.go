package clustermembercontroller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	"github.com/davecgh/go-spew/spew"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	mapiclientset "github.com/openshift/client-go/machine/clientset/versioned"
	machineinformersv1beta1 "github.com/openshift/client-go/machine/informers/externalversions/machine/v1beta1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/etcdserver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

const (
	masterLabel         = "node-role.kubernetes.io/master"
	machineAPINamespace = "openshift-machine-api"
)

type ClusterMemberController struct {
	mapiClient              mapiclientset.Interface
	operatorClient          v1helpers.OperatorClient
	kubeClient              kubernetes.Interface
	etcdClient              etcdcli.EtcdClient
	nodeLister              corev1listers.NodeLister
	networkLister           configv1listers.NetworkLister
	machineNamespacedLister machinelistersv1beta1.MachineNamespaceLister
	desiredControlPlaneSize int
}

func NewClusterMemberController(
	mapiClient mapiclientset.Interface,
	machineInformer machineinformersv1beta1.MachineInformer,
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &ClusterMemberController{
		mapiClient:              mapiClient,
		operatorClient:          operatorClient,
		kubeClient:              kubeClient,
		etcdClient:              etcdClient,
		nodeLister:              kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		networkLister:           networkInformer.Lister(),
		machineNamespacedLister: machineInformer.Lister().Machines(machineAPINamespace),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		networkInformer.Informer(),
		operatorClient.Informer(),
		machineInformer.Informer(),
	).WithSync(c.sync).ResyncEvery(time.Minute).ToController("ClusterMemberController", eventRecorder.WithComponentSuffix("cluster-member-controller"))
}

func (c *ClusterMemberController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.reconcileMembers(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("ClusterMemberControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "ClusterMemberControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *ClusterMemberController) reconcileMembers(ctx context.Context, recorder events.Recorder) error {
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return fmt.Errorf("could not get list of unhealthy members: %v", err)
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("reconcile aborted found unhealthy etcd members: %v", spew.Sdump(unhealthyMembers))
		return fmt.Errorf("unhealthy members found during reconciling members")
	}

	// Set desired cluster size
	// TODO(haseeb): Always update this to react to control plane size change in cluster config?
	if c.desiredControlPlaneSize == 0 {
		desiredControlPlaneSize, err := ceohelpers.GetMastersReplicaCount(ctx, c.kubeClient)
		if err != nil {
			return fmt.Errorf("failed to get control-plane replica count: %w", err)
		}
		c.desiredControlPlaneSize = desiredControlPlaneSize
	}

	// Update preDrain hooks on Machines
	// Blocks or unblocks machine deletion
	if err = c.reconcileMachineHooks(ctx); err != nil {
		return fmt.Errorf("failed to reconcile machine hooks: %v", err)
	}

	// Add a learner member if next peer found
	peerURL, err := c.getEtcdPeerURLToAdd(ctx)
	if err != nil {
		return fmt.Errorf("could not get etcd peerURL to add :%w", err)
	}
	if peerURL != "" {
		if err = c.etcdClient.MemberAddAsLearner(ctx, peerURL); err != nil {
			return fmt.Errorf("failed to add learner member: %w", err)
		}
	}

	// Scale up the cluser by promoting a learner member
	// if there is room for more voting members
	// Either during bootstrap to N members, or vertical scaling
	if err := c.ensureEtcdLearnerPromotion(ctx, recorder); err != nil {
		return fmt.Errorf("failed to ensure learner promotion: %w", err)
	}

	// Get next member to scale-down
	// This could be for scaling down in a vertical scaling scenario
	// or to remove a learner member to unblock a machine pending deletion
	member, err := c.getEtcdMemberToScaleDown(ctx)
	if err != nil {
		return fmt.Errorf("could not get etcd member for removal:%w", err)
	}
	// Scale down if next member found
	if member != nil {
		if err = c.etcdClient.MemberRemoveID(ctx, member); err != nil {
			return fmt.Errorf("failed to remove member: %w", err)
		}
	}

	return nil
}

// getEtcdPeerURLToAdd returns a PeerURL of the next etcd instance to be added as a learner to the cluster. If
// no elegible member is found return an empty string.
func (c *ClusterMemberController) getEtcdPeerURLToAdd(ctx context.Context) (string, error) {
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return "", err
	}
	// For a desired size of N voting members in a cluster (specified in the install-config)
	// we can only add a new learner member if we're under 2N total members (voting+learners).
	// This abides by the current max limit of N learner members.
	if len(members) >= 2*c.desiredControlPlaneSize {
		klog.V(4).Infof("Skipping learner member addition since the current membership size (%v) exceeds the max limit of 2N (%v) ", len(members), 2*c.desiredControlPlaneSize)
		return "", nil
	}

	memberMap := getMemberMap(members)

	nodes, err := c.nodeLister.List(labels.SelectorFromSet(labels.Set{masterLabel: ""}))
	if err != nil {
		return "", err
	}

	available, machines, err := c.isMachineAPIAvailable(ctx)
	if err != nil {
		return "", fmt.Errorf("could not determine Machine API availability: %v", err)
	}
	nodeToMachineMap := getNodeToMachineMap(machines)

	for _, node := range nodes {
		if node.DeletionTimestamp != nil {
			klog.V(4).Infof("Ignoring node (%s) for scale-up: pending deletion", node.Name)
			continue
		}

		peerURL, err := c.getPeerURLForNode(node)
		if err != nil {
			return "", fmt.Errorf("failed to get peerURL for node: %v", err)
		}

		// Ignore if the node is mapped to a member and already part of the quorum.
		_, isMember := memberMap[peerURL]
		if isMember {
			continue
		}

		// If the Machine API is unavailable then we scale-up as usual
		// without any consideration for vertical scaling
		// e.g the node's machine may be pending deletion
		if !available {
			return peerURL, nil
		}

		// If the Machine API is available we should filter out nodes (i.e Machines)
		// that are pending deletion
		machine, hasMachine := nodeToMachineMap[node.Name]
		if !hasMachine {
			// TODO(haseeb): Should this be an error-and-retry or ignore
			klog.V(4).Infof("Ignoring node (%s) for scale-up: no Machine(Status.NodeRef.Name) found referencing this node", node.Name)
			continue
		}
		if machine.DeletionTimestamp != nil {
			// Ignore since this node was previously scaled-down
			continue
		}
		return peerURL, nil
	}
	return "", nil
}

func (c *ClusterMemberController) ensureEtcdLearnerPromotion(ctx context.Context, recorder events.Recorder) error {
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("could not get etcd member list: %v", err)
	}
	numVotingMembers := countVotingMembers(members)

	available, machines, err := c.isMachineAPIAvailable(ctx)
	if err != nil {
		return fmt.Errorf("could not determine Machine API availability: %v", err)
	}

	memberMachineMap, err := c.getMemberToMachineMap(machines, members)
	if err != nil {
		return fmt.Errorf("could not get machine to member mapping: %v", err)
	}

	for _, member := range members {
		promote, err := c.decideOnPromotion(member, numVotingMembers, available, memberMachineMap)
		if err != nil {
			return fmt.Errorf("failed to decide on promotion for member (%s): %v", member.Name, err)
		}
		if !promote {
			continue
		}

		// Attempt promoting learner to voting member if its log is in sync with the leader.
		err = c.etcdClient.MemberPromote(ctx, member)
		// TODO(sam): this is not working as expected
		// TODO(haseeb): Confirm why?
		if err != nil {
			if errors.Is(err, etcdserver.ErrLearnerNotReady) {
				// Not being ready for promotion is a common and expected occurrence that should just be logged
				klog.V(4).Infof("promotion rejected: etcd learner (%s) is not yet in sync with leader log ", member.PeerURLs[0])
				continue
			}
			// TODO: Emit event for failing promotion for any other error?
			return fmt.Errorf("failed to promote learner member (%s): %v", member.PeerURLs[0], err)
		}
		// Only promote one member at a time
		return nil
	}
	return nil
}

// TODO(haseeb): Line break between args
func (c *ClusterMemberController) decideOnPromotion(member *etcdserverpb.Member, numVotingMembers int, machineAPIAvailable bool, memberMachineMap map[string]memberAndMachine) (bool, error) {
	// Ensure the member is a learner since we can't promote voting members
	if !member.IsLearner {
		return false, nil
	}

	// If the voting membership is below the desired control-plane size
	// then we need to promote without any considerations for deletion protection via machine hooks
	// E.g scale up during bootstrap
	if numVotingMembers < c.desiredControlPlaneSize {
		return true, nil
	}

	// If the voting members meet the minimum cluster size then we consider
	// promotion for a vertical scaling operation.

	// The Machine API must be available else we cannot safely promote and
	// surge up to another voting member without ensuring delete protection for the Machine
	if !machineAPIAvailable {
		return false, nil
	}

	// Don't promote the learner if the cluster already has more than the desired N voting members
	// We only allow a scale up/surge to a max of N+1 voting members during vertical scaling
	if numVotingMembers > c.desiredControlPlaneSize {
		return false, nil
	}

	// Before we can promote and scale up to N+1 voting members, there must be a
	// voting member Machine pending deletion that we can later remove
	// to scale back down to N voting members
	votingMemberPendingDeletion := false
	for _, memberMachine := range memberMachineMap {
		// Only check for a voting member machine that is pending deletion
		if memberMachine.member.IsLearner {
			continue
		}
		if memberMachine.machine.DeletionTimestamp != nil {
			votingMemberPendingDeletion = true
			break
		}
	}
	if !votingMemberPendingDeletion {
		return false, nil
	}

	// We have an existing voting Machine pending deletion
	// so we have room to promote a new voting member

	// Get the machine for this member
	memberMachine, ok := memberMachineMap[member.PeerURLs[0]]
	if !ok {
		return false, fmt.Errorf("could not map member (%s) to its Machine", member.Name)
	}

	// The learner member's Machine must have a PreDrain hook blocking deletion before we can promote it
	// All learner Machines should eventually be reconciled by reconcileMachineHooks() to have PreDrain hooks if missing
	if !hasPreDrainHook(memberMachine.machine.Spec.LifecycleHooks.PreDrain) {
		// TODO(haseeb): Error or requeue so the controller can add the missing hook to the learner member's machine
		klog.V(4).Infof("Learner member (%s) is missing the PreDrain hook on its Machine (%s/%s). Skipping promotion attempt",
			memberMachine.member.Name,
			memberMachine.machine.Namespace,
			memberMachine.machine.Name)
		return false, nil
	}

	// Promote to scale up to N+1 voting members
	return true, nil
}

// getEtcdMemberToScaleDown returns the next etcd member to scale down
// or remove from the cluster membership
func (c *ClusterMemberController) getEtcdMemberToScaleDown(ctx context.Context) (*etcdserverpb.Member, error) {
	// For a non-functional Machine API (aka no Machines)
	// we don't perform vertical scaling or scale down
	available, machines, err := c.isMachineAPIAvailable(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not determine Machine API availability: %v", err)
	}
	if !available {
		klog.Infof("Skipping member scale-down: non-functional Machine API or no running Machines found")
		return nil, nil
	}

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	memberMap := getMemberMap(members)
	numVotingMembers := countVotingMembers(members)

	for _, machine := range machines {
		// We only scale down machines that are pending deletion
		if machine.DeletionTimestamp == nil {
			continue
		}

		// Get etcd member for this Machine
		peerURL, err := c.getPeerURLForMachine(machine)
		if err != nil {
			if errors.Is(err, ErrMissingNodeOnMachine) {
				klog.V(4).Infof("Ignoring machine (%s/%s) for scale down. No node name found on status.NodeRef", machine.Namespace, machine.Name)
				continue
			}
			return nil, fmt.Errorf("failed to get peerURL for machine: %w", err)
		}
		member, isMember := memberMap[peerURL]

		// Ignore non-member machines
		if !isMember {
			continue
		}

		// Learner members can be safely removed from Machines
		// to unblock Machine deletion
		if member.IsLearner {
			return member, nil
		}

		// For a voting member, remove only if the number of voting members exceeds
		// the desired voting membership count. This indicates an N+1 surge-up
		// state so we can remove this older member from the membership.
		if numVotingMembers > c.desiredControlPlaneSize {
			return member, nil
		}
	}
	return nil, nil
}

// reconcileMachineHooks updates the control-plane Machines to add and remove PreDrain hooks
// to block or unblock Machine deletion
func (c *ClusterMemberController) reconcileMachineHooks(ctx context.Context) error {
	// Skip for non-functional Machine API
	available, machines, err := c.isMachineAPIAvailable(ctx)
	if err != nil {
		return fmt.Errorf("could not determine Machine API availability: %v", err)
	}
	if !available {
		klog.V(4).Infof("Skipping machine hook reconciliation: non-functional Machine API or no running Machines found")
		return nil
	}

	removeHookMachines, addHookMachines, err := c.getHooksToAddRemove(ctx, machines)
	if err != nil {
		return fmt.Errorf("failed to get machine hooks to add and remove: %v", err)
	}

	// TODO(haseeb): Add events and Machine conditions for adding/removing hooks
	if err := c.removePreDrainHooks(ctx, removeHookMachines); err != nil {
		return fmt.Errorf("failed to remove PreDrain deletion hooks from machines pending deletion: %v", err)
	}
	if err := c.addPreDrainHooks(ctx, addHookMachines); err != nil {
		return fmt.Errorf("failed to add PreDrain hooks to new learner machines: %v", err)
	}
	return nil
}

// getHooksToAddRemove observes the control-plane Machines and returns
// the Machines that need to be cleared of PreDrain hooks,
// and the new Machines that need to have PreDrain hooks added to them.
func (c *ClusterMemberController) getHooksToAddRemove(ctx context.Context, machines []machinev1beta1.Machine) ([]machinev1beta1.Machine, []machinev1beta1.Machine, error) {
	var removeHookMachines []machinev1beta1.Machine
	var addHookMachines []machinev1beta1.Machine

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return nil, nil, err
	}
	memberMap := getMemberMap(members)

	for _, machine := range machines {

		// Get etcd member for this Machine
		peerURL, err := c.getPeerURLForMachine(machine)
		if err != nil {
			if errors.Is(err, ErrMissingNodeOnMachine) {
				klog.V(4).Infof("Ignoring machine (%s/%s) for hook reconciliation. No node name found on status.NodeRef", machine.Namespace, machine.Name)
				continue
			}
			return nil, nil, fmt.Errorf("failed to get peerURL for machine: %w", err)
		}
		_, isMember := memberMap[peerURL]

		// For non-member machines, clear the hook if it exists
		if !isMember {
			if hasPreDrainHook(machine.Spec.LifecycleHooks.PreDrain) {
				removeHookMachines = append(removeHookMachines, machine)
			}
			continue
		}

		// Member machines not pending deletion should have a PreDrain hook
		// to block drainage of the member and deletion of machine
		if machine.DeletionTimestamp == nil {
			if !hasPreDrainHook(machine.Spec.LifecycleHooks.PreDrain) {
				addHookMachines = append(addHookMachines, machine)
			}
			continue
		}

		// For member Machines that are pending deletion we don't remove
		// the PreDrain hooks until the controller has first safely removed
		// the member from the etcd cluster.
		// At that point the Machine will be reconciled as a non-member
		// and have its hooks removed
	}

	return removeHookMachines, addHookMachines, nil
}

// isMachineAPIAvailable returns true when the Machine API is available by checking
// for the presence of any Machines in the Running phase.
func (c *ClusterMemberController) isMachineAPIAvailable(ctx context.Context) (bool, []machinev1beta1.Machine, error) {
	// TODO(haseeb): Replace label key/value with consts defined in github.com/openshift/machine-api-operator/pkg/controller/machine
	// Currently results a module build constraints error:
	// package github.com/openshift/machine-api-operator: build constraints exclude all Go files in /Users/haseeb/.../pkg/mod/github.com/openshift/machine-api-operator@v0.2.1-0.20211122180933-19198f66b0a6
	k, v := "machine.openshift.io/cluster-api-machine-role", "master"
	ls := labels.SelectorFromSet(labels.Set{k: v})

	ml, err := c.machineNamespacedLister.List(ls)
	if err != nil {
		return false, nil, fmt.Errorf("failed to list master type Machines: %v", err)
	}

	// TODO(haseeb): Convenient for now to use a list of structs here to
	// avoid changing all the method signatures where this is passed down
	machines := []machinev1beta1.Machine{}
	for _, m := range ml {
		machines = append(machines, *m)
	}

	if len(machines) == 0 {
		// No machines means non-functional machine API
		return false, machines, nil
	}

	for _, m := range machines {
		phase := pointer.StringDeref(m.Status.Phase, "")
		if phase == "Running" {
			// Having any Running machine means API is functional
			return true, machines, nil
		}
	}
	return false, machines, nil
}

func (c *ClusterMemberController) removeMembers(ctx context.Context, membersToRemove []string) error {
	for _, m := range membersToRemove {
		if err := c.etcdClient.MemberRemove(ctx, m); err != nil {
			// TODO(haseeb): If the member is already removed, ignore the error (do we get an error for that case?).
			return err
		}
	}
	return nil
}

func (c *ClusterMemberController) removePreDrainHooks(ctx context.Context, removeHookMachines []machinev1beta1.Machine) error {
	for _, m := range removeHookMachines {
		m.Spec.LifecycleHooks.PreDrain = filterPreDrainHook(m.Spec.LifecycleHooks.PreDrain)
		// TODO(haseeb): Patch this instead of update?
		_, err := c.mapiClient.MachineV1beta1().Machines(machineAPINamespace).Update(ctx, &m, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("could not update Machine spec for %s: %v", m.Name, err)
		}
	}
	return nil
}

func (c *ClusterMemberController) addPreDrainHooks(ctx context.Context, addHookMachines []machinev1beta1.Machine) error {
	for _, m := range addHookMachines {
		m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, getPreDrainHook())
		// TODO(haseeb): Patch this instead of update?
		_, err := c.mapiClient.MachineV1beta1().Machines(machineAPINamespace).Update(ctx, &m, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("could not update Machine spec for %s: %v", m.Name, err)
		}
	}
	return nil
}

var ErrMissingNodeOnMachine = errors.New("no Node found for Machine on status.NodeRef")

func (c *ClusterMemberController) getPeerURLForMachine(machine machinev1beta1.Machine) (string, error) {
	if machine.Status.NodeRef == nil || machine.Status.NodeRef.Name == "" {
		return "", ErrMissingNodeOnMachine
	}
	node, err := c.nodeLister.Get(machine.Status.NodeRef.Name)
	if err != nil {
		return "", err
	}

	return c.getPeerURLForNode(node)
}

func (c *ClusterMemberController) getPeerURLForNode(node *corev1.Node) (string, error) {
	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return "", fmt.Errorf("failed to list cluster network: %w", err)
	}

	internalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
	if err != nil {
		return "", fmt.Errorf("failed to get internal IP for node: %w", err)
	}
	return fmt.Sprintf("https://%s:2380", internalIP), nil
}

// machineAndMember is the pairing of an etcd member with the Machine that the member running on
type memberAndMachine struct {
	member  etcdserverpb.Member
	machine machinev1beta1.Machine
}

// getMachineMemberMap returns the mapping of a peerURL to the etcd member and the Machine where it is running
// Only Machines that have a NodeRef and a member from the list will be populated in the map.
//
// NOTE(haseeb): This member is mapped as part of the map value rather than using etcdserverpb.Member as a key
// in map[etcdserverpb.Member]machinev1beta1.Machine.
// The etcdserverpb.Member struct has some protobuf generated fields e.g XXX_NoUnkeyedLiteral which could(?) change across multiple requests
// That would change the key, so we use the peer URL instead
func (c *ClusterMemberController) getMemberToMachineMap(machines []machinev1beta1.Machine, members []*etcdserverpb.Member) (map[string]memberAndMachine, error) {
	memberMap := getMemberMap(members)
	memberToMachineMap := make(map[string]memberAndMachine)

	for _, machine := range machines {
		// Get etcd member peerURL for this Machine
		peerURL, err := c.getPeerURLForMachine(machine)
		if err != nil {
			if errors.Is(err, ErrMissingNodeOnMachine) {
				// TODO(haseeb): Should this be a warning or silent?
				klog.V(4).Infof("Can't map Machine (%s/%s) to etcd member. No node name found on status.NodeRef", machine.Namespace, machine.Name)
				continue
			}
			return nil, fmt.Errorf("failed to get peerURL for machine: %w", err)
		}
		member, isMember := memberMap[peerURL]

		// Ignore the machine if its node is not part of the current etcd cluster membership
		if !isMember {
			continue
		}
		memberToMachineMap[peerURL] = memberAndMachine{
			member:  *member,
			machine: machine,
		}
	}
	return memberToMachineMap, nil
}

// getNodeToMachineMap returns a map of Node name to the Machine referencing it in its machine.Status.NodeRef
func getNodeToMachineMap(machines []machinev1beta1.Machine) map[string]machinev1beta1.Machine {
	nodeToMachineMap := make(map[string]machinev1beta1.Machine)
	for _, machine := range machines {
		if machine.Status.NodeRef == nil || machine.Status.NodeRef.Name == "" {
			// TODO(haseeb): Should this be a warning or silent?
			klog.V(4).Infof("Can't map Machine (%s/%s) to Node. No node name found on status.NodeRef", machine.Namespace, machine.Name)
			continue
		}
		nodeToMachineMap[machine.Status.NodeRef.Name] = machine
	}
	return nodeToMachineMap
}

// getMemberMap returns a mapping of etcd member peerURL to member
func getMemberMap(members []*etcdserverpb.Member) map[string]*etcdserverpb.Member {
	memberMap := make(map[string]*etcdserverpb.Member)
	for _, m := range members {
		// The reason we use the PeerURL and not the member name is because
		// the member.Name field is only populated once the member starts.
		// So unstarted members will all have member.Name=""
		memberMap[m.PeerURLs[0]] = m
	}
	return memberMap
}

func countVotingMembers(members []*etcdserverpb.Member) int {
	numVoting := 0
	for _, m := range members {
		if !m.IsLearner {
			numVoting++
		}
	}
	return numVoting
}

func getPreDrainHook() machinev1beta1.LifecycleHook {
	return machinev1beta1.LifecycleHook{
		Name:  "EtcdQuorum",
		Owner: "etcd-cluster-operator",
	}
}

func hasPreDrainHook(hooks []machinev1beta1.LifecycleHook) bool {
	preDrainHook := getPreDrainHook()
	for _, h := range hooks {
		if h == preDrainHook {
			return true
		}
	}
	return false
}

func filterPreDrainHook(hooks []machinev1beta1.LifecycleHook) []machinev1beta1.LifecycleHook {
	preDrainHook := getPreDrainHook()
	var filtered []machinev1beta1.LifecycleHook
	for _, h := range hooks {
		if h == preDrainHook {
			continue
		}
		filtered = append(filtered, h)
	}
	return filtered
}
