package etcdmemberremovecontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	operatorv1 "github.com/openshift/api/operator/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corev1 "k8s.io/client-go/listers/core/v1"
)

const workQueueKey = "key"

// EtcdMemberRemoveController removes an etcd member
// if no node of same name found
type EtcdMemberRemoveController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
	nodeLister     corev1.NodeLister

	eventRecorder events.Recorder
	queue         workqueue.RateLimitingInterface
	cachesToSync  []cache.InformerSynced
}

func NewEtcdMemberRemoveController(operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
) *EtcdMemberRemoveController {
	nodeInformer := kubeInformers.InformersFor("").Core().V1().Nodes()
	c := &EtcdMemberRemoveController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
		nodeLister:     nodeInformer.Lister(),

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			nodeInformer.Informer().HasSynced,
		},

		eventRecorder: eventRecorder.WithComponentSuffix("member-remover-controller"),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EtcdMemberRemoveController"),
	}

	nodeInformer.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *EtcdMemberRemoveController) sync() error {
	err := c.removeMember()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMemberRemoverControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorRemovingEtcdMember",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("MemberRemoveErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "EtcdMemberRemoverControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "MembersAsExpected",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("MemberRemoveErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *EtcdMemberRemoveController) removeMember() error {
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return err
	}
	members, err := c.etcdClient.MemberList()
	if err != nil {
		return nil
	}

	memberToBeRemoved := ""
	for _, m := range members {
		if m.Name == "etcd-bootstrap" || m.Name == "" {
			// no node for this bootstrap or unstarted member
			continue
		}
		nodeFound := false
		for _, node := range nodes {
			if m.Name == node.Name {
				nodeFound = true
				break
			}
		}
		if !nodeFound {
			c.eventRecorder.Warningf("MemberToBeRemoved", "No node found with name %s", m.Name)
			memberToBeRemoved = m.Name
			break
		}
	}

	if memberToBeRemoved == "" {
		return nil
	}

	return c.etcdClient.MemberRemove(memberToBeRemoved)
}

func (c *EtcdMemberRemoveController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	klog.Infof("Starting EtcdMemberRemoveController")
	defer klog.Infof("Shutting down EtcdMemberRemoveController")

	if !cache.WaitForCacheSync(ctx.Done(), c.cachesToSync...) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, ctx.Done())

	<-ctx.Done()
}

func (c *EtcdMemberRemoveController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *EtcdMemberRemoveController) processNextWorkItem() bool {
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
func (c *EtcdMemberRemoveController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
