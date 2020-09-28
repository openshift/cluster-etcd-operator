package etcdenvvar

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const workQueueKey = "key"

type EnvVarController struct {
	operatorClient v1helpers.StaticPodOperatorClient

	envVarMapLock       sync.Mutex
	envVarMap           map[string]string
	targetImagePullSpec string
	listeners           []Enqueueable

	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	configmapLister      corev1listers.ConfigMapLister
	nodeLister           corev1listers.NodeLister

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue         workqueue.RateLimitingInterface
	cachesToSync  []cache.InformerSynced
	eventRecorder events.Recorder
}

type Enqueueable interface {
	Enqueue()
}

func NewEnvVarController(
	targetImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	eventRecorder events.Recorder,
) *EnvVarController {
	c := &EnvVarController{
		operatorClient:       operatorClient,
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),
		configmapLister:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister(),
		nodeLister:           kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		targetImagePullSpec:  targetImagePullSpec,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EnvVarController"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			infrastructureInformer.Informer().HasSynced,
			networkInformer.Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		},
		eventRecorder: eventRecorder.WithComponentSuffix("env-var-controller"),
	}

	operatorClient.Informer().AddEventHandler(c.eventHandler())
	infrastructureInformer.Informer().AddEventHandler(c.eventHandler())
	networkInformer.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())

	// TODO only trigger on master nodes
	kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *EnvVarController) AddListener(listener Enqueueable) {
	c.listeners = append(c.listeners, listener)
}

func (c *EnvVarController) GetEnvVars() map[string]string {
	c.envVarMapLock.Lock()
	defer c.envVarMapLock.Unlock()

	ret := map[string]string{}
	for k, v := range c.envVarMap {
		ret[k] = v
	}
	return ret
}

func (c *EnvVarController) sync() error {
	err := c.checkEnvVars()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EnvVarControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EnvVarControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "EnvVarControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *EnvVarController) checkEnvVars() error {
	operatorSpec, operatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	ctx := envVarContext{
		targetImagePullSpec:  c.targetImagePullSpec,
		spec:                 *operatorSpec,
		status:               *operatorStatus,
		configmapLister:      c.configmapLister,
		nodeLister:           c.nodeLister,
		infrastructureLister: c.infrastructureLister,
		networkLister:        c.networkLister,
	}

	bootstrapComplete, err := isBootstrapComplete(c.configmapLister, c.operatorClient)
	if err != nil {
		return fmt.Errorf("couldn't determine bootstrap status: %w", err)
	}
	isUnsupportedUnsafeEtcd, err := ceohelpers.IsUnsupportedUnsafeEtcd(&ctx.spec)
	if err != nil {
		return fmt.Errorf("couldn't determine etcd unsupported override status: %w", err)
	}
	// TODO: determine this through a state check
	isManagedByAssistedInstaller := true
	nodeCount := len(ctx.status.NodeStatuses)

	// There are three discrete validation paths to enforce HA invariants
	// depending on whether the unsupported/unsafe config is set, whether the
	// cluster is managed by assisted installer, and the default configuration.
	switch {
	case isUnsupportedUnsafeEtcd:
		// TODO: this is to help make sure assisted installer can run in a supported config.
		// TODO: can remove this after integration testing.
		if isManagedByAssistedInstaller {
			return fmt.Errorf("assisted installer is running with an unsupported etcd configuration")
		}
		// When running in an unsupported/unsafe configuration, no guarantees are
		// made once a single node is available.
		if nodeCount < 1 {
			return fmt.Errorf("at least one node is required to have a valid configuration")
		}
	case isManagedByAssistedInstaller:
		// When managed by assisted installer, tolerate unsafe conditions only up
		// until bootstrap is complete, and then enforce as in the supported case.
		if nodeCount < 3 && bootstrapComplete {
			return fmt.Errorf("at least three nodes are required to have a valid configuration (have %d)", nodeCount)
		}
	default:
		// When running in a normal supported configuration, enforce HA invariants
		// at all times.
		if nodeCount < 3 {
			return fmt.Errorf("at least three nodes are required to have a valid configuration (have %d)", nodeCount)
		}
	}

	currEnvVarMap, err := getEtcdEnvVars(ctx)
	if err != nil {
		return err
	}
	c.envVarMapLock.Lock()
	defer c.envVarMapLock.Unlock()

	if !reflect.DeepEqual(c.envVarMap, currEnvVarMap) {
		c.envVarMap = currEnvVarMap
		for _, listener := range c.listeners {
			listener.Enqueue()
		}
	}

	return nil
}

// Run starts the etcd and blocks until stopCh is closed.
func (c *EnvVarController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting EnvVarController")
	defer klog.Infof("Shutting down EnvVarController")

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

func (c *EnvVarController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *EnvVarController) processNextWorkItem() bool {
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
func (c *EnvVarController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

// isBootstrapComplete returns true if bootstrap has completed.
// TODO: consider sharing with etcdendpointscontroller.
func isBootstrapComplete(configMapClient corev1listers.ConfigMapLister, staticPodClient v1helpers.StaticPodOperatorClient) (bool, error) {
	// do a cheap check to see if the annotation is already gone.
	// check to see if bootstrapping is complete
	bootstrapFinishedConfigMap, err := configMapClient.ConfigMaps("kube-system").Get("bootstrap")
	if err != nil {
		if errors.IsNotFound(err) {
			// If the resource was deleted (e.g. by an admin) after bootstrap is actually complete,
			// this is a false negative.
			klog.V(4).Infof("bootstrap considered incomplete because the kube-system/bootstrap configmap wasn't found")
			return false, nil
		}
		// We don't know, give up quickly.
		return false, fmt.Errorf("failed to get configmap %s/%s: %w", "kube-system", "bootstrap", err)
	}

	if status, ok := bootstrapFinishedConfigMap.Data["status"]; !ok || status != "complete" {
		// do nothing, not torn down
		klog.V(4).Infof("bootstrap considered incomplete because status is %q", status)
		return false, nil
	}

	// now run check to stability of revisions
	_, status, _, err := staticPodClient.GetStaticPodOperatorState()
	if err != nil {
		return false, fmt.Errorf("failed to get static pod operator state: %w", err)
	}
	if status.LatestAvailableRevision == 0 {
		return false, nil
	}
	for _, curr := range status.NodeStatuses {
		if curr.CurrentRevision != status.LatestAvailableRevision {
			klog.V(4).Infof("bootstrap considered incomplete because revision %d is still in progress", status.LatestAvailableRevision)
			return false, nil
		}
	}
	return true, nil
}
