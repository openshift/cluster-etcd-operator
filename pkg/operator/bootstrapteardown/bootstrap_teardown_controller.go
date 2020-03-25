package bootstrapteardown

import (
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey = "key"
)

type BootstrapTeardownController struct {
	operatorClient  v1helpers.StaticPodOperatorClient
	etcdClient      etcdcli.EtcdClient
	configmapLister corev1listers.ConfigMapLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewBootstrapTeardownController(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,

	etcdClient etcdcli.EtcdClient,

	eventRecorder events.Recorder,
) *BootstrapTeardownController {
	c := &BootstrapTeardownController{
		operatorClient:  operatorClient,
		etcdClient:      etcdClient,
		configmapLister: kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Lister(),

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "BootstrapTeardownController"),
		eventRecorder: eventRecorder.WithComponentSuffix("bootstrap-teardown-controller"),
	}

	operatorClient.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())

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
	bootstrapFinishedConfigMap, err := c.configmapLister.ConfigMaps("kube-system").Get("bootstrap")
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

	isUnsupportedUnsafeEtcd, err := c.isUnsupportedUnsafeEtcd()
	if err != nil {
		return false, hasBootstrap, err
	}

	switch {
	case !isUnsupportedUnsafeEtcd && len(members) < 4:
		// bootstrap is not safe to remove until we scale to 4
		return false, hasBootstrap, nil
	case isUnsupportedUnsafeEtcd && len(members) < 2:
		// if etcd is unsupported, bootstrap is not safe to remove
		// until we scale to 2
		return false, hasBootstrap, nil
	default:
		// do nothing fall through on checking the unhealthy members
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

func (c *BootstrapTeardownController) isUnsupportedUnsafeEtcd() (bool, error) {
	spec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return false, err
	}
	return ceohelpers.IsUnsupportedUnsafeEtcd(spec)
}
