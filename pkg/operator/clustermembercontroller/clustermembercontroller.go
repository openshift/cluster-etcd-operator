package clustermembercontroller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/davecgh/go-spew/spew"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

const (
	masterLabel                = "node-role.kubernetes.io/master"
	msgPromotionFailedNotReady = "promotion failed etcd learner is not yet ready"
	msgLearnerPromoted         = "etcd learner promoted to voting member"
)

// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
type ClusterMemberController struct {
	operatorClient  v1helpers.OperatorClient
	etcdClient      etcdcli.EtcdClient
	configMapLister corev1listers.ConfigMapLister
	podLister       corev1listers.PodLister
	nodeLister      corev1listers.NodeLister
	networkLister   configv1listers.NetworkLister
}

func NewClusterMemberController(
	operatorClient v1helpers.OperatorClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &ClusterMemberController{
		operatorClient:  operatorClient,
		etcdClient:      etcdClient,
		configMapLister: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister(),
		podLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:      kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		networkLister:   networkInformer.Lister(),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		networkInformer.Informer(),
		operatorClient.Informer(),
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
		errMsg := "unhealthy members found during reconciling members"
		klog.V(4).Infof("%s: %v", errMsg, spew.Sdump(unhealthyMembers))
		return fmt.Errorf(errMsg)
	}

	if err := c.ensureEtcdLearnerPromotion(ctx, recorder); err != nil {
		return fmt.Errorf("learner promotion failed: %w", err)
	}

	// etcd is healthy, decide if we need to scale.
	peerURL, err := c.getEtcdPeerURLToScale(ctx)
	if err != nil {
		return fmt.Errorf("could not get etcd peer host :%w", err)
	}
	if peerURL == "" {
		// No more work left to do.
		return nil
	}

	recorder.Eventf("ScaleUpCluster", "scale up attempt peerURl: %s", peerURL)
	err = c.etcdClient.MemberAddAsLearner(ctx, peerURL)
	if err != nil {
		recorder.Warningf("ScaleUpCluster", "scale up failed peerURl: %s :%v", peerURL, err)
		return err
	}
	recorder.Eventf("ScaleUpCluster", "scale up etcd learner success peerURl: %s", peerURL)
	return nil
}

func (c *ClusterMemberController) ensureEtcdLearnerPromotion(ctx context.Context, recorder events.Recorder) error {
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("could not get etcd member list: %v", err)
	}

	var errs []error
	for _, member := range members {
		// Promote learner to voting member if log is in sync with leader.
		if member.IsLearner {
			err := c.etcdClient.MemberPromote(ctx, member.ID)
			if err != nil && errors.Is(err, etcdserver.ErrLearnerNotReady) {
				recorder.Warningf("ScaleUpCluster", "%s: %s", msgPromotionFailedNotReady, member.PeerURLs[0])
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			recorder.Eventf("ScaleUpCluster", "%s: %s", msgLearnerPromoted, member.PeerURLs[0])
		}
	}
	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}

	return nil
}

// getEtcdPeerURLToScale checks the exiting nodes for etcd members and returns a peerURL that can be added to the cluster.
func (c *ClusterMemberController) getEtcdPeerURLToScale(ctx context.Context) (string, error) {
	desiredMemberCount, err := ceohelpers.GetMastersReplicaCount(c.configMapLister)
	if err != nil {
		return "", fmt.Errorf("failed to get control-plane replica count: %w", err)
	}

	if desiredMemberCount == 0 {
		return "", fmt.Errorf("invalid install-config control plane replica count: %d", desiredMemberCount)
	}

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get etcd member list: %v", err)
	}

	currentMemberCount := len(members)
	maxMemberCount := desiredMemberCount + 1 // Surge up
	if currentMemberCount == int(maxMemberCount) {
		klog.Warningf("Skipping scale up etcd membership at maximum size: %d", desiredMemberCount+1)
		return "", nil
	}
	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return "", fmt.Errorf("failed to list cluster network: %w", err)
	}

	nodes, err := c.nodeLister.List(labels.SelectorFromSet(labels.Set{masterLabel: ""}))
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %v", err)
	}
	// Sort nodes by created timestamp.
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].CreationTimestamp.Before(&nodes[j].CreationTimestamp)
	})

	for _, node := range nodes {
		nodeInternalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return "", fmt.Errorf("failed to get internal IP for node: %w", err)
		}
		// Check to see if this member is already part of the quorum.
		peerURL := fmt.Sprintf("https://%s:2380", nodeInternalIP)
		if etcdcli.IsPeerURLMember(members, peerURL) {
			klog.V(4).Infof("node/%s is already mapped to an etcd member", node.Name)
			continue
		}
		return peerURL, nil
	}

	return "", nil
}
