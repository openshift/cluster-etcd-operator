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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

// EtcdEndpointsController maintains a configmap resource with
// IP addresses for etcd. It should never depend on DNS directly or transitively.
type EtcdEndpointsController struct {
	operatorClient  v1helpers.StaticPodOperatorClient
	etcdClient      etcdcli.EtcdClient
	nodeLister      corev1listers.NodeLister
	configmapLister corev1listers.ConfigMapLister
	configmapClient corev1client.ConfigMapsGetter
}

func NewEtcdEndpointsController(
	operatorClient v1helpers.StaticPodOperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
) factory.Controller {
	nodeInformer := kubeInformers.InformersFor("").Core().V1().Nodes()

	c := &EtcdEndpointsController{
		operatorClient:  operatorClient,
		etcdClient:      etcdClient,
		nodeLister:      nodeInformer.Lister(),
		configmapLister: kubeInformers.ConfigMapLister(),
		configmapClient: kubeClient.CoreV1(),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
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
	bootstrapComplete, err := ceohelpers.IsBootstrapComplete(c.configmapLister, c.operatorClient)
	if err != nil {
		return fmt.Errorf("couldn't determine bootstrap status: %w", err)
	}

	required := configMapAsset()

	// If the bootstrap IP is present on the existing configmap, either copy it
	// forward or remove it if possible so clients can forget about it.
	if existing, err := c.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints"); err == nil && existing != nil {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		memberHealth, err := c.etcdClient.MemberHealth(ctx)
		if err != nil {
			return fmt.Errorf("could not get member health: %w", err)
		}

		if existingIP, hasExistingIP := existing.Annotations[etcdcli.BootstrapIPAnnotationKey]; hasExistingIP {
			if bootstrapComplete && etcdcli.IsQuorumFaultTolerant(memberHealth) {
				// remove the annotation
				required.Annotations[etcdcli.BootstrapIPAnnotationKey+"-"] = existingIP
			} else {
				required.Annotations[etcdcli.BootstrapIPAnnotationKey] = existingIP
			}
		}
	} else if !errors.IsNotFound(err) {
		klog.Warningf("required configmap %s/%s will be created because it was missing: %w", operatorclient.TargetNamespace, "etcd-endpoints", err)
	}

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
	if _, _, err := resourceapply.ApplyConfigMap(ctx, c.configmapClient, recorder, required); err != nil {
		return fmt.Errorf("applying configmap update failed :%w", err)
	}

	return nil
}

func configMapAsset() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "etcd-endpoints",
			Namespace:   operatorclient.TargetNamespace,
			Annotations: map[string]string{},
		},
	}
}
