package etcdmemberscontroller

import (
	"context"
	"k8s.io/klog/v2"
	"time"

	errorsutil "k8s.io/apimachinery/pkg/util/errors"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
)

// EtcdMembersController reports the status conditions
// of etcd members.
type EtcdMembersController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
}

func NewEtcdMembersController(operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &EtcdMembersController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
	}
	return factory.New().ResyncEvery(time.Minute).WithSync(c.sync).ToController("EtcdMembersController", eventRecorder.WithComponentSuffix("member-observer-controller"))
}

func (c *EtcdMembersController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.reportEtcdMembers(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingReportEtcdMembers",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("ReportEtcdMembersErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "EtcdMembersControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "MembersReported",
	}))
	if updateErr != nil {
		syncCtx.Recorder().Warning("ReportEtcdMembersErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *EtcdMembersController) reportEtcdMembers(ctx context.Context, recorder events.Recorder) error {
	memberHealth, err := c.etcdClient.MemberHealth(ctx)
	if err != nil {
		return err
	}
	var updateErrors []error
	unhealthy := etcdcli.GetUnhealthyMemberNames(memberHealth)
	if len(unhealthy) > 0 {
		for _, u := range memberHealth {
			if !u.Healthy {
				klog.Errorf("Unhealthy etcd member found: %s, took=%s, err=%v", u.Member.Name, u.Took, u.Error)
			}
		}
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "UnhealthyMembers",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "No unhealthy members found",
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(etcdcli.GetUnstartedMemberNames(memberHealth)) > 0 {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "MembersNotStarted",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "No unstarted etcd members found",
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(etcdcli.GetHealthyMemberNames(memberHealth)) > len(memberHealth)/2 {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdQuorate",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	} else {
		// we will never reach here, if no quorum, we will always timeout
		// in the member list call and go to degraded with
		// etcdserver: request timed out
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionFalse,
			Reason:  "NoQuorum",
			Message: memberHealth.Status(),
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(updateErrors) > 0 {
		return errorsutil.NewAggregate(updateErrors)
	}

	return nil
}
