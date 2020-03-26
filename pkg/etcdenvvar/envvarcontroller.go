package etcdenvvar

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
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
	endpointLister       corev1listers.EndpointsLister
	nodeLister           corev1listers.NodeLister
	configmapLister      corev1listers.ConfigMapLister

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
		endpointLister:       kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		nodeLister:           kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		configmapLister:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister(),
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

	currEnvVarMap, err := getEtcdEnvVars(envVarContext{
		targetImagePullSpec:  c.targetImagePullSpec,
		spec:                 *operatorSpec,
		status:               *operatorStatus,
		endpointLister:       c.endpointLister,
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

func (c *EnvVarController) InstallerPodSkipInvalidRevisions() installer.InstallerPodMutationFunc {
	return func(pod *corev1.Pod, nodeName string, operatorSpec *operatorv1.StaticPodOperatorSpec, revision int32) error {
		configMapName := "etcd-pod"
		if revision != 0 {
			configMapName = configMapName + "-" + fmt.Sprint(revision)
		}
		configmap, err := c.configmapLister.ConfigMaps(operatorclient.TargetNamespace).Get(configMapName)
		if err != nil {
			return err
		}

		nodes, found := configmap.Data["nodes"]
		if !found {
			return fmt.Errorf("nodes key not found in configmap: %+v", configmap)
		}
		nodeFound := false
		for _, n := range strings.Split(nodes, ",") {
			if n == nodeName {
				nodeFound = true
				break
			}
		}
		if !nodeFound {
			klog.Infof("node %s not found in revision %d", nodeName, revision)
			pod.Spec.Containers[0].Command = []string{
				"/bin/sh",
				"-c",
				fmt.Sprintf(">&2 echo `node %s not found in revision %d; exit 1`", nodeName, revision)}
		}
		klog.Infof("node %s found in revision %d", nodeName, revision)
		return nil
	}
}
