package targetconfigcontroller

import (
	"fmt"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/client-go/dynamic"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

const workQueueKey = "key"

type TargetConfigController struct {
	targetImagePullSpec   string
	operatorImagePullSpec string

	operatorClient v1helpers.StaticPodOperatorClient

	dyanmicClient   dynamic.Interface
	kubeClient      kubernetes.Interface
	configMapLister corev1listers.ConfigMapLister
	endpointLister  corev1listers.EndpointsLister
	nodeLister      corev1listers.NodeLister
	eventRecorder   events.Recorder

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue        workqueue.RateLimitingInterface
	cachesToSync []cache.InformerSynced
}

func NewTargetConfigController(
	targetImagePullSpec, operatorImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	dyanmicClient dynamic.Interface,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) *TargetConfigController {
	c := &TargetConfigController{
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,

		operatorClient:  operatorClient,
		dyanmicClient:   dyanmicClient,
		kubeClient:      kubeClient,
		configMapLister: kubeInformersForNamespaces.ConfigMapLister(),
		endpointLister:  kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		nodeLister:      kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		eventRecorder:   eventRecorder.WithComponentSuffix("target-config-controller"),

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "TargetConfigController"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().HasSynced,
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		},
	}

	operatorClient.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())

	// TODO only trigger on master nodes
	kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c TargetConfigController) sync() error {
	operatorSpec, operatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}
	requeue, err := createTargetConfig(c, c.eventRecorder, operatorSpec, operatorStatus)
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
func createTargetConfig(c TargetConfigController, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec, operatorStatus *operatorv1.StaticPodOperatorStatus) (bool, error) {
	errors := []error{}

	_, _, err := manageEtcdConfig(c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/config", err))
	}
	_, _, err = c.managePod(c.kubeClient.CoreV1(), recorder, operatorSpec, operatorStatus, c.targetImagePullSpec, c.operatorImagePullSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/kube-apiserver-pod", err))
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

func (c *TargetConfigController) managePod(client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec, operatorStatus *operatorv1.StaticPodOperatorStatus, imagePullSpec, operatorImagePullSpec string) (*corev1.ConfigMap, bool, error) {
	envVarMap, err := getEtcdEnvVars(envVarContext{
		spec:           *operatorSpec,
		status:         *operatorStatus,
		nodeLister:     c.nodeLister,
		dynamicClient:  c.dyanmicClient,
		endpointLister: c.endpointLister,
	})
	if err != nil {
		return nil, false, err
	}
	envVarLines := []string{}
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("      - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("        value: %q", v))
	}

	podBytes := etcd_assets.MustAsset("etcd/pod.yaml")
	r := strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${OPERATOR_IMAGE}", operatorImagePullSpec,
		"${VERBOSITY}", loglevelToKlog(operatorSpec.LogLevel),
		"${LISTEN_ON_ALL_IPS}", "0.0.0.0", // TODO this needs updating to detect ipv6-ness
		"${LOCALHOST_IP}", "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		"${COMPUTED_ENV_VARS}", strings.Join(envVarLines, "\n"), // lacks beauty, but it works
	)
	substitutedPodString := r.Replace(string(podBytes))

	configMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/pod-cm.yaml"))
	configMap.Data["pod.yaml"] = substitutedPodString
	configMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	configMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(client, recorder, configMap)
}

// Run starts the etcd and blocks until stopCh is closed.
func (c *TargetConfigController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting TargetConfigController")
	defer klog.Infof("Shutting down TargetConfigController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		return
	}
	klog.V(2).Infof("caches synced")

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	go wait.Until(func() {
		c.queue.Add(workQueueKey)
	}, time.Minute, stopCh)

	<-stopCh
}

func (c *TargetConfigController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *TargetConfigController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *TargetConfigController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

func (c *TargetConfigController) namespaceEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				c.queue.Add(workQueueKey)
			}
			if ns.Name == ("openshift-etcd") {
				c.queue.Add(workQueueKey)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := old.(*corev1.Namespace)
			if !ok {
				c.queue.Add(workQueueKey)
			}
			if ns.Name == ("openshift-etcd") {
				c.queue.Add(workQueueKey)
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
				c.queue.Add(workQueueKey)
			}
		},
	}
}
