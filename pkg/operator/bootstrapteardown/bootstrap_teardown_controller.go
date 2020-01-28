package bootstrapteardown

import (
	"fmt"
	"time"

	v1 "github.com/openshift/client-go/operator/listers/operator/v1"

	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey = "key"
)

type BootstrapTeardownController struct {
	operatorConfigClient        v1helpers.OperatorClient
	clusterMemberShipController *clustermembercontroller.ClusterMemberController

	etcdOperatorLister v1.EtcdLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

// TODO wire a triggering lister
func NewBootstrapTeardownController(
	operatorConfigClient v1helpers.OperatorClient,
	clusterMemberShipController *clustermembercontroller.ClusterMemberController,

	operatorInformers operatorv1informers.SharedInformerFactory,

	eventRecorder events.Recorder,
) *BootstrapTeardownController {
	c := &BootstrapTeardownController{
		operatorConfigClient:        operatorConfigClient,
		clusterMemberShipController: clusterMemberShipController,

		etcdOperatorLister: operatorInformers.Operator().V1().Etcds().Lister(),

		cachesToSync:  []cache.InformerSynced{operatorConfigClient.Informer().HasSynced, operatorInformers.Operator().V1().Etcds().Informer().HasSynced},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "BootstrapTeardownController"),
		eventRecorder: eventRecorder.WithComponentSuffix("cluster-member-controller"),
	}

	operatorInformers.Operator().V1().Etcds().Informer().AddEventHandler(c.eventHandler())
	operatorConfigClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *BootstrapTeardownController) sync() error {
	err := c.removeBootstrap()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapTeardownDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("BootstrapTeardownErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "BootstrapTeardownDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}))
	return updateErr
}

func (c *BootstrapTeardownController) removeBootstrap() error {
	currentEtcdOperator, err := c.etcdOperatorLister.Get("cluster")
	if err != nil {
		return err
	}
	if currentEtcdOperator.Spec.ManagementState != operatorv1.Managed {
		return nil
	}

	etcdReady, err := isEtcdBootstrapped(currentEtcdOperator)
	if err != nil {
		return err
	}
	if !etcdReady {
		klog.Infof("Still waiting for the cluster-etcd-operator to bootstrap...")
		return nil
	}

	if !c.clusterMemberShipController.IsMember("etcd-bootstrap") {
		return nil
	}

	c.eventRecorder.Event("BootstrapTeardownController", "clusterversions is available, safe to remove bootstrap")
	if err := c.clusterMemberShipController.RemoveBootstrap(); err != nil {
		return err
	}
	return nil
}
func (c *BootstrapTeardownController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting BootstrapTeardownController")
	defer klog.Infof("Shutting down BootstrapTeardownController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	// add time based trigger
	go wait.PollImmediateUntil(time.Minute, func() (bool, error) {
		c.queue.Add(workQueueKey)
		return false, nil
	}, stopCh)

	<-stopCh
}

func (c *BootstrapTeardownController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *BootstrapTeardownController) processNextWorkItem() bool {
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

// eventHandler queues the operator to check spec and status
func (c *BootstrapTeardownController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
