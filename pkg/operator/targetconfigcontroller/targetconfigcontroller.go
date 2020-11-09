package targetconfigcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorclientv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

const (
	disableRollbackCopierAnnotation = "openshift.io/disable-rollbackcopier"
	rollbackCopierContainerName     = "rollbackcopier"
)

type TargetConfigController struct {
	targetImagePullSpec   string
	operatorImagePullSpec string

	operatorClient       v1helpers.StaticPodOperatorClient
	operatorConfigClient operatorversionedclient.Interface

	kubeClient           kubernetes.Interface
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	configMapLister      corev1listers.ConfigMapLister
	endpointLister       corev1listers.EndpointsLister
	nodeLister           corev1listers.NodeLister
	envVarGetter         *etcdenvvar.EnvVarController

	enqueueFn func()
}

func NewTargetConfigController(
	targetImagePullSpec, operatorImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	operatorConfigClient operatorversionedclient.Interface,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	kubeClient kubernetes.Interface,
	envVarGetter *etcdenvvar.EnvVarController,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &TargetConfigController{
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,

		operatorClient:       operatorClient,
		operatorConfigClient: operatorConfigClient,
		kubeClient:           kubeClient,
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),
		configMapLister:      kubeInformersForNamespaces.ConfigMapLister(),
		endpointLister:       kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		nodeLister:           kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		envVarGetter:         envVarGetter,
	}

	syncCtx := factory.NewSyncContext("TargetConfigController", eventRecorder.WithComponentSuffix("target-config-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}
	envVarGetter.AddListener(c)

	return factory.New().WithSyncContext(syncCtx).ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer(),
		kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer(),
		kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer(),
		kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
		infrastructureInformer.Informer(),
		networkInformer.Informer(),
	).WithSync(c.sync).ToController("TargetConfigController", syncCtx.Recorder())
}

func (c TargetConfigController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}
	requeue, err := createTargetConfig(c, syncCtx.Recorder(), operatorSpec)
	if err != nil {
		return err
	}
	if requeue {
		return fmt.Errorf("synthetic requeue request")
	}

	return nil
}

// createTargetConfig takes care of creation of valid resources in a fixed name.  These are inputs to other control loops.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func createTargetConfig(c TargetConfigController, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (bool, error) {
	errors := []error{}

	contentReplacer, err := c.getSubstitutionReplacer(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec)
	if err != nil {
		return false, err
	}

	_, _, err = manageEtcdConfig(c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/config", err))
	}
	_, _, err = c.manageStandardPod(contentReplacer, c.kubeClient.CoreV1(), c.operatorConfigClient.OperatorV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/etcd-pod", err))
	}
	_, _, err = c.manageRecoveryPod(contentReplacer, c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/restore-etcd-pod", err))
	}

	if len(errors) > 0 {
		condition := operatorv1.OperatorCondition{
			Type:    "TargetConfigControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SynchronizationError",
			Message: v1helpers.NewMultiLineAggregate(errors).Error(),
		}
		if _, _, err := v1helpers.UpdateStaticPodStatus(c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
			return true, err
		}
		return true, nil
	}

	condition := operatorv1.OperatorCondition{
		Type:   "TargetConfigControllerDegraded",
		Status: operatorv1.ConditionFalse,
	}
	if _, _, err := v1helpers.UpdateStaticPodStatus(c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
		return true, err
	}

	return false, nil
}

func manageEtcdConfig(client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	configMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/cm.yaml"))
	defaultConfig := etcd_assets.MustAsset("etcd/defaultconfig.yaml")

	requiredConfigMap, _, err := resourcemerge.MergeConfigMap(
		configMap,
		"config.yaml",
		nil,
		defaultConfig,
		operatorSpec.ObservedConfig.Raw,
		operatorSpec.UnsupportedConfigOverrides.Raw,
	)
	if err != nil {
		return nil, false, err
	}
	return resourceapply.ApplyConfigMap(client, recorder, requiredConfigMap)
}

func loglevelToKlog(logLevel operatorv1.LogLevel) string {
	switch logLevel {
	case operatorv1.Normal:
		return "2"
	case operatorv1.Debug:
		return "4"
	case operatorv1.Trace:
		return "6"
	case operatorv1.TraceAll:
		return "8"
	default:
		return "2"
	}
}

func (c *TargetConfigController) getSubstitutionReplacer(operatorSpec *operatorv1.StaticPodOperatorSpec, imagePullSpec, operatorImagePullSpec string) (*strings.Replacer, error) {
	envVarMap := c.envVarGetter.GetEnvVars()
	if len(envVarMap) == 0 {
		return nil, fmt.Errorf("missing env var values")
	}

	envVarLines := []string{}
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("      - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("        value: %q", v))
	}

	return strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${OPERATOR_IMAGE}", operatorImagePullSpec,
		"${VERBOSITY}", loglevelToKlog(operatorSpec.LogLevel),
		"${LISTEN_ON_ALL_IPS}", "0.0.0.0", // TODO this needs updating to detect ipv6-ness
		"${LOCALHOST_IP}", "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		"${COMPUTED_ENV_VARS}", strings.Join(envVarLines, "\n"), // lacks beauty, but it works
	), nil
}

func (c *TargetConfigController) manageRecoveryPod(substitutionReplacer *strings.Replacer, client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	podBytes := etcd_assets.MustAsset("etcd/restore-pod.yaml")
	substitutedPodString := substitutionReplacer.Replace(string(podBytes))

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/restore-pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = substitutedPodString
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(client, recorder, podConfigMap)
}

func (c *TargetConfigController) manageStandardPod(substitutionReplacer *strings.Replacer, client coreclientv1.ConfigMapsGetter, etcdsClient operatorclientv1.EtcdsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	podBytes := etcd_assets.MustAsset("etcd/pod.yaml")
	substitutedPodString := substitutionReplacer.Replace(string(podBytes))

	// Generically support removing the rollbackcopier container if the user has
	// explicitly disabled it through an annotation. Remove it dynamically here
	// so that the function works across all releases (as the copier doesn't exist
	// in versions 4.6+).
	etcdConfig, err := etcdsClient.Etcds().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	pod := &corev1.Pod{}
	if err := yaml.Unmarshal(podBytes, pod); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal etcd pod: %w", err)
	}
	updatedPod := ensureRollbackCopier(pod, etcdConfig)
	if !equality.Semantic.DeepEqual(pod, updatedPod) {
		newPodBytes, err := yaml.Marshal(updatedPod)
		if err != nil {
			return nil, false, fmt.Errorf("failed to marshal etcd pod: %w", err)
		}
		podBytes = newPodBytes
	}

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = substitutedPodString
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(client, recorder, podConfigMap)
}

func ensureRollbackCopier(etcdPod *corev1.Pod, etcdConfig *operatorv1.Etcd) *corev1.Pod {
	_, rollbackCopierDisabled := etcdConfig.Annotations[disableRollbackCopierAnnotation]
	if !rollbackCopierDisabled {
		return etcdPod
	}
	updated := etcdPod.DeepCopy()
	for i, container := range updated.Spec.Containers {
		if container.Name == rollbackCopierContainerName {
			updated.Spec.Containers = append(updated.Spec.Containers[:i], updated.Spec.Containers[i+1:]...)
			break
		}
	}
	return updated
}

func (c *TargetConfigController) Enqueue() {
	c.enqueueFn()
}

func (c *TargetConfigController) namespaceEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				c.Enqueue()
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := old.(*corev1.Namespace)
			if !ok {
				c.Enqueue()
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				ns, ok = tombstone.Obj.(*corev1.Namespace)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace %#v", obj))
					return
				}
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
	}
}
