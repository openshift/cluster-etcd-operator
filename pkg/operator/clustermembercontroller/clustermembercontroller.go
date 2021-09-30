package clustermembercontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
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

func (c *ClusterMemberController) reconcileMembers(ctx context.Context, recorder events.Recorder) error {
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return fmt.Errorf("could not get list of unhealthy members: %v", err)
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("unhealthy members: %v", spew.Sdump(unhealthyMembers))
		return fmt.Errorf("unhealthy members found during reconciling members")
	}

	// etcd is healthy, decide if we need to scale
	podToAdd, err := c.getEtcdPodToAddToMembership(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("could not get etcd pod: %w", err)
	case podToAdd == nil:
		// no more work left to do
		return nil
	default:
		recorder.Eventf("FoundPodToScale", "found pod to add to etcd membership: %v", podToAdd.Name)
	}

	etcdHost, err := c.getEtcdPeerHostToScale(podToAdd)
	if err != nil {
		return fmt.Errorf("could not get etcd peer host :%w", err)
	}
	err = c.etcdClient.MemberAdd(ctx, fmt.Sprintf("https://%s:2380", etcdHost))
	if err != nil {
		return fmt.Errorf("could not add member :%w", err)
	}
	return nil
}

func (c *ClusterMemberController) getEtcdPodToAddToMembership(ctx context.Context) (*corev1.Pod, error) {
	// list etcd member pods
	pods, err := c.podLister.List(labels.Set{"app": "etcd"}.AsSelector())
	if err != nil {
		return nil, err
	}

	// go through the list of all pods, pick one peerFQDN to return from unready pods
	// and collect dns resolution errors on the way.
	for _, pod := range pods {
		if !strings.HasPrefix(pod.Name, "etcd-") {
			continue
		}
		isEtcdContainerRunning, isEtcdContainerReady := false, false
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name != "etcd" {
				continue
			}
			// set running and ready flags
			isEtcdContainerRunning = containerStatus.State.Running != nil
			isEtcdContainerReady = containerStatus.Ready
			break
		}
		if !isEtcdContainerRunning || isEtcdContainerReady {
			continue
		}

		// now check to see if this member is already part of the quorum.  This logically requires being able to map every
		// type of member name we have ever created.  The most important for now is the nodeName.
		etcdMember, err := c.etcdClient.GetMember(ctx, pod.Spec.NodeName)
		switch {
		case apierrors.IsNotFound(err):
			return pod, nil
		case err != nil:
			return nil, err
		default:
			klog.Infof("skipping unready pod %q because it is already an etcd member: %#v", pod.Name, etcdMember)
		}
	}
	return nil, nil
}

// getValidPodFQDNToScale goes through the list on unready pods and
// returns a resolvable  podFQDN. If none of the DNSes are available
// yet it will return collected errors.
func (c *ClusterMemberController) getEtcdPeerHostToScale(podToAdd *corev1.Pod) (string, error) {
	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return "", err
	}
	node, err := c.nodeLister.Get(podToAdd.Spec.NodeName)
	if err != nil {
		return "", err
	}

	return dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
}
