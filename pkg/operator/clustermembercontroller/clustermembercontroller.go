package clustermembercontroller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
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

// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
type ClusterMemberController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
	podLister      corev1listers.PodLister
	nodeLister     corev1listers.NodeLister
	networkLister  configv1listers.NetworkLister
	// machineAPIChecker determines if the precondition for this controller is met,
	// this controller can be run only on a cluster that exposes a functional Machine API
	machineAPIChecker ceohelpers.MachineAPIChecker

	masterMachineLister   machinelistersv1beta1.MachineLister
	masterMachineSelector labels.Selector
}

func NewClusterMemberController(
	operatorClient v1helpers.OperatorClient,
	machineAPIChecker ceohelpers.MachineAPIChecker,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	masterMachineInformer cache.SharedIndexInformer,
	masterMachineSelector labels.Selector,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &ClusterMemberController{
		operatorClient:        operatorClient,
		machineAPIChecker:     machineAPIChecker,
		etcdClient:            etcdClient,
		podLister:             kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:            kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		networkLister:         networkInformer.Lister(),
		masterMachineLister:   machinelistersv1beta1.NewMachineLister(masterMachineInformer.GetIndexer()),
		masterMachineSelector: masterMachineSelector,
	}
	return factory.New().ResyncEvery(time.Minute).
		WithBareInformers(networkInformer.Informer()).
		WithInformers(
			kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
			kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
			operatorClient.Informer(),
			masterMachineInformer,
		).WithSync(c.sync).WithSyncDegradedOnError(operatorClient).ResyncEvery(time.Minute).ToController("ClusterMemberController", eventRecorder.WithComponentSuffix("cluster-member-controller"))
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
	}

	etcdHost, err := c.getEtcdPeerHostToScale(podToAdd)
	if err != nil {
		return fmt.Errorf("could not get etcd peer host :%w", err)
	}

	if machineForHostPendingDeletion, err := c.isMachinePendingDeletionFor(etcdHost); err != nil {
		return err
	} else if machineForHostPendingDeletion {
		return nil
	}

	recorder.Eventf("FoundPodToScale", "found pod to add to etcd membership: %v", podToAdd.Name)
	err = c.etcdClient.MemberAdd(ctx, fmt.Sprintf("https://%s", net.JoinHostPort(etcdHost, "2380")))
	if err != nil {
		return fmt.Errorf("could not add member :%w", err)
	}
	return nil
}

func (c *ClusterMemberController) isMachinePendingDeletionFor(etcdHost string) (bool, error) {
	if isFunctional, err := c.machineAPIChecker.IsFunctional(); err != nil {
		return false, err
	} else if !isFunctional {
		// always return false when the machine API is off
		// otherwise we won't be able to add any member
		return false, nil
	}

	masterMachines, err := c.masterMachineLister.List(c.masterMachineSelector)
	if err != nil {
		return false, err
	}

	// one we have functional machine API, get the corresponding machine and check DeletionTimestamp
	switch machineForEtcdHost, hasMachine := ceohelpers.IndexMachinesByNodeInternalIP(masterMachines)[etcdHost]; {
	case hasMachine && machineForEtcdHost.DeletionTimestamp != nil:
		klog.V(2).Infof("member: %v has a machine that is pending deletion: %v", etcdHost, machineForEtcdHost.Name)
		return true, nil
	case !hasMachine:
		return false, fmt.Errorf("unable to find machine for member: %v", etcdHost)
	default:
		// we've found a machine and it is not pending deletion
		return false, nil
	}
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

	ip, _, err := dnshelpers.GetPreferredInternalIPAddressForNodeName(network, node)
	return ip, err
}
