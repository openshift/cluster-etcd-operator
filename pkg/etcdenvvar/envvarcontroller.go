package etcdenvvar

import (
	"context"
	"fmt"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"reflect"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const workQueueKey = "key"

type EnvVarController struct {
	operatorClient v1helpers.StaticPodOperatorClient
	etcdClient     etcdcli.EtcdClient

	envVarMapLock       sync.Mutex
	envVarMap           map[string]string
	targetImagePullSpec string
	listeners           []Enqueueable

	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	configmapLister      corev1listers.ConfigMapLister
	nodeLister           corev1listers.NodeLister
	namespaceLister      corev1listers.NamespaceLister

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
	etcdClient etcdcli.EtcdClient,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	eventRecorder events.Recorder,
) *EnvVarController {
	c := &EnvVarController{
		operatorClient:       operatorClient,
		etcdClient:           etcdClient,
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),
		namespaceLister:      kubeInformersForNamespaces.InformersFor("").Core().V1().Namespaces().Lister(),
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

func (c *EnvVarController) sync(ctx context.Context) error {
	err := c.checkEnvVars(ctx)
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "EnvVarControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *EnvVarController) checkEnvVars(ctx context.Context) error {
	if err := ceohelpers.CheckSafeToScaleCluster(c.configmapLister, c.operatorClient, c.namespaceLister, c.infrastructureLister); err != nil {
		return fmt.Errorf("can't update etcd pod configurations because scaling is currently unsafe: %w", err)
	}

	operatorSpec, operatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	currEnvVarMap, err := getEtcdEnvVars(ctx, envVarContext{
		targetImagePullSpec:  c.targetImagePullSpec,
		spec:                 *operatorSpec,
		status:               *operatorStatus,
		etcdClient:           c.etcdClient,
		configmapLister:      c.configmapLister,
		nodeLister:           c.nodeLister,
		infrastructureLister: c.infrastructureLister,
		networkLister:        c.networkLister,
	})
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
func (c *EnvVarController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting EnvVarController")
	defer klog.Infof("Shutting down EnvVarController")

	if !cache.WaitForCacheSync(ctx.Done(), c.cachesToSync...) {
		return
	}
	klog.V(2).Infof("caches synced")

	// doesn't matter what workers say, only start one.
	go wait.UntilWithContext(ctx, func(ctx2 context.Context) {
		c.runWorker(ctx)
	}, time.Second)

	go wait.Until(func() {
		c.queue.Add(workQueueKey)
	}, time.Minute, ctx.Done())

	<-ctx.Done()
}

func (c *EnvVarController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *EnvVarController) processNextWorkItem(ctx context.Context) bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync(ctx)
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
