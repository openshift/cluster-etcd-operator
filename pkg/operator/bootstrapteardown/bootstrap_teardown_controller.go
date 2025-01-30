package bootstrapteardown

import (
	"context"
	"fmt"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/bootstrap"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
)

type BootstrapTeardownController struct {
	operatorClient       v1helpers.StaticPodOperatorClient
	etcdClient           etcdcli.EtcdClient
	configmapLister      corev1listers.ConfigMapLister
	namespaceLister      corev1listers.NamespaceLister
	infrastructureLister configv1listers.InfrastructureLister
}

func NewBootstrapTeardownController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
	infrastructureLister configv1listers.InfrastructureLister,
) factory.Controller {
	c := &BootstrapTeardownController{
		operatorClient:       operatorClient,
		etcdClient:           etcdClient,
		configmapLister:      kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Lister(),
		namespaceLister:      kubeInformersForNamespaces.InformersFor("").Core().V1().Namespaces().Lister(),
		infrastructureLister: infrastructureLister,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BootstrapTeardownController", syncer)

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Informer(),
	).WithSync(syncer.Sync).ToController("BootstrapTeardownController", eventRecorder.WithComponentSuffix("bootstrap-teardown-controller"))
}

func (c *BootstrapTeardownController) sync(ctx context.Context, _ factory.SyncContext) error {
	timeoutCtx, cancelFunc := context.WithTimeout(ctx, 1*time.Minute)
	defer cancelFunc()

	scalingStrategy, err := ceohelpers.GetBootstrapScalingStrategy(c.operatorClient, c.namespaceLister, c.infrastructureLister)
	if err != nil {
		return fmt.Errorf("failed to get bootstrap scaling strategy: %w", err)
	}
	// checks the actual etcd cluster membership API if etcd-bootstrap exists
	safeToRemoveBootstrap, hasBootstrap, bootstrapMember, err := c.canRemoveEtcdBootstrap(ctx, scalingStrategy)
	if err != nil {
		return fmt.Errorf("error while canRemoveEtcdBootstrap: %w", err)
	}

	if hasBootstrap {
		if err := c.ensureBootstrapIsNotLeader(ctx, bootstrapMember); err != nil {
			klog.Errorf("error while ensuring bootstrap is not leader: %v", err)
		}
	}

	err = c.removeBootstrap(timeoutCtx, safeToRemoveBootstrap, hasBootstrap, bootstrapMember)
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapTeardownDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			klog.Errorf("error while updating BootstrapTeardownDegraded status with actual error [%v], [%v]", err, updateErr)
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "BootstrapTeardownDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *BootstrapTeardownController) removeBootstrap(ctx context.Context, safeToRemoveBootstrap bool, hasBootstrap bool, bootstrapMember *etcdserverpb.Member) error {
	bootstrapID := uint64(0)
	bootstrapUrl := "unknown"
	if bootstrapMember != nil {
		bootstrapID = bootstrapMember.ID
		bootstrapUrl = bootstrapMember.GetClientURLs()[0]
	}

	if !hasBootstrap {
		klog.V(4).Infof("no bootstrap anymore setting removal status")
		// this is to ensure the status is always set correctly, even if the status update below failed
		updateErr := setSuccessfulBootstrapRemovalStatus(ctx, c.operatorClient)
		if updateErr != nil {
			return fmt.Errorf("error while setSuccessfulBootstrapRemovalStatus: %w", updateErr)
		}

		// if the bootstrap isn't present, then clearly we're available enough to terminate. This avoids any risk of flapping.
		_, _, updateErr = v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdRunningInCluster",
			Status:  operatorv1.ConditionTrue,
			Reason:  "BootstrapAlreadyRemoved",
			Message: "etcd-bootstrap member is already removed",
		}))

		if updateErr != nil {
			return fmt.Errorf("error while updating BootstrapAlreadyRemoved: %w", updateErr)
		}

		return nil
	}

	if !safeToRemoveBootstrap {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdRunningInCluster",
			Status:  operatorv1.ConditionFalse,
			Reason:  "NotEnoughEtcdMembers",
			Message: "still waiting for three healthy etcd members",
		}))
		if updateErr != nil {
			return fmt.Errorf("error while updating NotEnoughEtcdMembers: %w", updateErr)
		}
		return nil
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    "EtcdRunningInCluster",
		Status:  operatorv1.ConditionTrue,
		Reason:  "EnoughEtcdMembers",
		Message: "enough members found",
	}))
	if updateErr != nil {
		return fmt.Errorf("error while updating EnoughEtcdMembers: %w", updateErr)
	}

	// check to see if bootstrapping is complete
	if isBootstrapComplete, err := bootstrap.IsBootstrapComplete(c.configmapLister); !isBootstrapComplete || err != nil {
		return err
	}

	klog.Warningf("Removing bootstrap member [%x] (%s)", bootstrapID, bootstrapUrl)

	// this is ugly until bootkube is updated, but we want to be sure that bootkube has time to be waiting to watch the condition coming back.
	if err := c.etcdClient.MemberRemove(ctx, bootstrapID); err != nil {
		return fmt.Errorf("error while removing bootstrap member [%x] (%s): %w", bootstrapID, bootstrapUrl, err)
	}

	klog.Infof("Successfully removed bootstrap member [%x] (%s)", bootstrapID, bootstrapUrl)
	// below might fail, since the member removal can cause some downtime for raft to settle on a quorum
	// it's important that everything below is properly retried above during normal controller reconciliation
	return setSuccessfulBootstrapRemovalStatus(ctx, c.operatorClient)
}

func setSuccessfulBootstrapRemovalStatus(ctx context.Context, client v1helpers.StaticPodOperatorClient) error {
	_, _, updateErr := v1helpers.UpdateStatus(ctx, client, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    "EtcdBootstrapMemberRemoved",
		Status:  operatorv1.ConditionTrue,
		Reason:  "BootstrapMemberRemoved",
		Message: "etcd bootstrap member is removed",
	}))
	return updateErr
}

// canRemoveEtcdBootstrap returns whether it is safe to remove bootstrap, whether bootstrap is in the list, and an error
func (c *BootstrapTeardownController) canRemoveEtcdBootstrap(ctx context.Context, scalingStrategy ceohelpers.BootstrapScalingStrategy) (bool, bool, *etcdserverpb.Member, error) {
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return false, false, nil, err
	}

	var hasBootstrap bool
	var bootstrapMember *etcdserverpb.Member
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			hasBootstrap = true
			bootstrapMember = member
			break
		}
	}
	if !hasBootstrap {
		return false, hasBootstrap, bootstrapMember, nil
	}

	// First, enforce the main HA invariants in terms of member counts.
	switch scalingStrategy {
	case ceohelpers.HAScalingStrategy:
		if len(members) < 4 {
			return false, hasBootstrap, bootstrapMember, nil
		}
	case ceohelpers.DelayedHAScalingStrategy:
		if len(members) < 3 {
			return false, hasBootstrap, bootstrapMember, nil
		}
	case ceohelpers.UnsafeScalingStrategy:
		if len(members) < 2 {
			return false, hasBootstrap, bootstrapMember, nil
		}
	}

	// Next, given member counts are satisfied, check member health.
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return false, hasBootstrap, bootstrapMember, nil
	}

	// the etcd-bootstrap member is allowed to be unhealthy and can still be removed
	switch {
	case len(unhealthyMembers) == 0:
		return true, hasBootstrap, bootstrapMember, nil
	case len(unhealthyMembers) > 1:
		return false, hasBootstrap, bootstrapMember, nil
	default:
		if unhealthyMembers[0].Name == "etcd-bootstrap" {
			return true, true, bootstrapMember, nil
		}
		return false, hasBootstrap, bootstrapMember, nil
	}
}

func (c *BootstrapTeardownController) ensureBootstrapIsNotLeader(ctx context.Context, bootstrapMember *etcdserverpb.Member) error {
	if bootstrapMember == nil {
		return fmt.Errorf("bootstrap member was not provided")
	}
	status, err := c.etcdClient.Status(ctx, bootstrapMember.ClientURLs[0])
	if err != nil {
		return fmt.Errorf("could not find bootstrap member status: %w", err)
	}

	if bootstrapMember.ID != status.Leader {
		return nil
	}

	klog.Warningf("Bootstrap member [%x] (%s) detected as leader, trying to move elsewhere...", bootstrapMember.ID, bootstrapMember.GetClientURLs()[0])

	memberHealth, err := c.etcdClient.MemberHealth(ctx)
	if err != nil {
		return fmt.Errorf("could not find member health: %w", err)
	}

	var otherMember *etcdserverpb.Member
	// we can pick any other healthy voting member as the target to move to
	for _, m := range memberHealth.GetHealthyMembers() {
		if m.ID != bootstrapMember.ID && !m.IsLearner {
			otherMember = m
			break
		}
	}

	if otherMember == nil {
		return fmt.Errorf("could not find other healthy member to move leader")
	}

	klog.Warningf("Moving lead from bootstrap member [%x] (%s) detected as leader to [%x] (%s)", bootstrapMember.ID, bootstrapMember.GetClientURLs()[0], otherMember.ID, otherMember.GetClientURLs()[0])
	err = c.etcdClient.MoveLeader(ctx, otherMember.ID)
	if err != nil {
		return err
	}

	klog.Warningf("Moving lead from bootstrap member [%x] (%s) to [%x] (%s) succesfully!", bootstrapMember.ID, bootstrapMember.GetClientURLs()[0], otherMember.ID, otherMember.GetClientURLs()[0])

	return nil
}
