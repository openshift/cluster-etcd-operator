package clustermembercontroller2

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// todo: need to understand how to make this dynamic across all platforms
	totalDesiredEtcd = 3
)

// watches the etcd static pods, picks one unready pod and adds
// to etcd membership only if all existing members are running healthy
// skips if any one member is unhealthy.
type ClusterMemberController struct {
	dynamicClient   dynamic.Interface
	operatorClient  v1helpers.OperatorClient
	kubeInformers   informers.SharedInformerFactory
	endpointsLister corev1listers.EndpointsLister
	podLister       corev1listers.PodLister
	nodeLister      corev1listers.NodeLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewClusterMemberController(
	dynamicClient dynamic.Interface,
	operatorClient v1helpers.OperatorClient,
	kubeInformers informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *ClusterMemberController {
	c := &ClusterMemberController{
		dynamicClient:   dynamicClient,
		operatorClient:  operatorClient,
		endpointsLister: kubeInformers.Core().V1().Endpoints().Lister(),
		podLister:       kubeInformers.Core().V1().Pods().Lister(),
		nodeLister:      kubeInformers.Core().V1().Nodes().Lister(),

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformers.Core().V1().Endpoints().Informer().HasSynced,
			kubeInformers.Core().V1().Pods().Informer().HasSynced,
			kubeInformers.Core().V1().ConfigMaps().Informer().HasSynced,
			kubeInformers.Core().V1().Nodes().Informer().HasSynced,
			operatorClient.Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController2"),
		kubeInformers: kubeInformers,
		eventRecorder: eventRecorder.WithComponentSuffix("cluster-member-controller-2"),
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
	etcdHealthy, err := c.areAllEtcdMembersHealthy()
	if err != nil {
		return err
	}
	if !etcdHealthy {
		c.eventRecorder.Eventf("WaitingOnEtcdMember", "waiting for all member of etcd to be healthy")
		return nil
	}

	// etcd is healthy, decide if we need to scale
	unreadyPods, err := c.getUnreadyEtcdPods()
	if err != nil {
		return err
	}

	if len(unreadyPods) == 0 {
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
			Message: "Scaling etcd membership",
		}),
		// todo: remove this make bootsrap remove independent
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapSafeToRemove",
			Status:  operatorv1.ConditionFalse,
			Reason:  "EtcdScaling",
			Message: fmt.Sprintf("waiting for %d/%d pods to be scaled", len(unreadyPods), totalDesiredEtcd),
		}))
	if updateErr != nil {
		return updateErr
	}

	podFQDN, err := c.getValidPodFQDNToScale(unreadyPods)
	if err != nil {
		return err
	}

	err = c.AddMember(podFQDN)
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
		CertFile:      "/var/run/secrets/etcd-client/tls.crt",
		KeyFile:       "/var/run/secrets/etcd-client/tls.key",
		TrustedCAFile: "/var/run/configmaps/etcd-ca/ca-bundle.crt",
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
	etcdDiscoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return []string{}, err
	}
	hostEtcd, err := c.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
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
		endpoints = append(endpoints, fmt.Sprintf("https://%s.%s:2379", addr.Hostname, etcdDiscoveryDomain))
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

func (c *ClusterMemberController) areAllEtcdMembersHealthy() (bool, error) {
	// getting a new client everytime because we dont know what etcd-membership looks like
	etcdClient, err := c.getEtcdClient()
	if err != nil {
		return false, fmt.Errorf("error getting etcd client: %w", err)
	}
	defer etcdClient.Close()

	memberList, err := etcdClient.MemberList(context.Background())
	if err != nil {
		return false, fmt.Errorf("error getting etcd member list: %w", err)
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

func (c *ClusterMemberController) getUnreadyEtcdPods() ([]*corev1.Pod, error) {
	// list etcd member pods
	pods, err := c.podLister.List(labels.Set{"app": "etcd"}.AsSelector())
	if err != nil {
		return nil, err
	}

	// go through the list of all pods, pick one peerFQDN to return from unready pods
	// and collect dns resolution errors on the way.
	var unreadyPods []*corev1.Pod
	for _, pod := range pods {
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				ready = condition.Status == corev1.ConditionTrue
				klog.V(4).Infof("found pod %s ready", pod.Name)
				break
			}
		}
		if !ready {
			c.eventRecorder.Eventf("FoundPodToScale", "found pod %s to scale in etcd membership", pod.Name)
			unreadyPods = append(unreadyPods, pod)
		}
	}
	return unreadyPods, nil
}

func (c *ClusterMemberController) AddMember(peerFQDN string) error {
	etcdClient, err := c.getEtcdClient()
	if err != nil {
		c.eventRecorder.Warningf("ErrorGettingEtcdClient", "error getting etcd client: %#v", err)
		return err
	}
	defer etcdClient.Close()

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

func (c *ClusterMemberController) getEtcdDiscoveryDomain() (string, error) {
	controllerConfig, err := c.dynamicClient.
		Resource(schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "controllerconfigs"}).
		Get("machine-config-controller", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	etcdDiscoveryDomain, ok, err := unstructured.NestedString(controllerConfig.Object, "spec", "etcdDiscoveryDomain")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("controllerconfigs/machine-config-controller missing .spec.etcdDiscoveryDomain")
	}
	return etcdDiscoveryDomain, nil
}

// getValidPodFQDNToScale goes through the list on unready pods and
// returns a resolvable  podFQDN. If none of the DNSes are available
// yet it will return collected errors.
func (c *ClusterMemberController) getValidPodFQDNToScale(unreadyPods []*corev1.Pod) (string, error) {
	etcdDiscoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return "", err
	}
	errorStrings := []string{}
	for _, p := range unreadyPods {
		if p.Spec.NodeName == "" {
			return "", fmt.Errorf("node name empty for %s", p.Name)
		}
		nodeInternalIP, err := c.getNodeInternalIP(p.Spec.NodeName)
		if err != nil {
			errorStrings = append(errorStrings, err.Error())
		}
		podFQDN, err := clustermembercontroller.ReverseLookupSelf("etcd-server-ssl", "tcp", etcdDiscoveryDomain, nodeInternalIP)
		if err != nil {
			errorStrings = append(errorStrings, err.Error())
		}
		return podFQDN, nil
	}
	if len(errorStrings) > 0 {
		return "", fmt.Errorf("%s", strings.Join(errorStrings, ","))
	}
	return "", fmt.Errorf("cannot get a valid podFQDN to scale")
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
