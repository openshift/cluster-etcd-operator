package etcdmemberscontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/etcd/etcdserver/etcdserverpb"

	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog"

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
	klog.Info("EtcdMembersControllerSync")
	err := c.reportEtcdMembers(syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

func (c *EtcdMembersController) reportEtcdMembers(recorder events.Recorder) error {
	etcdMembers, err := c.etcdClient.MemberList()
	if err != nil {
		return err
	}
	etcdMemberHealthCheck, err := c.etcdClient.MemberHealth(etcdMembers)
	if err != nil {
		return err
	}
	availableMembers, unhealthyMembers, unstartedMembers := etcdMemberHealthCheck.Status()

	updateErrors := []error{}
	statusMessage := getMemberMessage(availableMembers, unhealthyMembers, unstartedMembers, etcdMembers)
	if len(unhealthyMembers) > 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "UnhealthyMembers",
			Message: statusMessage,
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
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
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(unstartedMembers) > 0 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "MembersNotStarted",
			Message: statusMessage,
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
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
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
			updateErrors = append(updateErrors, updateErr)
		}
	}

	if len(availableMembers) > len(etcdMembers)/2 {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMembersAvailable",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdQuorate",
			Message: statusMessage,
		}))
		if updateErr != nil {
			recorder.Warning("EtcdMembersErrorUpdatingStatus", updateErr.Error())
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
			Message: statusMessage,
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
