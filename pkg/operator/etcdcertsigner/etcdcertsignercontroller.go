package etcdcertsigner

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const workQueueKey = "key"

type EtcdCertSignerController struct {
	clientset corev1client.Interface
	// Not using this but still keeping it in there
	operatorConfigClient v1helpers.OperatorClient
	queue                workqueue.RateLimitingInterface
	eventRecorder        events.Recorder
}

func NewEtcdCertSignerController(
	clientset corev1client.Interface,
	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *EtcdCertSignerController {
	c := &EtcdCertSignerController{
		clientset:            clientset,
		operatorConfigClient: operatorConfigClient,
		queue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EtcdCertSignerController"),
		eventRecorder:        eventRecorder.WithComponentSuffix("etcd-cert-signer-controller"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *EtcdCertSignerController) Run(i int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController")
	defer klog.Infof("Shutting down ClusterMemberController")

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *EtcdCertSignerController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *EtcdCertSignerController) processNextWorkItem() bool {
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

func (c *EtcdCertSignerController) sync() error {
	// TODO: make the namespace and name constants in one of the packages
	cm, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting configmap %#v\n", err)
		return err
	}
	var members *clustermembercontroller.EtcdScaling
	membershipData, ok := cm.Annotations[clustermembercontroller.EtcdScalingAnnotationKey]
	if !ok {
		return errors.New("unable to find members data")
	}
	err = json.Unmarshal([]byte(membershipData), members)
	if err != nil {
		klog.Infof("unable to unmarshal members data %#v\n", err)
		return err
	}
	//TODO: Add the logic for generating certs
	klog.Infof("Found etcd configmap with data %#v\n", members)

	return nil
}

// eventHandler queues the operator to check spec and status
func (c *EtcdCertSignerController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
