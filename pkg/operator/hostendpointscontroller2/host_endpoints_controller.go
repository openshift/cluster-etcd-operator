package hostendpointscontroller2

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
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

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	workQueueKey = "key"
)

// HostEndpoints2Controller maintains an Endpoints resource with
// IP addresses for etcd.  It should never depend on DNS directly or transitively.
// in 4.4, we will abandon the old resource in etcd.
type HostEndpoints2Controller struct {
	operatorClient  v1helpers.OperatorClient
	nodeLister      corev1listers.NodeLister
	endpointsLister corev1listers.EndpointsLister
	endpointsClient corev1client.EndpointsGetter

	eventRecorder events.Recorder
	queue         workqueue.RateLimitingInterface
	cachesToSync  []cache.InformerSynced
}

func NewHostEndpoints2Controller(
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
) *HostEndpoints2Controller {
	kubeInformersForTargetNamespace := kubeInformers.InformersFor(operatorclient.TargetNamespace)
	endpointsInformer := kubeInformersForTargetNamespace.Core().V1().Endpoints()
	kubeInformersForCluster := kubeInformers.InformersFor("")
	nodeInformer := kubeInformersForCluster.Core().V1().Nodes()

	c := &HostEndpoints2Controller{
		eventRecorder: eventRecorder.WithComponentSuffix("host-etcd-2-endpoints-controller2"),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "HostEndpoints2Controller"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			endpointsInformer.Informer().HasSynced,
			nodeInformer.Informer().HasSynced,
		},
		operatorClient:  operatorClient,
		nodeLister:      nodeInformer.Lister(),
		endpointsLister: endpointsInformer.Lister(),
		endpointsClient: kubeClient.CoreV1(),
	}
	operatorClient.Informer().AddEventHandler(c.eventHandler())
	endpointsInformer.Informer().AddEventHandler(c.eventHandler())
	nodeInformer.Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *HostEndpoints2Controller) sync(ctx context.Context) error {
	err := c.syncHostEndpoints2(ctx)

	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "HostEndpoints2Degraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingHostEndpoints2",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("HostEndpoints2ErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "HostEndpoints2Degraded",
		Status: operatorv1.ConditionFalse,
		Reason: "HostEndpoints2Updated",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("HostEndpoints2ErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *HostEndpoints2Controller) syncHostEndpoints2(ctx context.Context) error {
	required := hostEndpointsAsset()

	// create endpoint addresses for each node
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return fmt.Errorf("unable to list expected etcd member nodes: %v", err)
	}
	endpointAddresses := []corev1.EndpointAddress{}
	for _, node := range nodes {
		var nodeInternalIP string
		for _, nodeAddress := range node.Status.Addresses {
			if nodeAddress.Type == corev1.NodeInternalIP {
				nodeInternalIP = nodeAddress.Address
				break
			}
		}
		if len(nodeInternalIP) == 0 {
			return fmt.Errorf("unable to determine internal ip address for node %s", node.Name)
		}

		endpointAddresses = append(endpointAddresses, corev1.EndpointAddress{
			IP:       nodeInternalIP,
			NodeName: &node.Name,
		})
	}

	required.Subsets[0].Addresses = endpointAddresses
	if len(required.Subsets[0].Addresses) == 0 {
		return fmt.Errorf("no master nodes are present")
	}

	return c.applyEndpoints(ctx, required)
}

func hostEndpointsAsset() *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-etcd-2",
			Namespace: operatorclient.TargetNamespace,
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

func (c *HostEndpoints2Controller) applyEndpoints(ctx context.Context, required *corev1.Endpoints) error {
	existing, err := c.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd-2")
	if errors.IsNotFound(err) {
		_, err := c.endpointsClient.Endpoints(operatorclient.TargetNamespace).Create(ctx, required, metav1.CreateOptions{})
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
	if !endpointsSubsetsEqual(existing.Subsets, required.Subsets) {
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
	updated, err := c.endpointsClient.Endpoints(operatorclient.TargetNamespace).Update(ctx, toWrite, metav1.UpdateOptions{})
	if err != nil {
		c.eventRecorder.Warningf("EndpointsUpdateFailed", "Failed to update endpoints/%s -n %s: %v", required.Name, required.Namespace, err)
		return err
	}
	klog.Infof("toWrite: \n%v", mergepatch.ToYAMLOrError(updated.Subsets))
	c.eventRecorder.Warningf("EndpointsUpdated", "Updated endpoints/%s -n %s because it changed: %v", required.Name, required.Namespace, jsonPatch)
	return nil
}

func endpointsSubsetsEqual(lhs, rhs []corev1.EndpointSubset) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	for i := range lhs {
		if !endpointSubsetEqual(lhs[i], rhs[i]) {
			return false
		}
	}
	return true
}

func endpointSubsetEqual(lhs, rhs corev1.EndpointSubset) bool {
	if len(lhs.Addresses) != len(rhs.Addresses) {
		return false
	}
	if len(lhs.NotReadyAddresses) != len(rhs.NotReadyAddresses) {
		return false
	}
	if len(lhs.Ports) != len(rhs.Ports) {
		return false
	}
	// sorts the endpoint addresses for comparison (make copy as to not clobber originals)
	count := len(lhs.Addresses)
	lhsAddresses := make([]corev1.EndpointAddress, count)
	rhsAddresses := make([]corev1.EndpointAddress, count)
	for i := 0; i < count; i++ {
		lhs.Addresses[i].DeepCopyInto(&lhsAddresses[i])
		rhs.Addresses[i].DeepCopyInto(&rhsAddresses[i])
	}
	sort.Slice(lhsAddresses, newEndpointAddressSliceComparator(lhsAddresses))
	sort.Slice(rhsAddresses, newEndpointAddressSliceComparator(rhsAddresses))
	return reflect.DeepEqual(lhsAddresses, rhsAddresses)
}

func newEndpointAddressSliceComparator(endpointAddresses []corev1.EndpointAddress) func(int, int) bool {
	return func(i, j int) bool {
		switch {
		case endpointAddresses[i].IP != endpointAddresses[j].IP:
			return endpointAddresses[i].IP < endpointAddresses[j].IP
		case endpointAddresses[i].Hostname != endpointAddresses[j].Hostname:
			return endpointAddresses[i].Hostname < endpointAddresses[j].Hostname
		default:
			return *endpointAddresses[i].NodeName < *endpointAddresses[j].NodeName
		}
	}
}

func (c *HostEndpoints2Controller) Run(ctx context.Context, workers int) {
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

func (c *HostEndpoints2Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *HostEndpoints2Controller) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync(context.TODO())
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

func (c *HostEndpoints2Controller) eventHandler() cache.ResourceEventHandler {
	// eventHandler queues the operator to check spec and status
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
