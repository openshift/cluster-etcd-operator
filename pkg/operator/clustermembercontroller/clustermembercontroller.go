package clustermembercontroller

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

// TODO: Update etcd-operator proposal for ClusterMemberController
// https://github.com/openshift/enhancements/blob/master/enhancements/etcd/cluster-etcd-operator.md#etcdmemberscontroller
// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
type ClusterMemberController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient

	podLister       corev1listers.PodLister
	networkLister   configv1listers.NetworkLister
	configMapLister corev1listers.ConfigMapNamespaceLister
	infraLister     configv1listers.InfrastructureLister
	// machineAPIChecker determines if the precondition for this controller is met,
	// this controller can be run only on a cluster that exposes a functional Machine API
	machineAPIChecker ceohelpers.MachineAPIChecker

	masterMachineLister   machinelistersv1beta1.MachineLister
	masterMachineSelector labels.Selector

	masterNodeLister   corev1listers.NodeLister
	masterNodeSelector labels.Selector
}

func NewClusterMemberController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	machineAPIChecker ceohelpers.MachineAPIChecker,
	masterNodeLister corev1listers.NodeLister,
	masterNodeSelector labels.Selector,
	machineLister machinelistersv1beta1.MachineLister,
	masterMachineSelector labels.Selector,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	infrastructureInformer configv1informers.InfrastructureInformer,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
	additionalInformers ...factory.Informer) factory.Controller {
	c := &ClusterMemberController{
		operatorClient:        operatorClient,
		machineAPIChecker:     machineAPIChecker,
		etcdClient:            etcdClient,
		podLister:             kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		networkLister:         networkInformer.Lister(),
		infraLister:           infrastructureInformer.Lister(),
		masterMachineLister:   machineLister,
		masterMachineSelector: masterMachineSelector,
		masterNodeLister:      masterNodeLister,
		masterNodeSelector:    masterNodeSelector,
		configMapLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister().ConfigMaps(operatorclient.TargetNamespace),
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("ClusterMemberController", syncer)

	informers := []factory.Informer{
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
		operatorClient.Informer(),
	}
	informers = append(informers, additionalInformers...)

	return factory.New().ResyncEvery(time.Minute).
		WithBareInformers(networkInformer.Informer(), infrastructureInformer.Informer()).
		WithInformers(informers...).
		WithSync(syncer.Sync).
		WithSyncDegradedOnError(operatorClient).
		ToController("ClusterMemberController", eventRecorder.WithComponentSuffix("cluster-member-controller"))
}

func (c *ClusterMemberController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	return c.reconcileMembers(ctx, syncCtx.Recorder())
}

func (c *ClusterMemberController) reconcileMembers(ctx context.Context, recorder events.Recorder) error {
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return fmt.Errorf("could not get list of unhealthy members: %v", err)
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("unhealthy members: %v", spew.Sdump(unhealthyMembers))
		// see https://issues.redhat.com/browse/OCPBUGS-14296
		return nil
	}

	// Check if DualReplica topology and transition to Pacemaker is complete.
	// In DualReplica clusters, after the transition is complete, member management
	// is handled by the TNF/Pacemaker component to avoid race conditions.
	if staticPodClient, ok := c.operatorClient.(v1helpers.StaticPodOperatorClient); ok {
		if ceohelpers.ShouldSkipMemberManagementForDualReplica(ctx, staticPodClient, c.infraLister) {
			klog.V(4).Infof("skipping member management: DualReplica cluster has completed transition to external etcd (Pacemaker)")
			return nil
		}
	} else {
		klog.Warningf("operatorClient does not implement StaticPodOperatorClient, cannot check DualReplica transition status")
	}

	// Add a learner member if next peer found
	var errs []error
	peerURL, err := c.getEtcdPeerURLToAdd(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("could not get etcd peerURL to add :%w", err))
	}
	if len(peerURL) > 0 {
		err = c.etcdClient.MemberAddAsLearner(ctx, peerURL)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to add learner member :%w", err))
		}
	}

	// Attempt to promote a learner member if there is room for more voting members
	if err := c.ensureEtcdLearnerPromotion(ctx, recorder); err != nil {
		errs = append(errs, fmt.Errorf("failed to promote learner: %w", err))
	}

	return kerrors.NewAggregate(errs)
}

// getEtcdPeerURLToAdd returns a PeerURL of the next etcd instance to be added as a learner to the cluster. If
// no eligible member is found return an empty string.
func (c *ClusterMemberController) getEtcdPeerURLToAdd(ctx context.Context) (string, error) {
	nodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return "", err
	}

	nonVotingMemberNodes, err := c.allNodesMapToVotingMembers(nodes)
	if err != nil {
		return "", fmt.Errorf("failed to map nodes to voting members: %v", err)
	}
	// If all nodes already map to voting members then there are no nodes
	// that need to be scaled-up or added to the membership
	if len(nonVotingMemberNodes) == 0 {
		return "", nil
	}

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return "", err
	}

	// aggregate errors so we don't block on the failure to add one node
	var errs []error
	for _, node := range nonVotingMemberNodes {
		if node.DeletionTimestamp != nil {
			klog.V(2).Infof("Ignoring node (%s) for new member addition as it's pending deletion", node.Name)
			continue
		}

		peerURL, err := c.getPeerURLForNode(node)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get peerURL for node: %v", err))
			continue
		}

		// Ignore if the node's PeerURL is mapped to a member and already part of the quorum.
		if isURLMappedToMember(peerURL, members) {
			continue
		}

		// If the Machine API is unavailable then we can't evaluate the machine's
		// deletion timestamp or deletion hooks so we scale it up without any considerations
		// for vertical scaling
		// This allows UPI clusters without a machine API to scale up
		// Check machine API availability
		isMachineAPIFunctional, err := c.machineAPIChecker.IsFunctional()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to determine Machine API availability: %v", err))
			continue
		}
		if !isMachineAPIFunctional {
			runningNotReady, err := c.isEtcdContainerRunningNotReady(node)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to check if pod is running and not ready: %v", err))
				continue
			}
			if !runningNotReady {
				continue
			}
			return peerURL, nil
		}

		// If the Machine API is functional we should ignore the machine
		// if it is pending deletion to avoid re-adding previously removed members
		internalIP, err := c.getInternalIPForNode(node)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get internal IP for node: %v", err))
			continue
		}
		machine, err := ceohelpers.FindMachineByNodeInternalIP(internalIP, c.masterMachineSelector, c.masterMachineLister)
		if err != nil {
			return "", fmt.Errorf("failed to get machine for node (%s): %v", node.Name, err)
		}
		if machine == nil {
			klog.V(2).Infof("Ignoring node (%s) for scale-up: no Machine found referencing this node's internal IP (%v)", node.Name, internalIP)
			continue
		}
		if machine.DeletionTimestamp != nil {
			// Ignore this node since its member previously was, or is in the process of being removed
			klog.V(2).Infof("Ignoring node (%s) for scale-up since its machine (%s) is pending deletion", node.Name, machine.Name)
			continue
		}

		// Wait until the machine has a deletion hook present before adding it as a member
		if !ceohelpers.HasMachineDeletionHook(machine) {
			klog.V(2).Infof("Ignoring node (%s) for scale-up since its machine (%s) is missing the PreDrain deletion hook (name: %s, owner: %s)", node.Name, machine.Name, ceohelpers.MachineDeletionHookName, ceohelpers.MachineDeletionHookOwner)
			continue
		}

		// Wait until the etcd pod is running but not ready on the node which indicates
		// it is waiting to be added to the cluster membership
		// Note: Checking the pod before adding the member ensures no two unstarted members are added in parallel.
		// If multiple new nodes are added as members in parallel they will fail with
		// a discovery error ("error validating peerURLs: ... member count is unequal") because
		// the unstarted members won't include each other in their ETCD_INITIAL_CLUSTER env.
		runningNotReady, err := c.isEtcdContainerRunningNotReady(node)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to check if pod is running and not ready: %v", err))
			continue
		}
		if !runningNotReady {
			continue
		}

		// We have a valid new node with a running etcd pod to add as a new member
		return peerURL, kerrors.NewAggregate(errs)
	}
	return "", kerrors.NewAggregate(errs)
}

// ensureEtcdLearnerPromotion checks for all learner members in the cluster membership and tries
// to promote them if they have the required machine deletion hooks present
func (c *ClusterMemberController) ensureEtcdLearnerPromotion(ctx context.Context, recorder events.Recorder) error {
	nodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return err
	}

	nonVotingMemberNodes, err := c.allNodesMapToVotingMembers(nodes)
	if err != nil {
		return fmt.Errorf("failed to map nodes to voting members: %v", err)
	}
	// If all nodes already map to voting members then there are no nodes
	// with learner members awaiting promotion
	if len(nonVotingMemberNodes) == 0 {
		return nil
	}

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("could not get etcd member list: %v", err)
	}

	// aggregate errors so we don't block on the failure to promote a member
	var errs []error
	for _, member := range members {
		// Check if the member should be promoted
		promote, err := c.shouldPromote(member)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to decide on promotion for member (%s): %v", member.Name, err))
			continue
		}
		if !promote {
			continue
		}

		// Attempt promoting a learner member
		err = c.etcdClient.MemberPromote(ctx, member)
		if err != nil {
			// Note: Cannot use errors.Is(err, etcdserver.ErrLearnerNotReady) as that is always false in this case.
			// because etcdserver.ErrLearnerNotReady always returns an errors.New() error type
			// So we compare the error strings here instead
			if err.Error() == errors.ErrLearnerNotReady.Error() {
				// Not being ready for promotion is an expected state until the learner catches up
				klog.V(2).Infof("Not ready for promotion: etcd learner member (%s) is not yet in sync with leader's log ", member.PeerURLs[0])
				continue
			}
			errs = append(errs, fmt.Errorf("failed to promote learner member (%s): %v", member.PeerURLs[0], err))
			continue
		}
	}
	return kerrors.NewAggregate(errs)
}

// shouldPromote returns true if the member is a learner that has the correct machine deletion hook present.
// When the machine API is not functional then the learner is always promoted.
func (c *ClusterMemberController) shouldPromote(member *etcdserverpb.Member) (bool, error) {
	// Ensure the member is a learner since we can't promote voting members
	if !member.IsLearner {
		return false, nil
	}

	// If the Machine API is unavailable then we can't evaluate the machine's
	// deletion hooks so we promote without any considerations for vertical scaling
	// This allows UPI clusters without a machine API to scale up
	isMachineAPIFunctional, err := c.machineAPIChecker.IsFunctional()
	if err != nil {
		return false, fmt.Errorf("failed to determine Machine API availability: %v", err)
	}
	if !isMachineAPIFunctional {
		return true, nil
	}

	// The learner member's Machine must have a PreDrain hook blocking deletion before we can promote it
	// All learner Machines should eventually be reconciled by the deletion hooks controller to have PreDrain hooks if missing
	nodeInternalIP, err := ceohelpers.MemberToNodeInternalIP(member)
	if err != nil {
		return false, fmt.Errorf("failed to get node IP from member's PeerURL: %v", err)
	}
	machines, err := ceohelpers.CurrentMemberMachinesWithDeletionHooks(c.masterMachineSelector, c.masterMachineLister)
	if err != nil {
		return false, fmt.Errorf("failed to get master machines: %v", err)
	}
	nodeIPToMachinesWithDeletionHooks := ceohelpers.IndexMachinesByNodeInternalIP(machines)

	machine, hasMachine := nodeIPToMachinesWithDeletionHooks[nodeInternalIP]
	if !hasMachine {
		klog.V(2).Infof("Ignoring member (%s) for promotion: no Machine found referencing this member's IP (%v)", member.Name, nodeInternalIP)
		return false, nil
	}

	// Do not promote if the learner member machine is pending deletion
	if machine.DeletionTimestamp != nil {
		klog.V(2).Infof("Ignoring member (%s) for promotion since its machine (%s) is missing the PreDrain deletion hook (name: %s, owner: %s)", member.Name, machine.Name, ceohelpers.MachineDeletionHookName, ceohelpers.MachineDeletionHookOwner)
		return false, nil
	}

	return true, nil
}

// isEtcdContainerRunningNotReady returns true if the etcd container on the specified node is running but not ready
func (c *ClusterMemberController) isEtcdContainerRunningNotReady(node *corev1.Node) (bool, error) {
	podName := fmt.Sprintf("etcd-%v", node.Name)
	pod, err := c.podLister.Pods(operatorclient.TargetNamespace).Get(podName)
	if apierrors.IsNotFound(err) {
		// etcd member pod not yet created
		// wait until it rolls out
		return false, nil
	}
	if err != nil {
		return false, err
	}

	isEtcdContainerRunning, isEtcdContainerReady := false, false
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name != "etcd" {
			continue
		}
		// check running and ready flags
		isEtcdContainerRunning = containerStatus.State.Running != nil
		isEtcdContainerReady = containerStatus.Ready
		break
	}
	if !isEtcdContainerRunning || isEtcdContainerReady {
		// The pod needs to be running but not ready (i.e discover-etcd-initial-cluster process is waiting to join the cluster)
		klog.V(2).Infof("Skipping %v as the etcd container is in incorrect state, isEtcdContainerRunning = %v, isEtcdContainerReady = %v", podName, isEtcdContainerRunning, isEtcdContainerReady)
		return false, nil
	}

	return true, nil
}

// allNodesMapToVotingMembers returns nodes that don't map to voting members in the etcd cluster membership.
// The voting members are read from the etcd-endpoints configmap
func (c *ClusterMemberController) allNodesMapToVotingMembers(nodes []*corev1.Node) ([]*corev1.Node, error) {
	var nonVotingMemberNodes []*corev1.Node
	currentVotingMemberIPListSet, err := ceohelpers.VotingMemberIPListSet(context.Background(), c.etcdClient)
	if err != nil {
		return nonVotingMemberNodes, fmt.Errorf("failed to get the set of voting members: %v", err)
	}

	for _, node := range nodes {
		nodeInternalIP, err := c.getInternalIPForNode(node)
		if err != nil {
			return nonVotingMemberNodes, err
		}
		if !currentVotingMemberIPListSet.Has(nodeInternalIP) {
			nonVotingMemberNodes = append(nonVotingMemberNodes, node)
		}
	}
	return nonVotingMemberNodes, nil
}

// getPeerURLForNode constructs the etcd member peer URL from the node's internal IP
func (c *ClusterMemberController) getPeerURLForNode(node *corev1.Node) (string, error) {
	internalIP, err := c.getInternalIPForNode(node)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s", net.JoinHostPort(internalIP, "2380")), nil
}

// getInternalIPForNode returns the IPv4 or IPv6 address for the node
func (c *ClusterMemberController) getInternalIPForNode(node *corev1.Node) (string, error) {
	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return "", fmt.Errorf("failed to list cluster network: %w", err)
	}

	// For IPv6 this gives us the unescaped (i.e no []) address since machines and nodes use unescaped
	// literal addresses
	internalIP, _, err := dnshelpers.GetPreferredInternalIPAddressForNodeName(network, node)
	if err != nil {
		return "", fmt.Errorf("failed to get escaped preferred internal IP for node: %w", err)
	}
	return internalIP, nil
}

// isURLMappedToMember returns true if the given peerURL matches any of the members' peerURLs
func isURLMappedToMember(peerURL string, members []*etcdserverpb.Member) bool {
	for _, m := range members {
		if peerURL == m.PeerURLs[0] {
			return true
		}
	}
	return false
}
