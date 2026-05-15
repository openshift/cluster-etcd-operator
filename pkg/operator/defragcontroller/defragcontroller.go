package defragcontroller

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	minDefragBytes                 int64   = 100 * 1024 * 1024 // 100MB
	minDefragWaitDuration                  = 36 * time.Second
	defaultDefragCooldown                  = 1 * time.Hour
	maxFragmentedPercentage        float64 = 45
	pollWaitDuration                       = 2 * time.Second
	pollTimeoutDuration                    = 60 * time.Second
	compactionInterval                     = 10 * time.Minute
	maxDefragFailuresBeforeDegrade         = 3

	defragDisabledCondition    = "DefragControllerDisabled"
	defragDisableConfigmapName = "etcd-disable-defrag"

	defragDegradedCondition = "DefragControllerDegraded"
)

// DefragController observes the etcd state file fragmentation via Status method of Maintenance API. Based on these
// observations the controller will perform rolling defragmentation of each etcd member in the cluster.
type DefragController struct {
	operatorClient       v1helpers.OperatorClient
	memberLister         etcdcli.AllMemberLister
	defragClient         etcdcli.Defragment
	statusClient         etcdcli.Status
	infrastructureLister configv1listers.InfrastructureLister
	configmapLister      corev1listers.ConfigMapLister

	numDefragFailures  int
	defragWaitDuration time.Duration
	defragCooldown     time.Duration
	lastDefragTime     time.Time
}

func NewDefragController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	memberLister etcdcli.AllMemberLister,
	defragClient etcdcli.Defragment,
	statusClient etcdcli.Status,
	infrastructureLister configv1listers.InfrastructureLister,
	eventRecorder events.Recorder,
	kubeInformers v1helpers.KubeInformersForNamespaces) factory.Controller {
	c := &DefragController{
		operatorClient:       operatorClient,
		memberLister:         memberLister,
		defragClient:         defragClient,
		statusClient:         statusClient,
		infrastructureLister: infrastructureLister,
		configmapLister:      kubeInformers.ConfigMapLister(),
		defragWaitDuration:   minDefragWaitDuration,
		defragCooldown:       defaultDefragCooldown,
	}
	syncer := health.NewCheckingSyncWrapper(c.sync, 3*compactionInterval+1*time.Minute)
	livenessChecker.Add("DefragController", syncer)

	return factory.New().ResyncEvery(compactionInterval+1*time.Minute).WithInformers( // attempt to sync outside of etcd compaction interval to ensure maximum gain by defragmentation.
		operatorClient.Informer(),
	).WithSync(syncer.Sync).ToController("DefragController", eventRecorder.WithComponentSuffix("defrag-controller"))
}

func (c *DefragController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	enabled, err := c.checkDefragEnabled(ctx, syncCtx.Recorder())
	if err != nil {
		return err
	}

	if !enabled {
		return nil
	}

	if !c.lastDefragTime.IsZero() && time.Since(c.lastDefragTime) < c.defragCooldown {
		klog.V(4).Infof("Defrag cooldown active, %v remaining", c.defragCooldown-time.Since(c.lastDefragTime))
		return nil
	}

	return c.runDefrag(ctx, syncCtx.Recorder())
}

func (c *DefragController) checkDefragEnabled(ctx context.Context, recorder events.Recorder) (bool, error) {
	disableConfigMap, err := c.configmapLister.ConfigMaps(operatorclient.OperatorNamespace).Get(defragDisableConfigmapName)
	if err != nil && !k8serror.IsNotFound(err) {
		return false, fmt.Errorf("failed to retrieve configmap %s/%s: %w", operatorclient.OperatorNamespace, defragDisableConfigmapName, err)
	}

	if disableConfigMap != nil {
		klog.V(4).Infof("Defrag controller disabled manually via configmap: %s/%s", operatorclient.OperatorNamespace, defragDisableConfigmapName)
		return false, c.ensureControllerDisabledCondition(ctx, operatorv1.ConditionTrue, recorder)
	}

	controlPlaneTopology, err := ceohelpers.GetControlPlaneTopology(c.infrastructureLister)
	if err != nil {
		return false, fmt.Errorf("failed to get control-plane topology: %w", err)
	}

	// Ensure defrag disabled unless HA.
	if !(controlPlaneTopology == configv1.HighlyAvailableTopologyMode ||
		controlPlaneTopology == configv1.HighlyAvailableArbiterMode ||
		controlPlaneTopology == configv1.DualReplicaTopologyMode) {
		klog.V(4).Infof("Defrag controller disabled for incompatible cluster topology: %s", controlPlaneTopology)
		return false, c.ensureControllerDisabledCondition(ctx, operatorv1.ConditionTrue, recorder)
	}

	if err := c.ensureControllerDisabledCondition(ctx, operatorv1.ConditionFalse, recorder); err != nil {
		return false, fmt.Errorf("failed to ensure enabled controller condition: %w", err)
	}

	return true, nil
}

func (c *DefragController) runDefrag(ctx context.Context, recorder events.Recorder) error {
	// Do not defrag if any of the cluster members are unhealthy.
	memberHealth, err := c.memberLister.MemberHealth(ctx)
	if err != nil {
		return err
	}
	if !etcdcli.IsClusterHealthy(memberHealth) {
		return fmt.Errorf("cluster is unhealthy: %s", memberHealth.Status())
	}

	members, err := c.memberLister.MemberList(ctx)
	if err != nil {
		return err
	}

	var (
		etcdMembers    []*etcdserverpb.Member
		endpointStatus []*clientv3.StatusResponse
		leader         *clientv3.StatusResponse
	)
	for _, member := range members {
		// filter out learner members since they don't support the defragment API call
		// and filter out unstarted members
		if member.IsLearner || len(member.ClientURLs) == 0 {
			continue
		}

		status, err := c.statusClient.Status(ctx, member.ClientURLs[0])
		if err != nil {
			return err
		}

		etcdMembers = append(etcdMembers, member)

		if leader == nil && status.Leader == member.ID {
			leader = status
			continue
		}
		endpointStatus = append(endpointStatus, status)
	}

	// Sort non-leader members by fragmentation percentage descending so we
	// defrag the most fragmented member first each cycle.
	slices.SortFunc(endpointStatus, func(a, b *clientv3.StatusResponse) int {
		return cmp.Compare(
			checkFragmentationPercentage(b.DbSize, b.DbSizeInUse),
			checkFragmentationPercentage(a.DbSize, a.DbSizeInUse),
		)
	})

	// Leader last if possible.
	if leader != nil {
		klog.V(4).Infof("Appending leader last, ID: %x", leader.Header.MemberId)
		endpointStatus = append(endpointStatus, leader)
	}

	for _, status := range endpointStatus {
		member, err := getMemberFromStatus(etcdMembers, status)
		if err != nil {
			c.numDefragFailures++
			if c.numDefragFailures >= maxDefragFailuresBeforeDegrade {
				c.setDegraded(ctx, recorder)
			}
			return err
		}

		if !isEndpointBackendFragmented(member, status) {
			continue
		}

		recorder.Eventf("DefragControllerDefragmentAttempt", "Attempting defrag on member: %s, memberID: %x, dbSize: %d, dbInUse: %d, leader ID: %d", member.Name, member.ID, status.DbSize, status.DbSizeInUse, status.Leader)
		if _, err := c.defragClient.Defragment(ctx, member); err != nil {
			// Defrag can timeout if defragmentation takes longer than etcdcli.DefragDialTimeout.
			errMsg := fmt.Sprintf("failed defrag on member: %s, memberID: %x: %v", member.Name, member.ID, err)
			recorder.Eventf("DefragControllerDefragmentFailed", errMsg)
			c.numDefragFailures++
			if c.numDefragFailures >= maxDefragFailuresBeforeDegrade {
				c.setDegraded(ctx, recorder)
			}
			return fmt.Errorf("%s", errMsg)
		}

		if postStatus, err := c.statusClient.Status(ctx, member.ClientURLs[0]); err != nil {
			klog.Warningf("post-defrag status check failed for member %s: %v", member.Name, err)
		} else {
			klog.Infof("Defrag complete for member %s: dbSize %d -> %d, dbInUse %d -> %d",
				member.Name, status.DbSize, postStatus.DbSize, status.DbSizeInUse, postStatus.DbSizeInUse)
		}

		recorder.Eventf("DefragControllerDefragmentSuccess", "etcd member has been defragmented: %s, memberID: %d", member.Name, member.ID)

		// Give cluster time to recover before the next sync defrags another member.
		if err := wait.PollUntilContextTimeout(ctx, pollWaitDuration, pollTimeoutDuration, false,
			func(ctx context.Context) (bool, error) {
				// Ensure defragmentation attempts have clear observable signal.
				klog.V(4).Infof("Waiting for cluster to recover after defrag of member %s: %v", member.Name, c.defragWaitDuration)
				time.Sleep(c.defragWaitDuration)

				memberHealth, err := c.memberLister.MemberHealth(ctx)
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
			klog.Warningf("timeout waiting for cluster to stabilize after defrag of member %s: %v", member.Name, err)
		}

		c.numDefragFailures = 0
		c.lastDefragTime = time.Now()
		c.clearDegraded(ctx, recorder)
		return nil
	}

	c.numDefragFailures = 0
	c.clearDegraded(ctx, recorder)
	return nil
}

func (c *DefragController) setDegraded(ctx context.Context, recorder events.Recorder) {
	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    defragDegradedCondition,
		Status:  operatorv1.ConditionTrue,
		Reason:  "Error",
		Message: fmt.Sprintf("degraded after %d attempts at defragmenting etcd members", c.numDefragFailures),
	}))
	if updateErr != nil {
		recorder.Warning("DefragControllerUpdatingStatus", updateErr.Error())
	}
}

func (c *DefragController) clearDegraded(ctx context.Context, recorder events.Recorder) {
	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   defragDegradedCondition,
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	if updateErr != nil {
		recorder.Warning("DefragControllerUpdatingStatus", updateErr.Error())
	}
}

func (c *DefragController) ensureControllerDisabledCondition(ctx context.Context, desiredStatus operatorv1.ConditionStatus, recorder events.Recorder) error {
	_, currentState, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	controllerDisabledCondition := v1helpers.FindOperatorCondition(currentState.Conditions, defragDisabledCondition)
	if controllerDisabledCondition == nil || controllerDisabledCondition.Status != desiredStatus {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
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
