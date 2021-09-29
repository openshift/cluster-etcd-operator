package clustermembercontroller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
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

type ClusterMemberController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
	podLister      corev1listers.PodLister
	nodeLister     corev1listers.NodeLister
	networkLister  configv1listers.NetworkLister
}

func NewClusterMemberController(
	operatorClient v1helpers.OperatorClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &ClusterMemberController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
		podLister:      kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:     kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		networkLister:  networkInformer.Lister(),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		networkInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ResyncEvery(time.Minute).ToController("ClusterMemberController", eventRecorder.WithComponentSuffix("cluster-member-controller"))
}

func (c *ClusterMemberController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.reconcileMembers(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "ClusterMemberControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
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
			// TODO: this is not working as expected
			if err != nil && errors.Is(err, etcdserver.ErrLearnerNotReady) {
				recorder.Warningf("ScaleUpCluster", "promotion failed etcd learner is not yet ready %s", member.PeerURLs[0])
				continue
			}
			if err != nil {
				//errs = append(errs, err)
				continue
			}
			recorder.Eventf("ScaleUpCluster", "etcd learner promoted to voting member peerURL: %s", member.PeerURLs[0])
		}
	}
	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}

	return nil
}

func (c *ClusterMemberController) reconcileMembers(ctx context.Context, recorder events.Recorder) error {
	if err := c.ensureEtcdLearnerPromotion(ctx, recorder); err != nil {
		return fmt.Errorf("learner promotion failed: %w", err)
	}

	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return err
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("scale up aborted found unhealthy etcd members: %v", spew.Sdump(unhealthyMembers))
		return fmt.Errorf("unhealthy members found during reconciling members")
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
	err = c.etcdClient.MemberAdd(ctx, peerURL)
	if err != nil {
		recorder.Warningf("ScaleUpCluster", "scale up failed peerURl: %s :%v", peerURL, err)
		return err
	}
	recorder.Eventf("ScaleUpCluster", "scale up etcd learner success peerURl: %s", peerURL)
	return nil
}

// getEtcdPeerURLToScale returns a PeerURL of the next etcd instance to be scaled up in the cluster. If
// no member is found to scale up return empty string.
func (c *ClusterMemberController) getEtcdPeerURLToScale(ctx context.Context) (string, error) {
	pods, err := c.podLister.List(labels.Set{"k8s-app": "etcd-quorum-guard"}.AsSelector())
	if err != nil {
		return "", err
	}
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return "", err
	}

	// Loop quorum-guard pods and scale up etcd to match.
	for _, pod := range pods {
		// Check to see if this member is already part of the quorum.
		for _, member := range members {
			if member.Name == "" || member.Name == pod.Spec.NodeName || member.Name == "etcd-bootstrap" {
				klog.Infof("quorum-guard pod/%s is already mapped to an etcd member peerURL: %s", pod.Name, member.PeerURLs[0])
				continue
			}
		}
		return fmt.Sprintf("https://%s:2380", pod.Status.HostIP), nil
	}

	return "", nil
}
