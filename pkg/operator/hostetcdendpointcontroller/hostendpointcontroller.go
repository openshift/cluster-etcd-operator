package hostetcdendpointcontroller

import (
	"fmt"
	"math/rand"
	"strconv"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey = "key"
	subnetPrefix = "192.0.2."
	maxIPAddress = 255
)

type HostEtcdEndpointController struct {
	clientset                              corev1client.Interface
	operatorConfigClient                   v1helpers.OperatorClient
	queue                                  workqueue.RateLimitingInterface
	kubeInformersForOpenshiftEtcdnamespace informers.SharedInformerFactory
	healthyEtcdMemberGetter                HealthyEtcdMembersGetter
	eventRecorder                          events.Recorder
}

func NewHostEtcdEndpointcontroller(
	clientset corev1client.Interface,
	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *HostEtcdEndpointController {
	h := &HostEtcdEndpointController{
		clientset:                              clientset,
		operatorConfigClient:                   operatorConfigClient,
		queue:                                  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "HostEtcdEndpointController"),
		kubeInformersForOpenshiftEtcdnamespace: kubeInformersForOpenshiftEtcdNamespace,
		healthyEtcdMemberGetter:                NewHealthyEtcdMemberGetter(operatorConfigClient),
		eventRecorder:                          eventRecorder.WithComponentSuffix("host-etcd-endpoint-controller"),
	}
	operatorConfigClient.Informer().AddEventHandler(h.eventHandler())
	h.kubeInformersForOpenshiftEtcdnamespace.Core().V1().Endpoints().Informer().AddEventHandler(h.eventHandler())
	//TODO: remove this when liveness probe is added to etcd-member.yaml.
	h.kubeInformersForOpenshiftEtcdnamespace.Core().V1().Pods().Informer().AddEventHandler(h.eventHandler())
	return h
}

func (h *HostEtcdEndpointController) Run(i int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer h.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController")
	defer klog.Infof("Shutting down ClusterMemberController")

	if !cache.WaitForCacheSync(stopCh,
		h.operatorConfigClient.Informer().HasSynced,
		h.kubeInformersForOpenshiftEtcdnamespace.Core().V1().Endpoints().Informer().HasSynced,
		h.kubeInformersForOpenshiftEtcdnamespace.Core().V1().Pods().Informer().HasSynced) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	go wait.Until(h.runWorker, time.Second, stopCh)

	<-stopCh
}

func (h *HostEtcdEndpointController) runWorker() {
	for h.processNextWorkItem() {
	}
}

func (h *HostEtcdEndpointController) processNextWorkItem() bool {
	dsKey, quit := h.queue.Get()
	if quit {
		return false
	}
	defer h.queue.Done(dsKey)

	err := h.sync()
	if err == nil {
		h.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	h.queue.AddRateLimited(dsKey)

	return true
}

func (h *HostEtcdEndpointController) eventHandler() cache.ResourceEventHandler {
	// eventHandler queues the operator to check spec and status
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { h.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { h.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { h.queue.Add(workQueueKey) },
	}
}

func (h *HostEtcdEndpointController) sync() error {
	operatorSpec, _, _, err := h.operatorConfigClient.GetOperatorState()
	if err != nil {
		return err
	}
	switch operatorSpec.ManagementState {
	case operatorv1.Managed:
	case operatorv1.Unmanaged:
		return nil
	case operatorv1.Removed:
		// TODO should we support removal?
		return nil
	default:
		h.eventRecorder.Warningf("ManagementStateUnknown", "Unrecognized operator management state %q", operatorSpec.ManagementState)
		return nil
	}

	ep, err := h.clientset.CoreV1().Endpoints(clustermembercontroller.EtcdEndpointNamespace).
		Get(clustermembercontroller.EtcdHostEndpointName, v1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting %s/%s endpoint: %#v",
			clustermembercontroller.EtcdEndpointNamespace,
			clustermembercontroller.EtcdEndpointName,
			err,
		)
		return err
	}
	if len(ep.Subsets) != 1 {
		klog.Errorf("length of host endpoint subset is not equal to 1")
		return fmt.Errorf("unexpected length of host endpoint subset")
	}

	newEP := ep.DeepCopy()

	newSubset, err := h.getNewAddressSubset(newEP.Subsets[0].Addresses)
	if err != nil {
		klog.Errorf("error getting new address subset: %#v", err)
	}

	newEP.Subsets[0].Addresses = newSubset
	_, err = h.clientset.CoreV1().Endpoints(clustermembercontroller.EtcdEndpointNamespace).Update(newEP)
	return err
}

func (h *HostEtcdEndpointController) getNewAddressSubset(addresses []corev1.EndpointAddress) ([]corev1.EndpointAddress, error) {
	hostnames := make([]string, len(addresses))
	ipAddresses := make([]string, len(addresses))
	for _, h := range addresses {
		hostnames = append(hostnames, h.Hostname)
		ipAddresses = append(ipAddresses, h.IP)
	}
	healthyMembers, err := h.healthyEtcdMemberGetter.GetHealthyEtcdMembers()
	if err != nil {
		return nil, err
	}
	add, remove := diff(hostnames, healthyMembers)

	newSubset := []corev1.EndpointAddress{}

	for _, h := range addresses {
		if ok := in(remove, h.Hostname); !ok {
			newSubset = append(newSubset, h)
		}
	}

	//  Since max of master etcd is 7 safe to not reuse the ip addresses of removed members.
	newIPAddresses := pickUniqueIPAddress(ipAddresses, len(add))
	for i, m := range add {
		newSubset = append(newSubset, corev1.EndpointAddress{
			IP:       newIPAddresses[i],
			Hostname: m,
		})
	}

	makeEtcdBootstrapLast(newSubset)

	return newSubset, nil
}

// since etcd-bootstrap will be removed after successfull scale up
// we need to make sure it is the last in the list of endpoint addresses
// the kube apiserver reads from this list and will use it as the last
// endpoint if all the other fails
func makeEtcdBootstrapLast(addresses []corev1.EndpointAddress) {
	if len(addresses) < 2 {
		return
	}
	for index, addr := range addresses {
		if addr.Hostname == "etcd-bootstrap" && index != len(addresses)-1 {
			e := addr
			addresses = append(addresses[0:index], addresses[index+1:]...)
			addresses = append(addresses, e)
			return
		}
	}
}

func pickUniqueIPAddress(assignedIPAddresses []string, newIPAddressNeeded int) []string {
	ipAddresses := make([]string, len(assignedIPAddresses))
	newIPAddresses := make([]string, newIPAddressNeeded)
	copy(ipAddresses, assignedIPAddresses)
	src := rand.NewSource(time.Now().Unix())
	r := rand.New(src)
	for i := 0; i < newIPAddressNeeded; i++ {
		tryIP := subnetPrefix + strconv.Itoa(r.Intn(maxIPAddress))
		for ok := in(ipAddresses, tryIP); ok; {
			tryIP = subnetPrefix + strconv.Itoa(r.Intn(maxIPAddress))
			ok = in(ipAddresses, tryIP)
		}
		newIPAddresses[i] = tryIP
		ipAddresses = append(ipAddresses, tryIP)
	}
	return newIPAddresses
}

func diff(hostnames, healthyMembers []string) (add, remove []string) {
	for _, h := range hostnames {
		if ok := in(healthyMembers, h); !ok {
			if h == "etcd-bootstrap" {
				continue
			}
			remove = append(remove, h)
		}
	}

	for _, m := range healthyMembers {
		if ok := in(hostnames, m); !ok {
			add = append(add, m)
		}
	}
	return
}

func in(list []string, member string) bool {
	for _, element := range list {
		if element == member {
			return true
		}
	}
	return false
}
