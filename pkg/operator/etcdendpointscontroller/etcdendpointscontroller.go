package etcdendpointscontroller

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
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
	quorumChecker   ceohelpers.QuorumChecker
}

func NewEtcdEndpointsController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.StaticPodOperatorClient,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
	quorumChecker ceohelpers.QuorumChecker,
) factory.Controller {
	nodeInformer := kubeInformers.InformersFor("").Core().V1().Nodes()

	c := &EtcdEndpointsController{
		operatorClient:  operatorClient,
		etcdClient:      etcdClient,
		nodeLister:      nodeInformer.Lister(),
		configmapLister: kubeInformers.ConfigMapLister(),
		configmapClient: kubeClient.CoreV1(),
		quorumChecker:   quorumChecker,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("EtcdEndpointsController", syncer)

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
		nodeInformer.Informer(),
	).WithSync(syncer.Sync).ToController("EtcdEndpointsController", eventRecorder.WithComponentSuffix("etcd-endpoints-controller"))
}

func (c *EtcdEndpointsController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.syncConfigMap(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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
	required := configMapAsset()
	// If the bootstrap IP is present on the existing configmap, either copy it
	// forward or remove it if possible so clients can forget about it.
	if existing, err := c.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints"); err == nil && existing != nil {
		if existingIP, hasExistingIP := existing.Annotations[etcdcli.BootstrapIPAnnotationKey]; hasExistingIP {
			bootstrapComplete, err := ceohelpers.IsBootstrapComplete(c.configmapLister, c.operatorClient, c.etcdClient)
			if err != nil {
				return fmt.Errorf("couldn't determine bootstrap status: %w", err)
			}

			if bootstrapComplete {
				// rename the annotation once we're done with bootstrapping
				required.Annotations[etcdcli.BootstrapIPAnnotationKey+"-"] = existingIP
			} else {
				required.Annotations[etcdcli.BootstrapIPAnnotationKey] = existingIP
			}
		}
	} else if !errors.IsNotFound(err) {
		klog.Warningf("required configmap %s/%s will be created because it was missing: %v", operatorclient.TargetNamespace, "etcd-endpoints", err)
	}

	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("failed to get member list: %w", err)
	}

	endpointAddresses := make(map[string]string, len(members))
	// Create endpoint addresses for each member of the cluster.
	for _, member := range members {
		// Since a learner member rejects all requests other than serializable reads and member status API
		// we have to exclude them from the etcd-endpoints configmap to prevent the API server from sending
		// requests to it which would fail with "rpc not supported for learner" errors
		if member.IsLearner {
			continue
		}

		if member.Name == "etcd-bootstrap" {
			continue
		}
		// Use of PeerURL is expected here because it is a mandatory field, and it will mirror ClientURL.
		ip, err := dnshelpers.GetIPFromAddress(member.PeerURLs[0])
		if err != nil {
			return err
		}
		endpointAddresses[fmt.Sprintf("%x", member.ID)] = ip
	}

	if len(endpointAddresses) == 0 {
		return fmt.Errorf("no etcd members are present")
	}

	required.Data = endpointAddresses

	safe, err := c.quorumChecker.IsSafeToUpdateRevision()
	if err != nil {
		return fmt.Errorf("EtcdEndpointsController can't evaluate whether quorum is safe: %w", err)
	}
	if !safe {
		return fmt.Errorf("skipping EtcdEndpointsController reconciliation due to insufficient quorum")
	}

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
