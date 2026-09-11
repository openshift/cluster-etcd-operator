package externaletcdsupportcontroller

import (
	"context"
	"encoding/json"
	"fmt"

	"time"

	"github.com/ghodss/yaml"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

const (
	// externalEtcdPodConfigMapName is the name of the ConfigMap that holds the
	// external etcd pod manifest, used by the installer to place the static pod
	// on each node.
	externalEtcdPodConfigMapName = "external-etcd-pod"

	// conditionExternalEtcdConfigMapSynced is the operator condition type set
	// after the forced installer revision for the external-etcd-pod ConfigMap
	// has been triggered. It persists forever, immune to revision pruning.
	conditionExternalEtcdConfigMapSynced = "ExternalEtcdConfigMapSynced"

	// forceRedeployReasonExternalEtcdSync is the ForceRedeploymentReason value
	// used to trigger a new installer revision when the external-etcd-pod
	// ConfigMap is first created.
	forceRedeployReasonExternalEtcdSync = "external-etcd-config-map-sync"
)

type ExternalEtcdEnablerController struct {
	operatorClient       v1helpers.StaticPodOperatorClient
	infrastructureLister configv1listers.InfrastructureLister

	targetImagePullSpec   string
	operatorImagePullSpec string
	envVarGetter          etcdenvvar.EnvVar
	etcdLister            operatorv1listers.EtcdLister
	kubeClient            kubernetes.Interface

	enqueueFn func()
}

func NewExternalEtcdEnablerController(
	operatorClient v1helpers.StaticPodOperatorClient,
	targetImagePullSpec, operatorImagePullSpec string,
	envVarGetter etcdenvvar.EnvVar,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	masterNodeInformer cache.SharedIndexInformer,
	etcdsInformer operatorv1informers.EtcdInformer,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder) factory.Controller {

	c := &ExternalEtcdEnablerController{
		operatorClient:        operatorClient,
		infrastructureLister:  infrastructureInformer.Lister(),
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,
		envVarGetter:          envVarGetter,
		kubeClient:            kubeClient,
		etcdLister:            etcdsInformer.Lister(),
	}
	syncCtx := factory.NewSyncContext("ExternalEtcdSupportController", eventRecorder.WithComponentSuffix("external-etcd-support-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}
	envVarGetter.AddListener(c)
	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(time.Minute).
		WithSync(syncer.Sync).
		WithInformers(
			operatorClient.Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer(),
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer(),
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer(),
			masterNodeInformer,
			infrastructureInformer.Informer(),
			networkInformer.Informer(),
			etcdsInformer.Informer(),
		).ToController("ExternalEtcdController", eventRecorder.WithComponentSuffix("external-etcd-controller"))
}

func (c *ExternalEtcdEnablerController) sync(ctx context.Context, syncCtx factory.SyncContext) error {

	operatorSpec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	envVars := c.envVarGetter.GetEnvVars()
	if len(envVars) == 0 {
		// note this will not degrade the controller, that can happen during CEO restarts often due to cold informer caches (expected)
		return fmt.Errorf("ExternalEtcdEnablerController missing env var values")
	}

	etcd, err := c.etcdLister.Get("cluster")
	if err != nil {
		return err
	}

	podSub, err := ceohelpers.GetPodSubstitution(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec, envVars, etcd, true)
	if err != nil {
		return err
	}

	_, _, err = c.supportExternalEtcdOnlyPod(ctx, podSub, c.kubeClient.CoreV1(), syncCtx.Recorder(), operatorSpec)
	if err != nil {
		return fmt.Errorf("configmap/%s: %w", externalEtcdPodConfigMapName, err)
	}

	// After applying the external-etcd-pod ConfigMap, ensure that the
	// installer runs a new revision so every node picks it up. The ConfigMap
	// is Optional:true, so the installer silently skips it if it doesn't
	// exist yet. Once created, a forced revision ensures all nodes sync it.
	if err := c.ensureInstallerRevisionForExternalEtcdPod(ctx, syncCtx.Recorder()); err != nil {
		return err
	}

	return nil
}

// ensureInstallerRevisionForExternalEtcdPod triggers a one-time installer
// revision after the external-etcd-pod ConfigMap is first created. It uses
// an operator condition to track whether the forced revision has already
// been triggered. The controller is implicitly gated on TNF (DualReplica)
// topology because supportExternalEtcdOnlyPod only creates the ConfigMap
// on external-etcd clusters with bootstrap completed; on all other
// topologies the ConfigMap does not exist and this function returns nil
// immediately.
func (c *ExternalEtcdEnablerController) ensureInstallerRevisionForExternalEtcdPod(
	ctx context.Context,
	recorder events.Recorder,
) error {
	// The external-etcd-pod ConfigMap does not exist yet — the transition
	// to external etcd is not complete.
	_, err := c.kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(ctx, externalEtcdPodConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check %s configmap: %w", externalEtcdPodConfigMapName, err)
	}

	// Get fresh operator state so we have the latest status (for condition
	// check) and a current resourceVersion for the spec update.
	operatorSpec, operatorStatus, resourceVersion, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	// The operator condition persists forever, immune to revision pruning.
	if v1helpers.IsOperatorConditionTrue(operatorStatus.Conditions, conditionExternalEtcdConfigMapSynced) {
		klog.V(4).Infof("ExternalEtcdConfigMapSynced condition already set, forced revision already triggered")
		return nil
	}

	// ConfigMap exists but condition not set: force a new revision so the
	// installer syncs the ConfigMap to all nodes.
	operatorSpec.ForceRedeploymentReason = forceRedeployReasonExternalEtcdSync
	_, _, err = c.operatorClient.UpdateStaticPodOperatorSpec(ctx, resourceVersion, operatorSpec)
	if err != nil {
		return fmt.Errorf("failed to set ForceRedeploymentReason for %s sync: %w", externalEtcdPodConfigMapName, err)
	}

	// Mark the forced revision as done via an operator condition.
	_, _, err = v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    conditionExternalEtcdConfigMapSynced,
		Status:  operatorv1.ConditionTrue,
		Reason:  "ConfigMapSynced",
		Message: fmt.Sprintf("%s configmap created, forced installer revision to sync to all nodes", externalEtcdPodConfigMapName),
	}))
	if err != nil {
		return fmt.Errorf("failed to set ExternalEtcdConfigMapSynced condition: %w", err)
	}

	recorder.Eventf("ExternalEtcdPodConfigMapSyncForced",
		"%s configmap created, forcing installer revision to sync to all nodes", externalEtcdPodConfigMapName)
	klog.V(2).Infof("%s configmap exists, set ForceRedeploymentReason to trigger new installer revision", externalEtcdPodConfigMapName)
	return nil
}

func (c *ExternalEtcdEnablerController) supportExternalEtcdOnlyPod(
	ctx context.Context,
	subs *ceohelpers.PodSubstitutionTemplate,
	client coreclientv1.ConfigMapsGetter,
	recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {

	// We always want the etcd container here
	subs.EnableEtcdContainer = true
	renderedTemplate, err := ceohelpers.RenderTemplate("etcd/pod.gotpl.yaml", subs)
	if err != nil {
		return nil, false, err
	}

	// keep only the etcd container
	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(renderedTemplate), &pod); err != nil {
		return nil, false, err
	}

	filteredContainer := []corev1.Container{}
	for _, container := range pod.Spec.Containers {
		if container.Name == "etcd" {
			filteredContainer = append(filteredContainer, container)
			break
		}
	}
	pod.Spec.Containers = filteredContainer

	// convert it back in json format. The external Etcd manager might need to
	// modify this manifest and the node is expected to have jq installed and not yq
	filteredPodBytes, err := json.Marshal(&pod)
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal pod.yaml: %w", err)
	}

	podConfigMap := resourceread.ReadConfigMapV1OrDie(bindata.MustAsset("etcd/external-etcd-pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = string(filteredPodBytes)
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()

	// Check external etcd cluster status including bootstrap completion
	externalEtcdStatus, err := ceohelpers.GetExternalEtcdClusterStatus(ctx, c.operatorClient, c.infrastructureLister)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get external etcd cluster status: %w", err)
	}

	// Only create the ConfigMap if it's an external etcd cluster AND bootstrap is completed
	if !externalEtcdStatus.IsExternalEtcdCluster || !externalEtcdStatus.IsEtcdRunningInCluster {
		klog.V(4).Infof("external etcd support is disabled or bootstrap not completed: deleting configmap")
		return resourceapply.DeleteConfigMap(ctx, client, recorder, podConfigMap)
	}
	klog.V(4).Infof("external etcd support enabled and bootstrap completed: creating configmap")
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *ExternalEtcdEnablerController) Enqueue() {
	c.enqueueFn()
}
