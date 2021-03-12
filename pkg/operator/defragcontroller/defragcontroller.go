package defragcontroller

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

const (
	// TODO these need validated
	minDefragBytes      int64   = 1073741824 * 2 // 2GiB
	maxDefragPercentage float64 = 50.00          // 50%
)

// DefragController observes the operand state file for fragmentation
type DefragController struct {
	operatorClient v1helpers.OperatorClient
	etcdClient     etcdcli.EtcdClient
}

func NewDefragController(
	operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &DefragController{
		operatorClient: operatorClient,
		etcdClient:     etcdClient,
	}
	return factory.New().ResyncEvery(10*time.Minute).WithInformers(
		operatorClient.Informer(),
	).WithSync(c.sync).ToController("DefragController", eventRecorder.WithComponentSuffix("defrag-controller"))
}

func (c *DefragController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.checkDefrag(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "DefragControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("DefragControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "DefragControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *DefragController) checkDefrag(ctx context.Context, recorder events.Recorder) error {
	etcdMembers, err := c.etcdClient.MemberList()
	if err != nil {
		return err
	}
	// dont defrag if any of the cluster members are unhealthy
	memberHealth := etcdcli.GetMemberHealth(etcdMembers)
	if len(etcdcli.GetUnhealthyMemberNames(memberHealth)) > 0 {
		return fmt.Errorf("skipping defragmentation: %s ", memberHealth.Status())
	}

	// TODO sort by leader, with leader last
	errs := []error{}
	for _, member := range etcdMembers {
		// check status
		status, err := c.etcdClient.EndpointStatus(context.TODO(), member)
		if err != nil {
			return err
		}
		// Check each member's status which includes the db size on disk `DbSize` and the db size in use `DbSizeInUse`
		// compare the % difference and if that difference is over the max diff threshold and also above the minimum
		// db size we defrag the members state file. In the case where this command only partially completed controller
		// can clean that up on the next defrag. Having the db sizes slightly different is not a problem in itself.
		// TODO we can consider a condition where if the db sizes per member are not aligned we defrag to align.
		if getFragmentationPercentage(status.DbSize, status.DbSizeInUse) > maxDefragPercentage && status.DbSize > minDefragBytes {
			// defrag
			klog.Infof("starting defrag on member: %q dbSize: %d , dbInUse: %d", member.Name, status.DbSize, status.DbSizeInUse)
			_, err := etcdcli.DefragMember(member)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			recorder.Eventf("DefragControllerSuccess", "member: %q has been defragmented", member.Name)
			// give cluster time to recover before we move to the next.
			time.Sleep(5 * time.Second)
		}
	}
	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}
	return nil
}

func getFragmentationPercentage(ondisk, inuse int64) float64 {
	diff := float64(ondisk - inuse)
	return (diff / float64(inuse)) * 100
}
