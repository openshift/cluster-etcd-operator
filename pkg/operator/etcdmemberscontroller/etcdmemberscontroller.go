package etcdmemberscontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
)

const workQueueKey = "key"

// EtcdMembersController reports the status conditions
// of etcd members.
type EtcdMembersController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient

	eventRecorder events.Recorder
	queue         workqueue.RateLimitingInterface
	cachesToSync  []cache.InformerSynced
}

func NewEtcdMembersController(operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
) *EtcdMembersController {
	nodeInformer := kubeInformers.InformersFor("").Core().V1().Nodes()
	endpointsInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints()
	podInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods()
	c := &EtcdMembersController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			endpointsInformer.Informer().HasSynced,
			nodeInformer.Informer().HasSynced,
			podInformer.Informer().HasSynced,
		},
		eventRecorder: eventRecorder.WithComponentSuffix("member-observer-controller"),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EtcdMembersController"),
	}
	operatorClient.Informer().AddEventHandler(c.eventHandler())
	nodeInformer.Informer().AddEventHandler(c.eventHandler())
	podInformer.Informer().AddEventHandler(c.eventHandler())
	endpointsInformer.Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *EtcdMembersController) sync() error {
	err := c.reportEtcdMembers()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingReportEtcdMembers",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ReportEtcdMembersErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "EtcdMembersDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "MembersReported",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("ReportEtcdMembersErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *EtcdMembersController) reportEtcdMembers() error {
	etcdMembers, err := c.etcdClient.MemberList()
	if err != nil {
		return err
	}

	healthyEtcdMembers, notStartedEtcdMembers, unhealthyMembers, unknownEtcdMembers := []string{}, []string{}, []string{}, []string{}

	for _, m := range etcdMembers {
		switch c.etcdClient.MemberStatus(m) {
		case etcdcli.EtcdMemberStatusAvailable:
			healthyEtcdMembers = append(healthyEtcdMembers, m.Name)
		case etcdcli.EtcdMemberStatusNotStarted:
			notStartedEtcdMembers = append(notStartedEtcdMembers, m.Name)
		case etcdcli.EtcdMemberStatusUnhealthy:
			unhealthyMembers = append(unhealthyMembers, m.Name)
		case etcdcli.EtcdMemberStatusUnknown:
			unknownEtcdMembers = append(unknownEtcdMembers, m.Name)
		}
	}

	if len(unhealthyMembers) != 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "UnhealthyMembers",
			Message: fmt.Sprintf("%s members are unhealthy", strings.Join(unhealthyMembers, ",")),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			return err
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "No unhealthy members found",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			return err
		}
	}

	if len(notStartedEtcdMembers) != 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "MembersNotStarted",
			Message: fmt.Sprintf("%s members have not started yet", strings.Join(notStartedEtcdMembers, ",")),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			return err
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "no unstarted members found",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			return err
		}
	}

	if len(healthyEtcdMembers) > len(etcdMembers)/2 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionTrue,
			Reason:  "MembersNotStarted",
			Message: fmt.Sprintf("%s members are available, %s have not started, %s are unhealthy", strings.Join(healthyEtcdMembers, ","), strings.Join(notStartedEtcdMembers, ","), strings.Join(unhealthyMembers, ",")),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			return err
		}
	} else {
		// we will never reach here, if no quorum, we will always timeout
		// in the member list call and go to degraded with
		// etcdserver: request timed out
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionFalse,
			Reason:  "No quorum",
			Message: fmt.Sprintf("%s members are available, %s have not started, %s are unhealthy", strings.Join(healthyEtcdMembers, ","), strings.Join(notStartedEtcdMembers, ","), strings.Join(unhealthyMembers, ",")),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			return err
		}
	}
	return nil
}

func (c *EtcdMembersController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	klog.Infof("Starting EtcdMembersController")
	defer klog.Infof("Shutting down EtcdMembersController")
	if !cache.WaitForCacheSync(ctx.Done(), c.cachesToSync...) {
		return
	}
	go wait.Until(c.runWorker, time.Second, ctx.Done())
	<-ctx.Done()
}

func (c *EtcdMembersController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *EtcdMembersController) processNextWorkItem() bool {
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

func (c *EtcdMembersController) eventHandler() cache.ResourceEventHandler {
	// eventHandler queues the operator to check spec and status
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
