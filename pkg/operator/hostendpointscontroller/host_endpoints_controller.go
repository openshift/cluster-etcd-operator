package hostendpointscontroller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	workQueueKey = "key"
)

// HostEndpointsController maintains an Endpoints resource with
// the dns names of the current etcd cluster members for use by
// components unable to use the etcd service directly.
type HostEndpointsController struct {
	eventRecorder       events.Recorder
	queue               workqueue.RateLimitingInterface
	cachesToSync        []cache.InformerSynced
	operatorClient      v1helpers.OperatorClient
	machineConfigLister cache.GenericLister
	podLister           corev1listers.PodLister
	endpointsLister     corev1listers.EndpointsLister
	endpointsClient     corev1client.EndpointsGetter
}

func NewHostEndpointsController(
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	dynamicInformers dynamicinformer.DynamicSharedInformerFactory,
) *HostEndpointsController {
	kubeInformersForTargetNamespace := kubeInformersForNamespaces.InformersFor("openshift-etcd")
	endpointsInformer := kubeInformersForTargetNamespace.Core().V1().Endpoints()
	podInformer := kubeInformersForTargetNamespace.Core().V1().Pods()
	machineConfigInformers := dynamicInformers.ForResource(schema.GroupVersionResource{
		Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "controllerconfigs"})

	c := &HostEndpointsController{
		eventRecorder: eventRecorder.WithComponentSuffix("host-etcd-endpoints-controller"),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "HostEndpointsController"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			endpointsInformer.Informer().HasSynced,
			podInformer.Informer().HasSynced,
			machineConfigInformers.Informer().HasSynced,
		},
		operatorClient:      operatorClient,
		machineConfigLister: machineConfigInformers.Lister(),
		podLister:           podInformer.Lister(),
		endpointsLister:     endpointsInformer.Lister(),
		endpointsClient:     kubeClient.CoreV1(),
	}
	operatorClient.Informer().AddEventHandler(c.eventHandler())
	endpointsInformer.Informer().AddEventHandler(c.eventHandler())
	machineConfigInformers.Informer().AddEventHandler(c.eventHandler())
	podInformer.Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *HostEndpointsController) sync() error {
	err := c.syncHostEndpoints()

	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "HostEndpointsDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingHostEndpoints",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("HostEndpointsErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "HostEndpointsDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "HostEndpointsUpdated",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("HostEndpointsErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *HostEndpointsController) syncHostEndpoints() error {
	discoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return fmt.Errorf("unable to determine etcd discovery domain: %v", err)
	}

	// list etcd member pods
	etcdPodRequirement, err := labels.NewRequirement("k8s-app", selection.In, []string{"etcd"})
	if err != nil {
		return err
	}
	etcdPodSelector := labels.NewSelector().Add(*etcdPodRequirement)
	pods, err := c.podLister.List(etcdPodSelector)

	// get dns names of ready etc member pods
	var addresses []string
	for _, pod := range pods {
		var ready bool
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				ready = condition.Status == corev1.ConditionTrue
				break
			}
		}
		if ready {
			dnsName, err := c.getEtcdDNSName(discoveryDomain, pod.Status.HostIP)
			if err != nil {
				return fmt.Errorf("unable to determine dns name for etcd member on node %s: %v", pod.Spec.NodeName, err)
			}
			addresses = append(addresses, dnsName)
		}
	}

	if len(addresses) == 0 {
		return fmt.Errorf("no etcd member pods are ready")
	}

	required := hostEndpointsAsset()

	if required.Annotations == nil {
		required.Annotations = map[string]string{}
	}
	required.Annotations["alpha.installer.openshift.io/dns-suffix"] = discoveryDomain

	sort.Strings(addresses)
	for i, address := range addresses {
		required.Subsets[0].Addresses = append(required.Subsets[0].Addresses, corev1.EndpointAddress{
			Hostname: strings.TrimSuffix(address, "."+discoveryDomain),
			IP:       net.IPv4(byte(192), byte(0), byte(2), byte(i+1)).String(),
		})
	}

	// if etcd-bootstrap exists, keep it (at the end of the list)
	existing, err := c.endpointsLister.Endpoints("openshift-etcd").Get("host-etcd")
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if !errors.IsNotFound(err) {
		for _, endpointAddress := range existing.Subsets[0].Addresses {
			if endpointAddress.Hostname == "etcd-bootstrap" {
				required.Subsets[0].Addresses = append(required.Subsets[0].Addresses, *endpointAddress.DeepCopy())
				break
			}
		}
	}

	return c.applyEndpoints(required)
}

func hostEndpointsAsset() *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: v1.ObjectMeta{
			Name:      "host-etcd",
			Namespace: "openshift-etcd",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Ports: []corev1.EndpointPort{
					{
						Name:     "etcd",
						Port:     2379,
						Protocol: "TCP",
					},
				},
			},
		},
	}
}

func (c *HostEndpointsController) getEtcdDiscoveryDomain() (string, error) {
	controllerConfigObj, err := c.machineConfigLister.Get("machine-config-controller")
	if err != nil {
		return "", err
	}
	controllerConfig := controllerConfigObj.(*unstructured.Unstructured).UnstructuredContent()

	etcdDiscoveryDomain, ok, err := unstructured.NestedString(controllerConfig, "spec", "etcdDiscoveryDomain")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("controllerconfigs/machine-config-controller missing .spec.etcdDiscoveryDomain")
	}
	return etcdDiscoveryDomain, nil
}

func (c *HostEndpointsController) getEtcdDNSName(discoveryDomain, ip string) (string, error) {
	dnsName, err := reverseLookup("etcd-server-ssl", "tcp", discoveryDomain, ip)
	if err != nil {
		return "", err
	}
	return dnsName, nil
}

// returns the target from the SRV record that resolves to ip.
func reverseLookup(service, proto, name, ip string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == ip {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}

func (c *HostEndpointsController) applyEndpoints(required *corev1.Endpoints) error {
	existing, err := c.endpointsLister.Endpoints("openshift-etcd").Get("host-etcd")
	if errors.IsNotFound(err) {
		_, err := c.endpointsClient.Endpoints("openshift-etcd").Create(required)
		if err != nil {
			c.eventRecorder.Warningf("EndpointsCreateFailed", "Failed to create endpoints/%s -n %s: %v", required.Name, required.Namespace, err)
			return err
		}
		c.eventRecorder.Warningf("EndpointsCreated", "Created endpoints/%s -n %s because it was missing", required.Name, required.Namespace)
	}
	if err != nil {
		return err
	}
	modified := resourcemerge.BoolPtr(false)
	toWrite := existing.DeepCopy()
	resourcemerge.EnsureObjectMeta(modified, &toWrite.ObjectMeta, required.ObjectMeta)
	if !equality.Semantic.DeepEqual(existing.Subsets, required.Subsets) {
		toWrite.Subsets = make([]corev1.EndpointSubset, len(required.Subsets))
		for i := range required.Subsets {
			required.Subsets[i].DeepCopyInto(&(toWrite.Subsets)[i])
		}
		*modified = true
	}
	if !*modified {
		// no update needed
		return nil
	}
	jsonPatch := resourceapply.JSONPatchNoError(existing, toWrite)
	if klog.V(4) {
		klog.Infof("Endpoints %q changes: %v", required.Namespace+"/"+required.Name, jsonPatch)
	}
	_, err = c.endpointsClient.Endpoints("openshift-etcd").Update(toWrite)
	if err != nil {
		c.eventRecorder.Warningf("EndpointsUpdateFailed", "Failed to update endpoints/%s -n %s: %v", required.Name, required.Namespace, err)
		return err
	}
	c.eventRecorder.Warningf("EndpointsUpdated", "Updated endpoints/%s -n %s because it changed: %v", required.Name, required.Namespace, jsonPatch)
	return nil
}

func (c *HostEndpointsController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	klog.Infof("Starting HostEtcdEndpointsController")
	defer klog.Infof("Shutting down HostEtcdEndpointsController")
	if !cache.WaitForCacheSync(ctx.Done(), c.cachesToSync...) {
		return
	}
	go wait.Until(c.runWorker, time.Second, ctx.Done())
	<-ctx.Done()
}

func (c *HostEndpointsController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *HostEndpointsController) processNextWorkItem() bool {
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

func (c *HostEndpointsController) eventHandler() cache.ResourceEventHandler {
	// eventHandler queues the operator to check spec and status
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
