package etcdendpointscontroller

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

// EtcdEndpointsController maintains a configmap resource with
// IP addresses for etcd. It should never depend on DNS directly or transitively.
type EtcdEndpointsController struct {
	operatorClient  v1helpers.OperatorClient
	etcdClient      etcdcli.EtcdClient
	nodeLister      corev1listers.NodeLister
	configmapLister corev1listers.ConfigMapLister
	configmapClient corev1client.ConfigMapsGetter
}

func NewEtcdEndpointsController(
	operatorClient v1helpers.OperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
) factory.Controller {
	kubeInformersForTargetNamespace := kubeInformers.InformersFor(operatorclient.TargetNamespace)
	configmapsInformer := kubeInformersForTargetNamespace.Core().V1().ConfigMaps()
	kubeInformersForCluster := kubeInformers.InformersFor("")
	nodeInformer := kubeInformersForCluster.Core().V1().Nodes()

	c := &EtcdEndpointsController{
		operatorClient:  operatorClient,
		etcdClient:      etcdClient,
		nodeLister:      nodeInformer.Lister(),
		configmapLister: configmapsInformer.Lister(),
		configmapClient: kubeClient.CoreV1(),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		configmapsInformer.Informer(),
		nodeInformer.Informer(),
	).WithSync(c.sync).ToController("EtcdEndpointsController", eventRecorder.WithComponentSuffix("etcd-endpoints-controller"))
}

func (c *EtcdEndpointsController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.syncConfigMap(ctx, syncCtx.Recorder())

	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdEndpointsDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingEtcdEndpoints",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("EtcdEndpointsErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "EtcdEndpointsDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "EtcdEndpointsUpdated",
	}))
	if updateErr != nil {
		syncCtx.Recorder().Warning("EtcdEndpointsErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *EtcdEndpointsController) syncConfigMap(ctx context.Context, recorder events.Recorder) error {
	// This resource should have been created at installation and should never
	// be deleted. Treat the inability to access it as fatal to smoke out any
	// problems which could indicate the resource is gone.
	existing, err := c.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
	if err != nil {
		return fmt.Errorf("couldn't get required configmap %s/%s: %w", operatorclient.TargetNamespace, "etcd-endpoints", err)
	}
	_, existingHasBootstrapIP := existing.Annotations[etcdcli.BootstrapIPAnnotationKey]

	// If the bootstrap IP annotation appears to have been prematurely removed, generate a warning.
	// This could be a transient condition depending on the state of caches at the time of sync,
	// so a warning seems sufficient. This check may be overly paranoid, but a significant amount
	// of time has been spent in the past tracing problems back to unexpected deletions and
	// modifications of the endpoints structure (previously a literal endpoints resource).
	isSafe, err := c.isSafeToRemoveBootstrapIP()
	if err != nil {
		return fmt.Errorf("failed to determine whether it's safe to remove the boostrap IP annotation: %w", err)
	}
	if !existingHasBootstrapIP && !isSafe {
		recorder.Warningf("EtcdEndpointsMissingBootstrapIP", "the bootstrap IP annotation has been removed from the %s/%s "+
			"configmap, but current cluster state indicates the annotation may still be required", existing.Namespace, existing.Name)
	}

	required := configMapAsset()

	// create endpoint addresses for each node
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return fmt.Errorf("unable to list expected etcd member nodes: %v", err)
	}
	endpointAddresses := map[string]string{}
	for _, node := range nodes {
		var nodeInternalIP string
		for _, nodeAddress := range node.Status.Addresses {
			if nodeAddress.Type == corev1.NodeInternalIP {
				nodeInternalIP = nodeAddress.Address
				break
			}
		}
		if len(nodeInternalIP) == 0 {
			return fmt.Errorf("unable to determine internal ip address for node %s", node.Name)
		}
		endpointAddresses[base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(nodeInternalIP))] = nodeInternalIP
	}

	if len(endpointAddresses) == 0 {
		return fmt.Errorf("no master nodes are present")
	}

	required.Data = endpointAddresses

	// Apply endpoint updates
	if _, _, err := resourceapply.ApplyConfigMap(c.configmapClient, recorder, required); err != nil {
		return err
	}

	// Try and remove the bootstrap annotation.
	if existingHasBootstrapIP {
		if err := c.tryRemoveBootstrapIP(ctx, recorder); err != nil {
			// We can try again later without rippling out failures because the impact
			// of the stale bootstrap endpoint is somewhat benign client errors which
			// should resolve when we finally succeed.
			utilruntime.HandleError(fmt.Errorf("failed to remove bootstrap IP annotation: %w", err))
		}
	}

	return nil
}

func configMapAsset() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd-endpoints",
			Namespace: operatorclient.TargetNamespace,
		},
	}
}

// isSafeToRemoveBootstrapIP returns true if it's safe to remove the bootstrap
// member IP from the endpoints list, otherwise false. If the determination
// can't be made, returns false and an error.
func (c *EtcdEndpointsController) isSafeToRemoveBootstrapIP() (bool, error) {
	// See if the etcd client knows about the bootstrap member.
	members, err := c.etcdClient.MemberList()
	if err != nil {
		return false, fmt.Errorf("couldn't list etcd members: %w", err)
	}
	bootstrapFound := false
	for _, member := range members {
		if member.Name == "etcd-bootstrap" {
			bootstrapFound = true
			break
		}
	}
	return !bootstrapFound && len(members) >= 3, nil
}

// tryRemoveBootstrapIP will remove the bootstrap IP annotation from the endpoints
// configmap. If bootstrapping is detected to still be in progress, the function
// does nothing and returns no error to facilitate quiet retries.
func (c *EtcdEndpointsController) tryRemoveBootstrapIP(ctx context.Context, recorder events.Recorder) error {
	existing, err := c.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
	if err != nil {
		return fmt.Errorf("couldn't get required configmap %s/%s: %w", operatorclient.TargetNamespace, "etcd-endpoints", err)
	}

	// Only proceed if there's actually something to do.
	if _, existingHasBootstrapIP := existing.Annotations[etcdcli.BootstrapIPAnnotationKey]; !existingHasBootstrapIP {
		return nil
	}

	// See if the etcd client knows about the bootstrap member.
	isSafe, err := c.isSafeToRemoveBootstrapIP()
	if err != nil {
		return fmt.Errorf("failed to determine whether it's safe to remove the boostrap IP annotation: %w", err)
	}
	// If it's not yet safe to remove the annotation, no-op and we'll try
	// again during another sync.
	if !isSafe {
		return nil
	}

	// Bootstrap appears to be complete, so remove the bootstrap IP annotation.
	updated := existing.DeepCopy()
	delete(updated.Annotations, etcdcli.BootstrapIPAnnotationKey)
	if _, err := c.configmapClient.ConfigMaps(operatorclient.TargetNamespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update configmap %s/%s: %w", updated.Namespace, updated.Name, err)
	}
	recorder.Eventf("BootstrapIPRemoved", "Removed etcd bootstrap member IP annotation from configmap %s/%s", updated.Namespace, updated.Name)
	return nil
}
