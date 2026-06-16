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

	numDefragFailures int
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

type StatusMember struct {
	Status *clientv3.StatusResponse
	Member *etcdserverpb.Member
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
		statusMembers = make([]StatusMember, 0, len(members))
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

		statusMembers = append(statusMembers, StatusMember{
			Status: status,
			Member: member,
		})
	}

	// Sort members by fragmentation percentage descending so we
	// defrag the most fragmented member first.
	slices.SortFunc(statusMembers, func(a, b StatusMember) int {
		return cmp.Compare(
			checkFragmentationPercentage(b.Status.DbSize, b.Status.DbSizeInUse),
			checkFragmentationPercentage(a.Status.DbSize, a.Status.DbSizeInUse),
		)
	})

	for _, statusMember := range statusMembers {
		status := statusMember.Status
		member := statusMember.Member

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

		recorder.Eventf("DefragControllerDefragmentSuccess", "etcd member has been defragmented: %s, memberID: %d", member.Name, member.ID)
		c.numDefragFailures = 0
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
