package defragcontroller

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
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
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	minDefragBytes                 int64   = 100 * 1024 * 1024 // 100MB
	maxFragmentedPercentage        float64 = 45
	compactionInterval                     = 10 * time.Minute
	maxDefragFailuresBeforeDegrade         = 3

	// defaultDefragSettleTime is the minimum time to wait after a leader transfer
	// to allow the cluster to stabilize before proceeding with defrag.
	defaultDefragSettleTime = 10 * time.Second

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
	leaderMover          etcdcli.LeaderMover
	infrastructureLister configv1listers.InfrastructureLister
	configmapLister      corev1listers.ConfigMapLister

	// defragSettleTime is the minimum time to wait after a leader transfer or defrag
	// to allow the cluster to stabilize before proceeding.
	defragSettleTime time.Duration

	numDefragFailures int
	// defragTargets tracks the ids of members that need to be defragged during the current cycle
	defragTargets []uint64
}

func NewDefragController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	memberLister etcdcli.AllMemberLister,
	defragClient etcdcli.Defragment,
	statusClient etcdcli.Status,
	leaderMover etcdcli.LeaderMover,
	infrastructureLister configv1listers.InfrastructureLister,
	eventRecorder events.Recorder,
	kubeInformers v1helpers.KubeInformersForNamespaces) factory.Controller {
	c := &DefragController{
		operatorClient:       operatorClient,
		memberLister:         memberLister,
		defragClient:         defragClient,
		statusClient:         statusClient,
		leaderMover:          leaderMover,
		infrastructureLister: infrastructureLister,
		configmapLister:      kubeInformers.ConfigMapLister(),
		defragSettleTime:     defaultDefragSettleTime,
	}
	syncer := health.NewCheckingSyncWrapper(c.sync, 3*compactionInterval+1*time.Minute)
	livenessChecker.Add("DefragController", syncer)

	return factory.New().ResyncEvery(compactionInterval+1*time.Minute).WithBareInformers( // attempt to sync outside of etcd compaction interval to ensure maximum gain by defragmentation.
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

	return c.runDefrag(ctx, syncCtx)
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

type StatusMember struct {
	Status *clientv3.StatusResponse
	Member *etcdserverpb.Member
}

func (sm StatusMember) IsLeader() bool {
	return sm.Status.Leader == sm.Member.ID
}

func (c *DefragController) runDefrag(ctx context.Context, syncCtx factory.SyncContext) error {
	recorder := syncCtx.Recorder()
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
		isNewCycle    = len(c.defragTargets) == 0
		statusMembers = make(map[uint64]StatusMember)
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
		} else if status == nil {
			return fmt.Errorf("endpoint status returned nil for member %q (%s)", member.Name, member.ClientURLs[0])
		}

		sm := StatusMember{
			Status: status,
			Member: member,
		}

		statusMembers[member.ID] = sm

		if isNewCycle && isEndpointBackendFragmented(member, status) {
			c.defragTargets = append(c.defragTargets, member.ID)
		}
	}

	if !isNewCycle {
		c.defragTargets = slices.DeleteFunc(c.defragTargets, func(targetID uint64) bool {
			statusMember, has := statusMembers[targetID]
			if !has {
				// Remove any defrag targets that we don't have a status for.
				return true
			}

			// Remove any members that no longer meet the conditions for defrag.
			return !isEndpointBackendFragmented(statusMember.Member, statusMember.Status)
		})
	}

	if len(c.defragTargets) == 0 {
		targets := make([]string, len(c.defragTargets))
		for i, targetID := range c.defragTargets {
			target := statusMembers[targetID]
			percent := checkFragmentationPercentage(target.Status.DbSize, target.Status.DbSizeInUse)
			targets[i] = fmt.Sprintf("%s={id: %d, fragmentation: %.2f%%, sizeInUse: %d}", target.Member.Name, target.Member.ID, percent, target.Status.DbSize)
		}
		klog.V(4).Infof("Defrag skipped: no etcd members meet the conditions for defragmentation:\n%s", strings.Join(targets, ", "))
		return nil
	}

	// Sort fragmented members so we defragment the most fragmented member first while defragging the leader last
	slices.SortFunc(c.defragTargets, func(a, b uint64) int {
		aIsLeader, bIsLeader := statusMembers[a].IsLeader(), statusMembers[b].IsLeader()
		if aIsLeader && !bIsLeader {
			return 1
		} else if !aIsLeader && bIsLeader {
			return -1
		}
		return sortByMostFragmented(statusMembers[a], statusMembers[b])
	})

	defragTarget := statusMembers[c.defragTargets[0]]
	defragTargetStatus, defragTargetMember := defragTarget.Status, defragTarget.Member

	// Preemptively attempt to move the leadership away from the current defrag target to another valid follower.
	// We try this to avoid multiple leader elections in the case where defragging the leader causes leadership
	// to move to a member we've yet to defrag, which could in turn lose leadership, etc. causing a lot of churn.
	// We record any error that occurs while attempting this, but we do not halt defrag if the move fails.
	if defragTarget.IsLeader() && len(statusMembers) > 1 {
		followers := make([]StatusMember, 0, len(statusMembers))
		for id, member := range statusMembers {
			if defragTargetMember.ID == id {
				continue
			}
			followers = append(followers, member)
		}

		slices.SortFunc(followers, sortByLeastFragmented)

		for _, newLeader := range followers {
			err := c.leaderMover.MoveLeader(ctx, defragTargetMember, newLeader.Member.ID)
			if err != nil {
				recorder.Warningf("DefragControllerLeaderTransferAttemptFailed", "Failed to move leader away from member %s to member %s before defrag: %v", defragTargetMember.Name, newLeader.Member.Name, err)
				continue
			}

			recorder.Eventf("DefragControllerLeaderTransferSuccess", "Moved leadership away from member %s (memberID: %x) to member %s (memberID: %x) before defrag, requeueing to allow etcd to settle", defragTargetMember.Name, defragTargetMember.ID, newLeader.Member.Name, newLeader.Member.ID)
			syncCtx.Queue().AddAfter(syncCtx.QueueKey(), c.defragSettleTime)
			return nil
		}
		recorder.Warningf("DefragControllerLeaderTransferFailed", "Failed to move leader away from member %s, continuing with blocking leader defrag", defragTargetMember.Name)
	}

	recorder.Eventf("DefragControllerDefragmentAttempt", "Attempting defrag on member: %s, memberID: %x, dbSize: %d, dbInUse: %d, leader ID: %d", defragTargetMember.Name, defragTargetMember.ID, defragTargetStatus.DbSize, defragTargetStatus.DbSizeInUse, defragTargetStatus.Leader)
	if _, err := c.defragClient.Defragment(ctx, defragTargetMember); err != nil {
		errMsg := fmt.Sprintf("failed defrag on member: %s, memberID: %x: %v", defragTargetMember.Name, defragTargetMember.ID, err)
		recorder.Warningf("DefragControllerDefragmentFailed", errMsg)
		klog.Errorf("%s", errMsg)
		c.numDefragFailures++
		if c.numDefragFailures >= maxDefragFailuresBeforeDegrade {
			c.setDegraded(ctx, recorder)
		}
		syncCtx.Queue().AddAfter(syncCtx.QueueKey(), c.defragSettleTime)
		return nil
	}

	recorder.Eventf("DefragControllerDefragmentSuccess", "etcd member has been defragmented: %s, memberID: %d", defragTargetMember.Name, defragTargetMember.ID)
	c.numDefragFailures = 0
	c.clearDegraded(ctx, recorder)

	c.defragTargets = c.defragTargets[1:]
	syncCtx.Queue().AddAfter(syncCtx.QueueKey(), c.defragSettleTime)
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

func sortByLeastFragmented(a, b StatusMember) int {
	return cmp.Compare(
		checkFragmentationPercentage(a.Status.DbSize, a.Status.DbSizeInUse),
		checkFragmentationPercentage(b.Status.DbSize, b.Status.DbSizeInUse),
	)
}

func sortByMostFragmented(a, b StatusMember) int {
	return cmp.Compare(
		checkFragmentationPercentage(b.Status.DbSize, b.Status.DbSizeInUse),
		checkFragmentationPercentage(a.Status.DbSize, a.Status.DbSizeInUse),
	)
}
