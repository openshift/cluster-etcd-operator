package bootstrapteardown

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	syncer := health.NewCheckingSyncWrapper(c.sync, 5*time.Minute)
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
	safeToRemoveBootstrap, hasBootstrap, bootstrapID, err := c.canRemoveEtcdBootstrap(ctx, scalingStrategy)
	if err != nil {
		return fmt.Errorf("error while canRemoveEtcdBootstrap: %w", err)
	}

	err = c.removeBootstrap(timeoutCtx, safeToRemoveBootstrap, hasBootstrap, bootstrapID)
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

func (c *BootstrapTeardownController) removeBootstrap(ctx context.Context, safeToRemoveBootstrap bool, hasBootstrap bool, bootstrapID uint64) error {
	if !hasBootstrap {
		klog.V(4).Infof("no bootstrap anymore setting removal status")
		// this is to ensure the status is always set correctly, even if the status update below failed
		updateErr := setSuccessfulBoostrapRemovalStatus(ctx, c.operatorClient)
		if updateErr != nil {
			return fmt.Errorf("error while setSuccessfulBoostrapRemovalStatus: %w", updateErr)
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
	cmNamespace := "kube-system"
	cmName := "bootstrap"
	bootstrapFinishedConfigMap, err := c.configmapLister.ConfigMaps(cmNamespace).Get(cmName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Warningf("cluster-bootstrap is not yet finished - ConfigMap '%s/%s' not found", cmNamespace, cmName)
			return nil
		}
		return fmt.Errorf("error while getting bootstrap configmap: %w", err)
	}

	if bootstrapFinishedConfigMap.Data["status"] != "complete" {
		klog.Warningf("cluster-bootstrap is not yet finished, bootstrap status=[%s]", bootstrapFinishedConfigMap.Data["status"])
		return nil
	}

	klog.Warningf("Removing bootstrap member [%x]", bootstrapID)

	// this is ugly until bootkube is updated, but we want to be sure that bootkube has time to be waiting to watch the condition coming back.
	if err := c.etcdClient.MemberRemove(ctx, bootstrapID); err != nil {
		return fmt.Errorf("error while removing bootstrap member [%x]: %w", bootstrapID, err)
	}

	klog.Infof("Successfully removed bootstrap member [%x]", bootstrapID)
	// below might fail, since the member removal can cause some downtime for raft to settle on a quorum
	// it's important that everything below is properly retried above during normal controller reconciliation
	return setSuccessfulBoostrapRemovalStatus(ctx, c.operatorClient)
}

func setSuccessfulBoostrapRemovalStatus(ctx context.Context, client v1helpers.StaticPodOperatorClient) error {
	_, _, updateErr := v1helpers.UpdateStatus(ctx, client, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    "EtcdBoostrapMemberRemoved",
		Status:  operatorv1.ConditionTrue,
		Reason:  "BootstrapMemberRemoved",
		Message: "etcd bootstrap member is removed",
	}))
	return updateErr
}

// canRemoveEtcdBootstrap returns whether it is safe to remove bootstrap, whether bootstrap is in the list, and an error
func (c *BootstrapTeardownController) canRemoveEtcdBootstrap(ctx context.Context, scalingStrategy ceohelpers.BootstrapScalingStrategy) (bool, bool, uint64, error) {
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return false, false, 0, err
	}

	var hasBootstrap bool
	var bootstrapMemberID uint64
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			hasBootstrap = true
			bootstrapMemberID = member.ID
			break
		}
	}
	if !hasBootstrap {
		return false, hasBootstrap, bootstrapMemberID, nil
	}

	// First, enforce the main HA invariants in terms of member counts.
	switch scalingStrategy {
	case ceohelpers.HAScalingStrategy:
		if len(members) < 4 {
			return false, hasBootstrap, bootstrapMemberID, nil
		}
	case ceohelpers.DelayedHAScalingStrategy:
		if len(members) < 3 {
			return false, hasBootstrap, bootstrapMemberID, nil
		}
	case ceohelpers.UnsafeScalingStrategy:
		if len(members) < 2 {
			return false, hasBootstrap, bootstrapMemberID, nil
		}
	}

	// Next, given member counts are satisfied, check member health.
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return false, hasBootstrap, bootstrapMemberID, nil
	}

	// the etcd-bootstrap member is allowed to be unhealthy and can still be removed
	switch {
	case len(unhealthyMembers) == 0:
		return true, hasBootstrap, bootstrapMemberID, nil
	case len(unhealthyMembers) > 1:
		return false, hasBootstrap, bootstrapMemberID, nil
	default:
		if unhealthyMembers[0].Name == "etcd-bootstrap" {
			return true, true, unhealthyMembers[0].ID, nil
		}
		return false, hasBootstrap, bootstrapMemberID, nil
	}
}
