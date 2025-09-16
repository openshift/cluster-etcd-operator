package externaletcdsupportcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ghodss/yaml"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/status"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

type ExternalEtcdEnablerController struct {
	operatorClient v1helpers.StaticPodOperatorClient

	targetImagePullSpec      string
	operatorImagePullSpec    string
	envVarGetter             etcdenvvar.EnvVar
	etcdLister               operatorv1listers.EtcdLister
	dualReplicaClusterStatus status.ClusterStatus
	kubeClient               kubernetes.Interface

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
	eventRecorder events.Recorder,
	dualReplicaClusterStatus status.ClusterStatus) factory.Controller {

	c := &ExternalEtcdEnablerController{
		operatorClient:           operatorClient,
		targetImagePullSpec:      targetImagePullSpec,
		operatorImagePullSpec:    operatorImagePullSpec,
		envVarGetter:             envVarGetter,
		kubeClient:               kubeClient,
		etcdLister:               etcdsInformer.Lister(),
		dualReplicaClusterStatus: dualReplicaClusterStatus,
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
		err = fmt.Errorf("%q: %w", "configmap/external-etcd-pod", err)
	}

	return err
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

	// we can prepare the external etcd support already while TNF setup is still running
	supportExternalEtcd := c.dualReplicaClusterStatus.IsBootstrapCompleted()
	if !supportExternalEtcd {
		klog.V(4).Infof("external etcd support is disabled: deleting configmap")
		return resourceapply.DeleteConfigMap(ctx, client, recorder, podConfigMap)
	}
	klog.V(4).Infof("external etcd support enabled: creating configmap")
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *ExternalEtcdEnablerController) Enqueue() {
	c.enqueueFn()
}
