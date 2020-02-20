package bootstrapteardown

import (
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey = "key"
)

type BootstrapTeardownController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
	kubeClient     kubernetes.Interface

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewBootstrapTeardownController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,

	etcdClient etcdcli.EtcdClient,

	eventRecorder events.Recorder,
) *BootstrapTeardownController {
	c := &BootstrapTeardownController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
		kubeClient:     kubeClient,

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "BootstrapTeardownController"),
		eventRecorder: eventRecorder.WithComponentSuffix("bootstrap-teardown-controller"),
	}

	operatorClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *BootstrapTeardownController) sync() error {
	err := c.removeBootstrap()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "BootstrapTeardownDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *BootstrapTeardownController) removeBootstrap() error {
	// checks the actual etcd cluster membership API if etcd-bootstrap exists
	safeToRemoveBootstrap, hasBootstrap, err := c.canRemoveEtcdBootstrap()
	switch {
	case err != nil:
		return err

	case !hasBootstrap:
		// if the bootstrap isn't present, then clearly we're available enough to terminate. This avoids any risk of flapping.
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdRunningInCluster",
			Status:  operatorv1.ConditionTrue,
			Reason:  "BootstrapAlreadyRemoved",
			Message: "etcd-bootstrap member is already removed",
		}))
		if updateErr != nil {
			return updateErr
		}
		return nil

	case !safeToRemoveBootstrap:
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdRunningInCluster",
			Status:  operatorv1.ConditionFalse,
			Reason:  "NotEnoughEtcdMembers",
			Message: "still waiting for three healthy etcd members",
		}))
		if updateErr != nil {
			return updateErr
		}
		return nil

	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    "EtcdRunningInCluster",
		Status:  operatorv1.ConditionTrue,
		Reason:  "EnoughEtcdMembers",
		Message: "enough members found",
	}))
	if updateErr != nil {
		return updateErr
	}

	// check to see if bootstrapping is complete
	bootstrapFinishedConfigMap, err := c.kubeClient.CoreV1().ConfigMaps("kube-system").Get("bootstrap", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		c.eventRecorder.Event("DelayingBootstrapTeardown", "cluster-bootstrap is not yet finished")
		return nil
	case err != nil:
		return err

	case bootstrapFinishedConfigMap.Data["status"] != "complete":
		c.eventRecorder.Event("DelayingBootstrapTeardown", "cluster-bootstrap is not yet finished")
		return nil
	}

	c.eventRecorder.Event("RemoveBootstrapEtcd", "removing etcd-bootstrap member")
	// this is ugly until bootkube is updated, but we want to be sure that bootkube has time to be waiting to watch the condition coming back.
	if err := c.etcdClient.MemberRemove("etcd-bootstrap"); err != nil {
		return err
	}
	return nil
}

// canRemoveEtcdBootstrap returns whether it is safe to remove bootstrap, whether bootstrap is in the list, and an error
func (c *BootstrapTeardownController) canRemoveEtcdBootstrap() (bool, bool, error) {
	members, err := c.etcdClient.MemberList()
	if err != nil {
		return false, false, err
	}

	hasBootstrap := false
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			hasBootstrap = true
		}
	}
	if !hasBootstrap {
		return false, hasBootstrap, nil
	}

	if len(members) < 4 {
		return false, hasBootstrap, nil
	}

	unhealthyMembers, err := c.etcdClient.UnhealthyMembers()
	if err != nil {
		return false, hasBootstrap, nil
	}

	// the etcd-bootstrap member is allowed to be unhealthy and can still be removed
	switch {
	case len(unhealthyMembers) == 0:
		return true, hasBootstrap, nil
	case len(unhealthyMembers) > 1:
		return false, hasBootstrap, nil
	default:
		if unhealthyMembers[0].Name == "etcd-bootstrap" {
			return true, true, nil
		}
		return false, hasBootstrap, nil
	}
}

func (c *BootstrapTeardownController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting BootstrapTeardownController")
	defer klog.Infof("Shutting down BootstrapTeardownController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		return
	}
	klog.V(2).Infof("caches synced BootstrapTeardownController")

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
