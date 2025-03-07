package externaletcdsupportcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"text/template"
	"time"

	"github.com/ghodss/yaml"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/targetconfigcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type ExternalEtcdEnablerController struct {
	operatorClient v1helpers.StaticPodOperatorClient

	targetImagePullSpec   string
	operatorImagePullSpec string
	envVarGetter          etcdenvvar.EnvVar

	kubeClient kubernetes.Interface

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
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder) factory.Controller {

	c := &ExternalEtcdEnablerController{
		operatorClient:        operatorClient,
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,
		envVarGetter:          envVarGetter,
		kubeClient:            kubeClient,
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

	podSub := c.getPodSubstitution(c.targetImagePullSpec, c.operatorImagePullSpec, envVars)
	_, _, err = c.supportExternalPod(ctx, podSub, c.kubeClient.CoreV1(), syncCtx.Recorder(), operatorSpec)
	if err != nil {
		err = fmt.Errorf("%q: %w", "configmap/external-etcd-pod", err)
	}

	return err
}

func (c *ExternalEtcdEnablerController) getPodSubstitution(
	imagePullSpec, operatorImagePullSpec string,
	envVarMap map[string]string) *targetconfigcontroller.PodSubstitutionTemplate {

	var nameValues []targetconfigcontroller.NameValue
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		nameValues = append(nameValues, targetconfigcontroller.NameValue{k, v})
	}

	return &targetconfigcontroller.PodSubstitutionTemplate{
		Image:               imagePullSpec,
		OperatorImage:       operatorImagePullSpec,
		ListenAddress:       "0.0.0.0",   // TODO this needs updating to detect ipv6-ness
		LocalhostAddress:    "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		LogLevel:            "info",
		EnvVars:             nameValues,
		EnableEtcdContainer: true,
	}
}

func (c *ExternalEtcdEnablerController) supportExternalPod(ctx context.Context, subs *targetconfigcontroller.PodSubstitutionTemplate,
	client coreclientv1.ConfigMapsGetter, recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {

	fm := template.FuncMap{"quote": func(arg reflect.Value) string {
		return "\"" + arg.String() + "\""
	}}
	podBytes := etcd_assets.MustAsset("etcd/pod.gotpl.yaml")
	tmpl, err := template.New("pod").Funcs(fm).Parse(string(podBytes))
	if err != nil {
		return nil, false, err
	}

	w := &strings.Builder{}
	err = tmpl.Execute(w, subs)
	if err != nil {
		return nil, false, err
	}

	// keep only the etcd container
	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(w.String()), &pod); err != nil {
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

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/external-etcd-pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = string(filteredPodBytes)
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()

	enabled, err := ceohelpers.IsExternalEtcdSupport(operatorSpec)
	if err != nil {
		return nil, false, fmt.Errorf("could not determine useExternalEtcdSupport config override: %w", err)
	}
	if !enabled {
		klog.V(4).Infof("external etcd support is disabled: deleting configmap")
		return resourceapply.DeleteConfigMap(ctx, client, recorder, podConfigMap)
	}
	klog.V(4).Infof("external etcd support enabled: creating configmap")
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *ExternalEtcdEnablerController) Enqueue() {
	c.enqueueFn()
}
