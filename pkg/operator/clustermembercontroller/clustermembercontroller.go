package clustermembercontroller

import (
	"fmt"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey = "key"
)

// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
type ClusterMemberController struct {
	operatorClient       v1helpers.OperatorClient
	etcdClient           etcdcli.EtcdClient
	kubeInformers        informers.SharedInformerFactory
	endpointsLister      corev1listers.EndpointsLister
	podLister            corev1listers.PodLister
	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewClusterMemberController(
	operatorClient v1helpers.OperatorClient,
	kubeInformers informers.SharedInformerFactory,
	infrastructureInformer configv1informers.InfrastructureInformer,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) *ClusterMemberController {
	c := &ClusterMemberController{
		operatorClient:       operatorClient,
		etcdClient:           etcdClient,
		endpointsLister:      kubeInformers.Core().V1().Endpoints().Lister(),
		podLister:            kubeInformers.Core().V1().Pods().Lister(),
		nodeLister:           kubeInformers.Core().V1().Nodes().Lister(),
		infrastructureLister: infrastructureInformer.Lister(),

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformers.Core().V1().Endpoints().Informer().HasSynced,
			kubeInformers.Core().V1().Pods().Informer().HasSynced,
			kubeInformers.Core().V1().ConfigMaps().Informer().HasSynced,
			kubeInformers.Core().V1().Nodes().Informer().HasSynced,
			infrastructureInformer.Informer().HasSynced,
			operatorClient.Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController"),
		kubeInformers: kubeInformers,
		eventRecorder: eventRecorder.WithComponentSuffix("cluster-member-controller"),
	}
	kubeInformers.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	kubeInformers.Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())
	kubeInformers.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	operatorClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ClusterMemberController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController")
	defer klog.Infof("Shutting down ClusterMemberController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	go wait.Until(func() {
		c.queue.Add(workQueueKey)
	}, time.Minute, stopCh)

	<-stopCh
}

func (c *ClusterMemberController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ClusterMemberController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

func (c *ClusterMemberController) sync() error {
	err := c.reconcileMembers()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberControllerUpdatingStatus", updateErr.Error())
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

func (c *ClusterMemberController) reconcileMembers() error {
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers()
	if err != nil {
		return err
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("unhealthy members: %v", spew.Sdump(unhealthyMembers))
		memberNames := []string{}
		for _, member := range unhealthyMembers {
			memberNames = append(memberNames, member.Name)
		}
		c.eventRecorder.Eventf("UnhealthyEtcdMember", "unhealthy members: %v", strings.Join(memberNames, ","))
		return nil
	}

	// etcd is healthy, decide if we need to scale
	podToAdd, err := c.getEtcdPodToAddToMembership()
	switch {
	case err != nil:
		return err
	case podToAdd == nil:
		// no more work left to do
		return nil
	default:
		c.eventRecorder.Eventf("FoundPodToScale", "found pod to add to etcd membership: %v", podToAdd.Name)
	}

	podFQDN, err := c.getValidPodFQDNToScale(podToAdd)
	if err != nil {
		return err
	}
	err = c.etcdClient.MemberAdd(fmt.Sprintf("https://%s:2380", podFQDN))
	if err != nil {
		return err
	}
	return nil
}

func (c *ClusterMemberController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

func (c *ClusterMemberController) getEtcdPodToAddToMembership() (*corev1.Pod, error) {
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
		if strings.HasPrefix(pod.Name, "etcd-member") {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				ready = condition.Status == corev1.ConditionTrue
				klog.V(4).Infof("found pod %s ready", pod.Name)
				break
			}
		}
		if ready {
			continue
		}

		// now check to see if this member is already part of the quorum.  This logically requires being able to map every
		// type of member name we have ever created.  The most important for now is the nodeName.
		etcdMember, err := c.etcdClient.GetMember(pod.Spec.NodeName)
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

func (c *ClusterMemberController) getEtcdDiscoveryDomain() (string, error) {
	infrastructure, err := c.infrastructureLister.Get("cluster")
	if err != nil {
		return "", err
	}
	etcdDiscoveryDomain := infrastructure.Status.EtcdDiscoveryDomain
	if len(etcdDiscoveryDomain) == 0 {
		return "", fmt.Errorf("infrastructures.config.openshit.io/cluster missing .status.etcdDiscoveryDomain")
	}
	return etcdDiscoveryDomain, nil
}

// getValidPodFQDNToScale goes through the list on unready pods and
// returns a resolvable  podFQDN. If none of the DNSes are available
// yet it will return collected errors.
func (c *ClusterMemberController) getValidPodFQDNToScale(podToAdd *corev1.Pod) (string, error) {
	etcdDiscoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return "", err
	}

	if podToAdd.Spec.NodeName == "" {
		return "", fmt.Errorf("node name empty for %s", podToAdd.Name)
	}
	nodeInternalIP, err := c.getNodeInternalIP(podToAdd.Spec.NodeName)
	if err != nil {
		return "", err
	}
	podFQDN, err := ReverseLookupSelf("etcd-server-ssl", "tcp", etcdDiscoveryDomain, nodeInternalIP)
	if err != nil {
		return "", err
	}

	return podFQDN, nil
}

func (c *ClusterMemberController) getNodeInternalIP(nodeName string) (string, error) {
	node, err := c.nodeLister.Get(nodeName)
	if err != nil {
		return "", err
	}
	if node.Status.Addresses == nil {
		return "", fmt.Errorf("cannot get node IP address, addresses for node %s is nil", nodeName)
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("unable to get internal IP address for node %s", nodeName)
}
