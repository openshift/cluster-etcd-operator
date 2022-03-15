package defragcontroller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	k8serror "k8s.io/apimachinery/pkg/api/errors"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
)

const (
	minDefragBytes          int64   = 100 * 1024 * 1024 // 100MB
	minDefragWaitDuration           = 36 * time.Second
	maxFragmentedPercentage float64 = 45
	pollWaitDuration                = 2 * time.Second
	pollTimeoutDuration             = 60 * time.Second
	compactionInterval              = 10 * time.Minute

	defragDisabledCondition    = "DefragControllerDisabled"
	defragDisableConfigmapName = "etcd-disable-defrag"
)

// DefragController observes the operand state file for fragmentation
type DefragController struct {
	operatorClient       v1helpers.OperatorClient
	etcdClient           etcdcli.EtcdClient
	infrastructureLister configv1listers.InfrastructureLister
	configmapLister      corev1listers.ConfigMapLister

	defragWaitDuration time.Duration
}

func NewDefragController(
	operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	infrastructureLister configv1listers.InfrastructureLister,
	eventRecorder events.Recorder,
	kubeInformers v1helpers.KubeInformersForNamespaces) factory.Controller {
	c := &DefragController{
		operatorClient:       operatorClient,
		etcdClient:           etcdClient,
		infrastructureLister: infrastructureLister,
		configmapLister:      kubeInformers.ConfigMapLister(),
		defragWaitDuration:   minDefragWaitDuration,
	}
	return factory.New().ResyncEvery(compactionInterval+1*time.Minute).WithInformers( // attempt to sync outside of etcd compaction interval to ensure maximum gain by defragmentation.
		operatorClient.Informer(),
	).WithSync(c.sync).ToController("DefragController", eventRecorder.WithComponentSuffix("defrag-controller"))
}

func (c *DefragController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.checkDefrag(ctx, syncCtx.Recorder())
	if err != nil && !errors.Is(err, context.Canceled) {
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
	disableConfigMap, err := c.configmapLister.ConfigMaps(operatorclient.OperatorNamespace).Get(defragDisableConfigmapName)
	if err != nil && !k8serror.IsNotFound(err) {
		return fmt.Errorf("failed to retrieve configmap %s/%s: %w", operatorclient.OperatorNamespace, defragDisableConfigmapName, err)
	}

	if disableConfigMap != nil {
		klog.V(4).Infof("Defrag controller disabled manually via configmap: %s/%s", operatorclient.OperatorNamespace, defragDisableConfigmapName)
		return c.ensureControllerDisabledCondition(operatorv1.ConditionTrue, recorder)
	}

	controlPlaneTopology, err := ceohelpers.GetControlPlaneTopology(c.infrastructureLister)
	if err != nil {
		return fmt.Errorf("failed to get control-plane topology: %w", err)
	}

	// Ensure defrag disabled unless HA.
	if controlPlaneTopology != configv1.HighlyAvailableTopologyMode {
		klog.V(4).Infof("Defrag controller disabled for non HA cluster topology: %s", controlPlaneTopology)
		return c.ensureControllerDisabledCondition(operatorv1.ConditionTrue, recorder)
	}

	if err := c.ensureControllerDisabledCondition(operatorv1.ConditionFalse, recorder); err != nil {
		return fmt.Errorf("failed to ensure enabled controller condition: %w", err)
	}

	// Do not defrag if any of the cluster members are unhealthy.
	memberHealth, err := c.etcdClient.MemberHealth(ctx)
	if err != nil {
		return err
	}
	if !etcdcli.IsClusterHealthy(memberHealth) {
		return fmt.Errorf("cluster is unhealthy: %s", memberHealth.Status())
	}

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return err
	}

	// filter out learner members since they don't support the defragment API call
	etcdMembers := []*etcdserverpb.Member{}
	for _, m := range members {
		if !m.IsLearner {
			etcdMembers = append(etcdMembers, m)
		}
	}

	var endpointStatus []*clientv3.StatusResponse
	var leader *clientv3.StatusResponse
	for _, member := range etcdMembers {
		if len(member.ClientURLs) == 0 {
			// skip unstarted member
			continue
		}
		status, err := c.etcdClient.Status(ctx, member.ClientURLs[0])
		if err != nil {
			return err
		}
		if leader == nil && status.Leader == member.ID {
			leader = status
			continue
		}
		endpointStatus = append(endpointStatus, status)
	}

	// Leader last if possible.
	if leader != nil {
		klog.V(4).Infof("Appending leader last, ID: %x", leader.Header.MemberId)
		endpointStatus = append(endpointStatus, leader)
	}

	var errors []error
	for _, status := range endpointStatus {
		member, err := getMemberFromStatus(etcdMembers, status)
		if err != nil {
			errors = append(errors, err)
			continue
		}

		// Check each member's status which includes the db size on disk "DbSize" and the db size in use "DbSizeInUse"
		// compare the % difference and if that difference is over the max diff threshold and also above the minimum
		// db size we defrag the members state file. In the case where this command only partially completed controller
		// can clean that up on the next sync. Having the db sizes slightly different is not a problem in itself.
		if isEndpointBackendFragmented(member, status) {
			recorder.Eventf("DefragControllerDefragmentAttempt", "Attempting defrag on member: %s, memberID: %x, dbSize: %d, dbInUse: %d, leader ID: %d", member.Name, member.ID, status.DbSize, status.DbSizeInUse, status.Leader)
			if _, err := c.etcdClient.Defragment(ctx, member); err != nil {
				// Defrag can timeout if defragmentation takes longer than etcdcli.DefragDialTimeout.
				errMsg := fmt.Sprintf("failed defrag on member: %s, memberID: %x: %v", member.Name, member.ID, err)
				recorder.Eventf("DefragControllerDefragmentFailed", errMsg)
				errors = append(errors, fmt.Errorf(errMsg))
				continue
			}

			recorder.Eventf("DefragControllerDefragmentSuccess", "etcd member has been defragmented: %s, memberID: %d", member.Name, member.ID)

			// Give cluster time to recover before we move to the next member.
			if err := wait.Poll(
				pollWaitDuration,
				pollTimeoutDuration,
				func() (bool, error) {
					// Ensure defragmentation attempts have clear observable signal.
					klog.V(4).Infof("Sleeping to allow cluster to recover before defrag next member: %v", c.defragWaitDuration)
					time.Sleep(c.defragWaitDuration)

					memberHealth, err := c.etcdClient.MemberHealth(ctx)
					if err != nil {
						klog.Warningf("failed checking member health: %v", err)
						return false, nil
					}
					if !etcdcli.IsClusterHealthy(memberHealth) {
						klog.Warningf("cluster is unhealthy: %s", memberHealth.Status())
						return false, nil
					}
					return true, nil
				}); err != nil {
				errors = append(errors, fmt.Errorf("timeout waiting for cluster to stabalize after defrag"))
			}
		}
	}

	return v1helpers.NewMultiLineAggregate(errors)
}

func (c *DefragController) ensureControllerDisabledCondition(desiredStatus operatorv1.ConditionStatus, recorder events.Recorder) error {
	_, currentState, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	controllerDisabledCondition := v1helpers.FindOperatorCondition(currentState.Conditions, defragDisabledCondition)
	if controllerDisabledCondition == nil || controllerDisabledCondition.Status != desiredStatus {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
			v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:   defragDisabledCondition,
				Status: desiredStatus,
				Reason: "AsExpected",
			}))
		if updateErr != nil {
			recorder.Warning("DefragControllerUpdatingStatus", updateErr.Error())
			return updateErr
		}
	}

	return nil
}

// isEndpointBackendFragmented checks the status of all cluster members to ensure that no members have a fragmented store.
// This can happen if the operator starts defrag of the cluster but then loses leader status and is rescheduled before
// the operator can defrag all members.
func isEndpointBackendFragmented(member *etcdserverpb.Member, endpointStatus *clientv3.StatusResponse) bool {
	if endpointStatus == nil {
		klog.Errorf("endpoint status validation failed: %v", endpointStatus)
		return false
	}
	fragmentedPercentage := checkFragmentationPercentage(endpointStatus.DbSize, endpointStatus.DbSizeInUse)
	if fragmentedPercentage > 0.00 {
		klog.Infof("etcd member %q backend store fragmented: %.2f %%, dbSize: %d", member.Name, fragmentedPercentage, endpointStatus.DbSize)
	}
	return fragmentedPercentage >= maxFragmentedPercentage && endpointStatus.DbSize >= minDefragBytes
}

func checkFragmentationPercentage(ondisk, inuse int64) float64 {
	diff := float64(ondisk - inuse)
	fragmentedPercentage := (diff / float64(ondisk)) * 100
	return math.Round(fragmentedPercentage*100) / 100
}

func getMemberFromStatus(members []*etcdserverpb.Member, endpointStatus *clientv3.StatusResponse) (*etcdserverpb.Member, error) {
	if endpointStatus == nil {
		return nil, fmt.Errorf("endpoint status validation failed: %v", endpointStatus)
	}
	for _, member := range members {
		if member.ID == endpointStatus.Header.MemberId {
			return member, nil
		}
	}
	return nil, fmt.Errorf("no member found in MemberList matching ID: %v", endpointStatus.Header.MemberId)
}
