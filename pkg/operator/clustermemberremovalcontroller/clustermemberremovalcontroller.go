package clustermemberremovalcontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// errNotFound a sentinel errors indicating a given item hasn't been found
var errNotFound = errors.New("not found")

type clusterMemberRemovalController struct {
	operatorClient                    operatorv1helpers.StaticPodOperatorClient
	etcdClient                        etcdcli.EtcdClient
	masterNodeLister                  corev1listers.NodeLister
	masterMachineLister               machinelistersv1beta1.MachineLister
	networkLister                     configv1listers.NetworkLister
	configMapListerForTargetNamespace corev1listers.ConfigMapNamespaceLister
	// machineAPIChecker determines if the precondition for this controller is met,
	// this controller can be run only on a cluster that exposes a functional Machine API
	machineAPIChecker     ceohelpers.MachineAPIChecker
	masterMachineSelector labels.Selector
	masterNodeSelector    labels.Selector
	configMapLister       corev1listers.ConfigMapLister

	lastTimeScaleDownEventWasSent time.Time
}

// NewClusterMemberRemovalController removes an etcd member if the machine and a node for the etcd member is gone and the machine-api is active
//
// Note:
//
//	since this controller needs to reconcile only master nodes and machine objects
//	make sure nodeInformer and machineInformer contain only filtered data
//	otherwise it might be expensive to react to every node update in larger installations
func NewClusterMemberRemovalController(
	operatorClient operatorv1helpers.StaticPodOperatorClient,
	etcdClient etcdcli.EtcdClient,
	machineAPIChecker ceohelpers.MachineAPIChecker,
	masterMachineSelector labels.Selector, masterNodeSelector labels.Selector,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	masterNodeInformer cache.SharedIndexInformer,
	masterMachineInformer cache.SharedIndexInformer,
	networkInformer configv1informers.NetworkInformer,
	configMapLister corev1listers.ConfigMapLister,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &clusterMemberRemovalController{
		operatorClient:                    operatorClient,
		etcdClient:                        etcdClient,
		machineAPIChecker:                 machineAPIChecker,
		masterMachineSelector:             masterMachineSelector,
		masterNodeSelector:                masterNodeSelector,
		masterNodeLister:                  corev1listers.NewNodeLister(masterNodeInformer.GetIndexer()),
		masterMachineLister:               machinelistersv1beta1.NewMachineLister(masterMachineInformer.GetIndexer()),
		networkLister:                     networkInformer.Lister(),
		configMapListerForTargetNamespace: kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister().ConfigMaps(operatorclient.TargetNamespace),
		configMapLister:                   configMapLister,
	}
	return factory.New().
		WithSync(c.sync).
		WithSyncDegradedOnError(operatorClient).
		ResyncEvery(33*time.Minute). // make it slow since nodes are updated every few minutes
		WithBareInformers(networkInformer.Informer()).
		WithInformers(
			masterNodeInformer,
			masterMachineInformer,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
		).ToController("ClusterMemberRemovalController", eventRecorder.WithComponentSuffix("cluster-member-removal-controller"))
}

func (c *clusterMemberRemovalController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	// skip reconciling this controller, if bootstrapping is not completed
	bootstrapComplete, err := ceohelpers.IsBootstrapComplete(c.configMapLister, c.operatorClient, c.etcdClient)
	if err != nil {
		return fmt.Errorf("IsBootstrapComplete failed to determine bootstrap status: %w", err)
	}
	if !bootstrapComplete {
		return nil
	}

	// only attempt to scale down if the machine API is functional
	if isFunctional, err := c.machineAPIChecker.IsFunctional(); err != nil {
		return err
	} else if !isFunctional {
		return nil
	}

	var errs []error
	if err := c.removeMemberWithoutMachine(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.attemptToRemoveLearningMember(ctx, syncCtx.Recorder()); err != nil {
		errs = append(errs, err)
	}
	if err := c.attemptToScaleDown(ctx, syncCtx.Recorder()); err != nil {
		errs = append(errs, err)
	}
	return kerrors.NewAggregate(errs)

}

// attemptToScaleDown attempts to remove a voting member only once we have identified that
// a Machine resource is being deleted and a replacement member has been created
func (c *clusterMemberRemovalController) attemptToScaleDown(ctx context.Context, recorder events.Recorder) error {
	currentVotingMemberIPListSet, err := ceohelpers.VotingMemberIPListSet(ctx, c.etcdClient)
	if err != nil {
		return err
	}

	desiredControlPlaneReplicasCount, err := ceohelpers.ReadDesiredControlPlaneReplicasCount(c.operatorClient)
	if err != nil {
		return err
	}
	if desiredControlPlaneReplicasCount == 0 {
		return fmt.Errorf("desired control plane replicas count cannot be empty")
	}

	liveVotingMembers, err := c.getAllVotingMembers(ctx)
	if err != nil {
		return fmt.Errorf("could not list etcd members: %w", err)
	}

	if len(liveVotingMembers) < desiredControlPlaneReplicasCount {
		klog.V(2).Infof("Ignoring scale-down since the number of voting, non-bootstrap etcd members is (%d) < desired number of control-plane replicas (%d)", len(liveVotingMembers), desiredControlPlaneReplicasCount)
		return nil
	}

	// machines with master role and deletion hook
	memberMachines, err := ceohelpers.CurrentMemberMachinesWithDeletionHooks(c.masterMachineSelector, c.masterMachineLister)
	if err != nil {
		return fmt.Errorf("could not find master machines with deletion hook: %w", err)
	}

	var votingMembersMachines []*machinev1beta1.Machine
	for memberMachineIP, memberMachine := range ceohelpers.IndexMachinesByNodeInternalIP(memberMachines) {
		if currentVotingMemberIPListSet.Has(memberMachineIP) {
			votingMembersMachines = append(votingMembersMachines, memberMachine)
		}
	}

	votingMembersMachinesPendingDeletion := ceohelpers.FilterMachinesPendingDeletion(votingMembersMachines)
	if len(votingMembersMachinesPendingDeletion) == 0 {
		return nil
	}

	// do not trust data in the cache, compare with the current state
	healthyLiveVotingMembers, err := c.getHealthyVotingMembers(ctx)
	if err != nil {
		return fmt.Errorf("could not list etcd healthy members: %w", err)
	}

	// scaling down invariant
	minTolerableQuorum, err := etcdcli.MinimumTolerableQuorum(desiredControlPlaneReplicasCount)
	if err != nil {
		klog.V(2).Infof("etcd cluster could not determine minimum quorum required. desiredControlPlaneReplicasCount is %v. minimum quorum required is %v: %w", desiredControlPlaneReplicasCount, minTolerableQuorum, err)
	}

	if len(healthyLiveVotingMembers) < minTolerableQuorum {
		klog.V(2).Infof("ignoring scale down since the number of healthy voting, non-bootstrap etcd members (%d) < minimum required to maintain quorum (%d)", len(healthyLiveVotingMembers), minTolerableQuorum)
		if time.Now().After(c.lastTimeScaleDownEventWasSent.Add(5 * time.Minute)) {
			recorder.Eventf("ScaleDown", "Ignoring scale down since the number of healthy voting, non-bootstrap etcd members (%d) < minimum required to maintain quorum (%d)", len(healthyLiveVotingMembers), minTolerableQuorum)
			c.lastTimeScaleDownEventWasSent = time.Now()
		}
		return nil
	}

	// before removing a voting member, we need to make sure that
	// current members are healthy otherwise we might lose quorum
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return fmt.Errorf("could not get a list of unhealthy members: %v", err)
	}

	// map machines pending deletion to node internal ip
	votingMemberMachinesPendingDeletionIndex := ceohelpers.IndexMachinesByNodeInternalIP(votingMembersMachinesPendingDeletion)
	// find unhealthy machine pending deletion to remove first from the cluster, without violating quorum
	var unhealthyVotingMemberMachinesPendingDeletion []*machinev1beta1.Machine
	if len(unhealthyMembers) > 0 {
		var unhealthyMembersURLs []string
		for _, unhealthyMember := range unhealthyMembers {
			unhealthyMemberNodeInternalIP, err := ceohelpers.MemberToNodeInternalIP(unhealthyMember)
			if err != nil {
				return fmt.Errorf("cannot get node internal ip for unhealthy member %v: %w", unhealthyMember, err)
			}
			unhealthyVotingMemberMachinePendingDeletion, ok := votingMemberMachinesPendingDeletionIndex[unhealthyMemberNodeInternalIP]
			if ok {
				unhealthyVotingMemberMachinesPendingDeletion = append(unhealthyVotingMemberMachinesPendingDeletion, unhealthyVotingMemberMachinePendingDeletion)
			}
			if len(unhealthyMember.PeerURLs) > 0 {
				unhealthyMembersURLs = append(unhealthyMembersURLs, unhealthyMember.PeerURLs[0])
			} else {
				unhealthyMembersURLs = append(unhealthyMembersURLs, unhealthyMember.Name)
			}
		}
		if len(unhealthyVotingMemberMachinesPendingDeletion) == 0 {
			klog.V(2).Infof("cannot proceed with scaling down, unhealthy voting etcd members found: %v but none are pending deletion", unhealthyMembersURLs)
			return fmt.Errorf("cannot proceed with scaling down, unhealthy voting etcd members found: %v but none are pending deletion", unhealthyMembersURLs)
		}
	}

	// Don't scale down if all members are healthy and the cluster is already at the desired control-plane size
	// When at the desired control-plane size, scale down is only allowed if a member is unhealthy so that a new machine can replace it.
	// For healthy members, deleting a machine should result in a new machine being created and member addition before scale down can occur
	if reflect.DeepEqual(liveVotingMembers, healthyLiveVotingMembers) && len(healthyLiveVotingMembers) <= desiredControlPlaneReplicasCount {
		klog.V(2).Infof("skip scale down: all voting, non-bootstrap members are healthy and membership does not exceed the desired cluster size of %v", desiredControlPlaneReplicasCount)
		return nil
	}

	// remove the unhealthy machine pending deletion first
	// if no unhealthy machine pending deletion found, then attempt to scale down the healthy machines pending deletion
	if len(unhealthyVotingMemberMachinesPendingDeletion) > 0 {
		klog.V(2).Infof("found unhealthy voting etcd members with machine pending deletion: %v", unhealthyVotingMemberMachinesPendingDeletion)
		votingMembersMachinesPendingDeletion = append(unhealthyVotingMemberMachinesPendingDeletion, votingMembersMachinesPendingDeletion...)
	}

	var allErrs []error
	for _, votingMemberMachinePendingDeletion := range votingMembersMachinesPendingDeletion {
		removed, errs := c.attemptToRemoveMemberFor(ctx, liveVotingMembers, votingMemberMachinePendingDeletion, recorder)
		if removed {
			break // we want to remove only one member at a time
		}
		if len(errs) > 0 {
			allErrs = append(allErrs, errs...)
		}
	}

	return kerrors.NewAggregate(allErrs)
}

func (c *clusterMemberRemovalController) removeMemberWithoutMachine(ctx context.Context) error {
	etcdEndpointsConfigMap, err := c.configMapListerForTargetNamespace.Get("etcd-endpoints")
	if err != nil {
		return err // should not happen
	}
	if len(etcdEndpointsConfigMap.Data) == 0 {
		return nil
	}
	for _, potentialMemberToRemoveIP := range etcdEndpointsConfigMap.Data {
		memberLocator := potentialMemberToRemoveIP
		machine, err := c.getMachineForMember(potentialMemberToRemoveIP)
		if err != nil && err != errNotFound {
			return fmt.Errorf("unable to get a machine for member: %v, err: %v", memberLocator, err)
		}
		if machine != nil {
			klog.V(4).Infof("cannot remove member: %v because a machine resource still exists (%v/%v)", memberLocator, machine.Name, machine.UID)
			continue
		}

		node, err := c.getNodeForMember(potentialMemberToRemoveIP)
		if err != nil && err != errNotFound {
			return fmt.Errorf("unable to get a node for member: %v, err: %v", memberLocator, err)
		}
		if node != nil {
			klog.V(4).Infof("cannot remove member: %v because a node resource still exists (%v/%v)", memberLocator, node.Name, node.UID)
			continue
		}

		// it looks like we have found a member that isn't backed by a machine nor a node
		// issue a live request to the etcd cluster and remove it from the cluster
		memberList, err := c.etcdClient.MemberList(ctx)
		if err != nil {
			return err
		}
		for _, member := range memberList {
			memberIP, err := ceohelpers.MemberToNodeInternalIP(member)
			if err != nil {
				return err
			}
			if memberIP != potentialMemberToRemoveIP {
				continue
			}
			// a member without a node must be reported as unhealthy
			isMemberHealthy, err := c.etcdClient.IsMemberHealthy(ctx, member)
			if err != nil {
				return err
			}
			if isMemberHealthy {
				return fmt.Errorf("cannot remove member: %v because it is reported as healthy but it doesn't have a machine nor a node resource", memberLocator)
			}

			memberLocator = fmt.Sprintf("[ url: %v, name: %v, id: %v ]", memberIP, member.Name, member.ID)
			err = c.etcdClient.MemberRemove(ctx, member.ID)
			if err != nil {
				return fmt.Errorf("failed to remove member: %v, err: %v", memberLocator, err)
			}
			klog.V(2).Infof("successfully removed member: %v from the cluster", memberLocator)
			break
		}
	}
	return nil
}

// attemptToRemoveLearningMember attempts to remove a learning member pending deletion regardless of whether a replacement member has been found
func (c *clusterMemberRemovalController) attemptToRemoveLearningMember(ctx context.Context, recorder events.Recorder) error {
	currentVotingMemberIPListSet, err := ceohelpers.VotingMemberIPListSet(ctx, c.etcdClient)
	if err != nil {
		return err
	}
	memberMachines, err := ceohelpers.CurrentMemberMachinesWithDeletionHooks(c.masterMachineSelector, c.masterMachineLister)
	if err != nil {
		return fmt.Errorf("could not find master machines with deletion hook: %w", err)
	}
	var learningMachines []*machinev1beta1.Machine
	for memberMachineIP, memberMachine := range ceohelpers.IndexMachinesByNodeInternalIP(memberMachines) {
		if !currentVotingMemberIPListSet.Has(memberMachineIP) {
			learningMachines = append(learningMachines, memberMachine)
		}
	}

	learnerMachinesPendingDeletion := ceohelpers.FilterMachinesPendingDeletion(learningMachines)
	if len(learnerMachinesPendingDeletion) == 0 {
		return nil
	}

	// based on the data in the cache it looks like we have something to do
	// get the live members, to observe the current state and the member IDs
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return err
	}

	var liveLearnerMembers []*etcdserverpb.Member
	for _, member := range members {
		if !member.IsLearner {
			continue
		}
		liveLearnerMembers = append(liveLearnerMembers, member)
	}

	var allErrs []error
	for _, learnerMachinePendingDeletion := range learnerMachinesPendingDeletion {
		_, errs := c.attemptToRemoveMemberFor(ctx, liveLearnerMembers, learnerMachinePendingDeletion, recorder)
		if len(errs) > 0 {
			allErrs = append(allErrs, errs...)
		}
	}

	return kerrors.NewAggregate(allErrs)
}

func (c *clusterMemberRemovalController) getMachineForMember(memberInternalIP string) (*machinev1beta1.Machine, error) {
	node, err := c.getNodeForMember(memberInternalIP)
	if err != nil && err != errNotFound {
		return nil, err
	}

	// node is gone, make sure we don't have a machine with
	// the same internal IP as the node would have had
	if node == nil {
		nodeInternalIP := memberInternalIP
		machine, err := ceohelpers.FindMachineByNodeInternalIP(nodeInternalIP, c.masterMachineSelector, c.masterMachineLister)
		if err != nil {
			return nil, err
		}
		return machine, nil
	}

	machines, err := c.masterMachineLister.List(c.masterMachineSelector)
	if err != nil {
		return nil, err
	}
	for _, machine := range machines {
		if machine.Status.NodeRef != nil && machine.Status.NodeRef.Name == node.Name {
			return machine, nil
		}
	}
	return nil, errNotFound
}

func (c *clusterMemberRemovalController) getNodeForMember(memberInternalIP string) (*corev1.Node, error) {
	masterNodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return nil, err
	}

	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster network: %w", err)
	}

	for _, masterNode := range masterNodes {
		internalNodeIP, _, err := dnshelpers.GetPreferredInternalIPAddressForNodeName(network, masterNode)
		if err != nil {
			return nil, fmt.Errorf("failed to get internal IP for node: %w", err)
		}
		if memberInternalIP == internalNodeIP {
			return masterNode, nil
		}
	}
	return nil, errNotFound
}

func (c *clusterMemberRemovalController) attemptToRemoveMemberFor(ctx context.Context, members []*etcdserverpb.Member, machinePendingDeletion *machinev1beta1.Machine, recorder events.Recorder) (removed bool, errs []error) {
	for _, member := range members {
		memberIP, err := ceohelpers.MemberToNodeInternalIP(member)
		if err != nil {
			memberLocator := fmt.Sprintf("[ name: %v, id: %v ]", member.Name, member.ID)
			errs = append(errs, fmt.Errorf("failed to get an IP for member: %v, err: %v", memberLocator, err))
			continue // ignore unhealthy members
		}
		if hasInternalIP(machinePendingDeletion, memberIP) {
			memberLocator := fmt.Sprintf("[ url: %v, name: %v, id: %v ]", memberIP, member.Name, member.ID)
			if err := c.etcdClient.MemberRemove(ctx, member.ID); err != nil {
				errs = append(errs, fmt.Errorf("failed to remove member: %v, err: %v", memberLocator, err))
				break // stop, we have found a matching member for the provided machine
			}
			recorder.Eventf("ScaleDown", "successfully removed member: %v from the cluster", memberLocator)
			klog.V(2).Infof("successfully removed member: %v from the cluster", memberLocator)
			removed = true
			break // stop, we have found a matching member for the provided machine
		}
	}
	return removed, errs
}

func (c *clusterMemberRemovalController) getHealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	members, err := c.etcdClient.HealthyVotingMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list healthy voting members: %w", err)
	}
	return members, nil
}

func (c *clusterMemberRemovalController) getAllVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	members, err := c.etcdClient.VotingMemberList(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list all voting members: %w", err)
	}
	return members, nil
}

func hasInternalIP(machine *machinev1beta1.Machine, memberInternalIP string) bool {
	for _, addr := range machine.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address == memberInternalIP {
			return true
		}
	}
	return false
}
