package bootstrapteardown

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"

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

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Informer(),
	).WithSync(c.sync).ToController("BootstrapTeardownController", eventRecorder.WithComponentSuffix("bootstrap-teardown-controller"))
}

func (c *BootstrapTeardownController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.removeBootstrap(ctx, syncCtx)
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapTeardownDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("BootstrapTeardownErrorUpdatingStatus", updateErr.Error())
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

func (c *BootstrapTeardownController) removeBootstrap(ctx context.Context, syncCtx factory.SyncContext) error {
	// checks the actual etcd cluster membership API if etcd-bootstrap exists
	safeToRemoveBootstrap, hasBootstrap, err := c.canRemoveEtcdBootstrap(ctx)
	switch {
	case err != nil:
		return err

	case !hasBootstrap:
		// if the bootstrap isn't present, then clearly we're available enough to terminate. This avoids any risk of flapping.
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdRunningInCluster",
			Status:  operatorv1.ConditionTrue,
			Reason:  "BootstrapAlreadyRemoved",
			Message: "etcd-bootstrap member is already removed",
		}))
		if updateErr != nil {
			return updateErr
		}
		return nil

	case !safeToRemoveBootstrap:
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdRunningInCluster",
			Status:  operatorv1.ConditionFalse,
			Reason:  "NotEnoughEtcdMembers",
			Message: "still waiting for three healthy etcd members",
		}))
		if updateErr != nil {
			return updateErr
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
		return updateErr
	}

	// check to see if bootstrapping is complete
	cmNamespace := "kube-system"
	cmName := "bootstrap"
	bootstrapFinishedConfigMap, err := c.configmapLister.ConfigMaps(cmNamespace).Get(cmName)
	switch {
	case apierrors.IsNotFound(err):
		syncCtx.Recorder().Event("DelayingBootstrapTeardown", fmt.Sprintf("cluster-bootstrap is not yet finished - ConfigMap '%s/%s' not found", cmNamespace, cmName))
		return nil
	case err != nil:
		return err

	case bootstrapFinishedConfigMap.Data["status"] != "complete":
		syncCtx.Recorder().Event("DelayingBootstrapTeardown", "cluster-bootstrap is not yet finished")
		return nil
	}

	syncCtx.Recorder().Event("RemoveBootstrapEtcd", "removing etcd-bootstrap member")
	// this is ugly until bootkube is updated, but we want to be sure that bootkube has time to be waiting to watch the condition coming back.
	if err := c.etcdClient.MemberRemove(ctx, "etcd-bootstrap"); err != nil {
		return err
	}
	return nil
}

// canRemoveEtcdBootstrap returns whether it is safe to remove bootstrap, whether bootstrap is in the list, and an error
func (c *BootstrapTeardownController) canRemoveEtcdBootstrap(ctx context.Context) (bool, bool, error) {
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return false, false, err
	}

	hasBootstrap := false
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			hasBootstrap = true
		}
	}
	if !hasBootstrap {
		return false, hasBootstrap, nil
	}

	scalingStrategy, err := ceohelpers.GetBootstrapScalingStrategy(c.operatorClient, c.namespaceLister, c.infrastructureLister)
	if err != nil {
		return false, hasBootstrap, fmt.Errorf("failed to get bootstrap scaling strategy: %w", err)
	}

	// First, enforce the main HA invariants in terms of member counts.
	switch scalingStrategy {
	case ceohelpers.HAScalingStrategy:
		if len(members) < 4 {
			return false, hasBootstrap, nil
		}
	case ceohelpers.DelayedHAScalingStrategy, ceohelpers.UnsafeScalingStrategy:
		if len(members) < 2 {
			return false, hasBootstrap, nil
		}
	}

	// Next, given member counts are satisfied, check member health.
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers(ctx)
	if err != nil {
		return false, hasBootstrap, nil
	}

	// the etcd-bootstrap member is allowed to be unhealthy and can still be removed
	switch {
	case len(unhealthyMembers) == 0:
		return true, hasBootstrap, nil
	case len(unhealthyMembers) > 1:
		return false, hasBootstrap, nil
	default:
		if unhealthyMembers[0].Name == "etcd-bootstrap" {
			return true, true, nil
		}
		return false, hasBootstrap, nil
	}
}
