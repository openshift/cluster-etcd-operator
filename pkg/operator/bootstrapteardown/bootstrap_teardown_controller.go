package bootstrapteardown

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type BootstrapTeardownController struct {
	operatorClient  v1helpers.StaticPodOperatorClient
	etcdClient      etcdcli.EtcdClient
	configmapLister corev1listers.ConfigMapLister
	endpointsLister corev1listers.EndpointsLister
	endpointsClient corev1client.EndpointsGetter
}

func NewBootstrapTeardownController(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
) factory.Controller {
	c := &BootstrapTeardownController{
		operatorClient:  operatorClient,
		etcdClient:      etcdClient,
		configmapLister: kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Lister(),
		endpointsLister: kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		endpointsClient: kubeClient.CoreV1(),
	}

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Informer(),
	).WithSync(c.sync).ToController("BootstrapTeardownController", eventRecorder.WithComponentSuffix("bootstrap-teardown-controller"))
}

func (c *BootstrapTeardownController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.removeBootstrap(ctx, syncCtx)
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "BootstrapTeardownDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *BootstrapTeardownController) removeBootstrap(ctx context.Context, syncCtx factory.SyncContext) error {
	// checks the actual etcd cluster membership API if etcd-bootstrap exists
	safeToRemoveBootstrap, hasBootstrap, err := c.canRemoveEtcdBootstrap()
	switch {
	case err != nil:
		return err

	case !hasBootstrap:
		// If the bootstrap isn't present, then clearly we're available enough to terminate. This avoids any risk of flapping.
		//
		// Remove the bootstrap IP annotation from the well-known endpoint so etcd clients will stop trying
		// to use the defunct bootstrap member endpoint.
		endpoint, err := c.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd-2")
		if err != nil {
			return err
		}
		if _, found := endpoint.Annotations[etcdcli.BootstrapIPAnnotationKey]; found {
			delete(endpoint.Annotations, etcdcli.BootstrapIPAnnotationKey)
			_, err := c.endpointsClient.Endpoints(operatorclient.TargetNamespace).Update(ctx, endpoint, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to remove bootstrap endpoint annotation from endpoiint %s/%s: %v", endpoint.Namespace, endpoint.Name, err)
			}
			syncCtx.Recorder().Eventf("EndpointsUpdated", "Updated endpoints/%s -n %s to remove bootstrap IP annotation", endpoint.Name, endpoint.Namespace)
		}
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    "EtcdRunningInCluster",
		Status:  operatorv1.ConditionTrue,
		Reason:  "EnoughEtcdMembers",
		Message: "enough members found",
	}))
	if updateErr != nil {
		return updateErr
	}

	// check to see if bootstrapping is complete
	bootstrapFinishedConfigMap, err := c.configmapLister.ConfigMaps("kube-system").Get("bootstrap")
	switch {
	case apierrors.IsNotFound(err):
		syncCtx.Recorder().Event("DelayingBootstrapTeardown", "cluster-bootstrap is not yet finished")
		return nil
	case err != nil:
		return err

	case bootstrapFinishedConfigMap.Data["status"] != "complete":
		syncCtx.Recorder().Event("DelayingBootstrapTeardown", "cluster-bootstrap is not yet finished")
		return nil
	}

	syncCtx.Recorder().Event("RemoveBootstrapEtcd", "removing etcd-bootstrap member")
	// this is ugly until bootkube is updated, but we want to be sure that bootkube has time to be waiting to watch the condition coming back.
	if err := c.etcdClient.MemberRemove("etcd-bootstrap"); err != nil {
		return err
	}
	return nil
}

// canRemoveEtcdBootstrap returns whether it is safe to remove bootstrap, whether bootstrap is in the list, and an error
func (c *BootstrapTeardownController) canRemoveEtcdBootstrap() (bool, bool, error) {
	members, err := c.etcdClient.MemberList()
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

	isUnsupportedUnsafeEtcd, err := c.isUnsupportedUnsafeEtcd()
	if err != nil {
		return false, hasBootstrap, err
	}

	switch {
	case !isUnsupportedUnsafeEtcd && len(members) < 4:
		// bootstrap is not safe to remove until we scale to 4
		return false, hasBootstrap, nil
	case isUnsupportedUnsafeEtcd && len(members) < 2:
		// if etcd is unsupported, bootstrap is not safe to remove
		// until we scale to 2
		return false, hasBootstrap, nil
	default:
		// do nothing fall through on checking the unhealthy members
	}

	unhealthyMembers, err := c.etcdClient.UnhealthyMembers()
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

func (c *BootstrapTeardownController) isUnsupportedUnsafeEtcd() (bool, error) {
	spec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return false, err
	}
	return ceohelpers.IsUnsupportedUnsafeEtcd(spec)
}
