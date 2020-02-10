package clustermembercontroller2

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey      = "key"
	etcdCertFile      = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile       = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
	// todo: need to understand how to make this dynamic across all platforms
	totalDesiredEtcd = 3
)

// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
type ClusterMemberController struct {
	operatorClient                         v1helpers.OperatorClient
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory
	endpointsLister                        corev1listers.EndpointsLister
	podLister                              corev1listers.PodLister

	etcdDiscoveryDomain  string
	numberOfReadyMembers int

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewClusterMemberController(
	operatorClient v1helpers.OperatorClient,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
	etcdDiscoveryDomain string,
) *ClusterMemberController {
	c := &ClusterMemberController{
		operatorClient:  operatorClient,
		endpointsLister: kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Lister(),
		podLister:       kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Lister(),

		etcdDiscoveryDomain:  etcdDiscoveryDomain,
		numberOfReadyMembers: 0,

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Informer().HasSynced,
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().HasSynced,
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().HasSynced,
			operatorClient.Informer().HasSynced,
		},
		queue:                                  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController2"),
		kubeInformersForOpenshiftEtcdNamespace: kubeInformersForOpenshiftEtcdNamespace,
		eventRecorder:                          eventRecorder.WithComponentSuffix("cluster-member-controller-2"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	operatorClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ClusterMemberController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController2")
	defer klog.Infof("Shutting down ClusterMemberController2")

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
			Type:    "ClusterMemberController2Degraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberController2UpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "ClusterMemberController2Degraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *ClusterMemberController) reconcileMembers() error {
	etcdHealthy, err := c.isEtcdHealthy()
	if err != nil {
		return err
	}
	if !etcdHealthy {
		c.eventRecorder.Eventf("WaitingOnEtcdMember", "waiting for all member of etcd to be healthy")
		return nil
	}

	// etcd is healthy, decide if we need to scale
	peerFQDN, dnsErrs := c.getPeerToScale()
	if len(dnsErrs) != 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberControllerDNSLookupDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorGettingDNSNames",
			Message: fmt.Sprintf("errod resolving etcd dns names: %#v", dnsErrs),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberController2UpdatingStatus", updateErr.Error())
			return updateErr
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "ClusterMemberControllerDNSLookupDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberController2UpdatingStatus", updateErr.Error())
			return updateErr
		}
	}
	if peerFQDN == "" && c.numberOfReadyMembers == totalDesiredEtcd {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
			v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    "ClusterMemberControllerScalingProgressing",
				Status:  operatorv1.ConditionFalse,
				Reason:  "AsExpected",
				Message: "Scaling etcd membership completed",
			}),
			// todo: remove this make bootsrap remove independent
			v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    "BootstrapSafeToRemove",
				Status:  operatorv1.ConditionTrue,
				Reason:  "AsExpected",
				Message: "Scaling etcd membership has completed",
			}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberController2UpdatingStatus", updateErr.Error())
			return updateErr
		}
		// no more work left to do
		return nil
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberControllerScalingProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Scaling",
			Message: fmt.Sprintf("Scaling etcd membership, adding member %s", peerFQDN),
		}),
		// todo: remove this make bootsrap remove independent
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapSafeToRemove",
			Status:  operatorv1.ConditionFalse,
			Reason:  "EtcdScaling",
			Message: fmt.Sprintf("%d/%d member scaled, waiting for others", c.numberOfReadyMembers, totalDesiredEtcd),
		}))
	if updateErr != nil {
		c.eventRecorder.Warning("ClusterMemberController2UpdatingStatus", updateErr.Error())
		return updateErr
	}

	err = c.AddMember(peerFQDN)
	if err != nil {
		return err
	}
	return nil
}

func (c *ClusterMemberController) getEtcdClient() (*clientv3.Client, error) {
	endpoints, err := c.Endpoints()
	if err != nil {
		return nil, err
	}

	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
	}

	tlsInfo := transport.TLSInfo{
		CertFile:      etcdCertFile,
		KeyFile:       etcdKeyFile,
		TrustedCAFile: etcdTrustedCAFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()

	cfg := &clientv3.Config{
		DialOptions: dialOptions,
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return cli, err
}

func (c *ClusterMemberController) Endpoints() ([]string, error) {
	hostEtcd, err := c.endpointsLister.Endpoints(clustermembercontroller.EtcdEndpointNamespace).Get(clustermembercontroller.EtcdEndpointName)
	if err != nil {
		c.eventRecorder.Warningf("ErrorGettingHostEtcd", "error occured while getting host-etcd endpoint: %#v", err)
		return []string{}, err
	}
	if len(hostEtcd.Subsets) == 0 {
		c.eventRecorder.Warningf("EtcdAddressNotFound", "could not find etcd address in host-etcd")
		return []string{}, fmt.Errorf("could not find etcd address in host-etcd")
	}
	var endpoints []string
	for _, addr := range hostEtcd.Subsets[0].Addresses {
		endpoints = append(endpoints, fmt.Sprintf("https://%s.%s:2379", addr.Hostname, c.etcdDiscoveryDomain))
	}
	return endpoints, nil
}

func (c *ClusterMemberController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

func (c *ClusterMemberController) isEtcdHealthy() (bool, error) {
	// getting a new client everytime because we dont know what etcd-membership looks like
	etcdClient, err := c.getEtcdClient()
	defer etcdClient.Close()
	if err != nil {
		c.eventRecorder.Warningf("ErrorGettingEtcdClient", "error getting etcd client: %#v", err)
		return false, err
	}

	memberList, err := etcdClient.MemberList(context.Background())
	if err != nil {
		c.eventRecorder.Warningf("ErrorGettingMemberList", "error getting etcd member list: %#v", err)
		return false, err
	}
	for _, member := range memberList.Members {
		statusResp, err := etcdClient.Status(context.Background(), member.ClientURLs[0])
		if err != nil {
			c.eventRecorder.Warningf("EtcdMemberNotHealthy", "etcd member %s is not healthy: %#v", member.Name, err)
			// since error is indicative of unhealthy member, not returning
			// the actual error
			return false, nil
		}
		klog.V(4).Infof("etcd member %s is healthy committed and with %s index", member.Name, statusResp.RaftIndex)
	}
	return true, nil
}

func (c *ClusterMemberController) getPeerToScale() (string, []error) {
	// list etcd member pods
	etcdPodRequirement, err := labels.NewRequirement("k8s-app", selection.In, []string{"etcd"})
	if err != nil {
		c.eventRecorder.Warningf("ErrorGettingLabelSelector", "error get k8s-app=etcd selector: %#v", err)
		return "", []error{err}
	}
	etcdPodSelector := labels.NewSelector().Add(*etcdPodRequirement)
	pods, err := c.podLister.List(etcdPodSelector)
	if err != nil {
		c.eventRecorder.Warningf("ErrorListingPods", "error listing pods with label selector k8s-app=etcd: %#v", err)
		return "", []error{err}
	}

	// go through the list of all pods, pick one peerFQDN to return from unready pods
	// and collect dns resolution errors on the way.
	var dnsErrors []error
	peerFQDN := ""
	for _, pod := range pods {
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				ready = condition.Status == corev1.ConditionTrue
				break
			}
		}
		if !ready {
			c.eventRecorder.Eventf("FoundPodToScale", "found pod %s to scale in etcd membership", pod.Name)
			peerFQDN, err = clustermembercontroller.ReverseLookupSelf("etcd-server-ssl", "tcp", c.etcdDiscoveryDomain, pod.Status.HostIP)
			if err != nil {
				c.eventRecorder.Warningf("ErrorGettingPodDNS", "error looking up dns for pod %s from ip %s: %#v", pod.Name, pod.Status.HostIP, err)
				dnsErrors = append(dnsErrors, err)
			}
		} else {
			klog.V(4).Infof("found pod %s ready with %s peerFQDN", pod.Name, peerFQDN)
			c.numberOfReadyMembers += 1
		}
	}
	if len(dnsErrors) != 0 {
		return peerFQDN, dnsErrors
	}
	return "", nil
}

func (c *ClusterMemberController) AddMember(peerFQDN string) error {
	etcdClient, err := c.getEtcdClient()
	defer etcdClient.Close()
	if err != nil {
		c.eventRecorder.Warningf("ErrorGettingEtcdClient", "error getting etcd client: %#v", err)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := etcdClient.MemberAdd(ctx, []string{fmt.Sprintf("https://%s:2380", peerFQDN)})
	cancel()
	if err != nil {
		c.eventRecorder.Warningf("ErrorAddingMember", "error adding member with peerFQDN %s to etcd api: %#v", peerFQDN, err)
		return err
	}
	c.eventRecorder.Eventf("MemberAdded", "member %s added to etcd membership %#v", resp.Member.Name, resp.Members)
	return nil
}
