package etcdmemberscontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const workQueueKey = "key"

// EtcdMembersController reports the status conditions
// of etcd members.
type EtcdMembersController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient

	eventRecorder events.Recorder
	queue         workqueue.RateLimitingInterface
}

func NewEtcdMembersController(operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) *EtcdMembersController {
	c := &EtcdMembersController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,

		eventRecorder: eventRecorder.WithComponentSuffix("member-observer-controller"),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EtcdMembersController"),
	}
	return c
}

func (c *EtcdMembersController) sync() error {
	err := c.reportEtcdMembers()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersControllerDegraded",
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
		Type:   "EtcdMembersControllerDegraded",
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
	memberHealth := etcdcli.GetMemberHealth(etcdMembers)
	updateErrors := []error{}
	if len(etcdcli.GetUnhealthyMemberNames(memberHealth)) > 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "UnhealthyMembers",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
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
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(etcdcli.GetUnstartedMemberNames(memberHealth)) > 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "MembersNotStarted",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "No unstarted etcd members found",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(etcdcli.GetHealthyMemberNames(memberHealth)) > len(etcdMembers)/2 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdQuorate",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	} else {
		// we will never reach here, if no quorum, we will always timeout
		// in the member list call and go to degraded with
		// etcdserver: request timed out
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionFalse,
			Reason:  "No quorum",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(updateErrors) > 0 {
		return errorsutil.NewAggregate(updateErrors)
	}

	return nil
}

func getMemberMessage(availableMembers, unhealthyMembers, unstartedMembers []string, allMembers []*etcdserverpb.Member) string {
	messages := []string{}
	if len(availableMembers) > 0 && len(availableMembers) == len(allMembers) {
		messages = append(messages, fmt.Sprintf("%d members are available", len(availableMembers)))
	}
	if len(availableMembers) > 0 && len(availableMembers) != len(allMembers) {
		messages = append(messages, fmt.Sprintf("%d of %d members are available", len(availableMembers), len(allMembers)))
	}
	if len(unhealthyMembers) > 0 {
		for _, name := range unhealthyMembers {
			messages = append(messages, fmt.Sprintf("%s is unhealthy", name))
		}
	}
	if len(unstartedMembers) > 0 {
		for _, name := range unstartedMembers {
			messages = append(messages, fmt.Sprintf("%s has not started", name))
		}
	}
	return strings.Join(messages, ", ")
}

func (c *EtcdMembersController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	klog.Infof("Starting EtcdMembersController")
	defer klog.Infof("Shutting down EtcdMembersController")

	go wait.Until(c.runWorker, time.Second, ctx.Done())

	// add time based trigger
	go wait.PollImmediateUntil(time.Minute, func() (bool, error) {
		c.queue.Add(workQueueKey)
		return false, nil
	}, ctx.Done())

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
