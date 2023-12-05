package etcdenvvar

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"
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

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const workQueueKey = "key"

var nodeEnvRegex = regexp.MustCompile("^NODE_(?P<NodeName>.*)_(?P<Suffix>IP|ETCD_URL_HOST|ETCD_NAME)$")

type EnvVar interface {
	AddListener(listener Enqueueable)
	GetEnvVars() map[string]string
}

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

func (c *EnvVarController) sync(ctx context.Context) error {
	err := c.checkEnvVars()
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

func (c *EnvVarController) checkEnvVars() error {
	operatorSpec, operatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	currEnvVarMap, err := getEtcdEnvVars(envVarContext{
		targetImagePullSpec:  c.targetImagePullSpec,
		spec:                 *operatorSpec,
		status:               *operatorStatus,
		configmapLister:      c.configmapLister,
		nodeLister:           c.nodeLister,
		infrastructureLister: c.infrastructureLister,
		networkLister:        c.networkLister,
	})
	if err != nil {
		return err
	}

	if err := validateMap(*operatorStatus, c.nodeLister, currEnvVarMap); err != nil {
		return fmt.Errorf("found invalid env var configs: %w", err)
	}

	func() {
		c.envVarMapLock.Lock()
		defer c.envVarMapLock.Unlock()

		if !reflect.DeepEqual(c.envVarMap, currEnvVarMap) {
			c.envVarMap = currEnvVarMap
		}
	}()

	// update listeners outside the lock in-case they are synchronously retrieving via GetEnvVars within the listener
	for _, listener := range c.listeners {
		listener.Enqueue()
	}

	return nil
}

// Run starts the etcd and blocks until stopCh is closed.
func (c *EnvVarController) Run(_ int, stopCh <-chan struct{}) {
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
	// TODO: wire this context properly
	ctx := context.TODO()
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

// validateMap checks for coherence of the generated env vars map. Returns an error when an invalid config is detected.
func validateMap(
	status operatorv1.StaticPodOperatorStatus,
	nodeLister corev1listers.NodeLister,
	envVars map[string]string) error {

	if _, ok := envVars[AllEtcdEndpoints]; !ok {
		return fmt.Errorf("map does not contain %s", AllEtcdEndpoints)
	}

	endpoints := strings.Split(envVars[AllEtcdEndpoints], ",")

	// we need to ensure that for every endpoint, the IP and HOST names are properly filled:
	//	NODE_%s_IP
	//	NODE_%s_ETCD_URL_HOST
	//	NODE_%s_ETCD_NAME

	// this map takes the "envVarSafe" node name as key, the value is a map indexed by the suffix (IP, HOST, NAME), which points to its value.
	nodeNameMap := map[string]map[string]string{}

	for k, v := range envVars {
		if nodeEnvRegex.MatchString(k) {
			matches := nodeEnvRegex.FindStringSubmatch(k)
			nodeName := matches[nodeEnvRegex.SubexpIndex("NodeName")]
			suffix := matches[nodeEnvRegex.SubexpIndex("Suffix")]
			m := nodeNameMap[nodeName]
			if m != nil {
				m[suffix] = v
			} else {
				nodeNameMap[nodeName] = map[string]string{suffix: v}
			}
		}
	}

	if len(endpoints) != len(nodeNameMap) {
		return fmt.Errorf("unexpected difference in env vars with endpoints [%v] vs. [%v]", endpoints, nodeNameMap)
	}

	for _, endpoint := range endpoints {
		endpointUrl, _ := url.Parse(endpoint)
		hostPort := strings.Split(endpointUrl.Host, ":")
		if len(hostPort) != 2 {
			return fmt.Errorf("malformed endpoint detected, must be a host:port combination. Found: [%s]", endpoint)
		}

		// we expect the host to be in the value of the IP and ETCD_URL_HOST vars once
		found := false
		for _, v := range nodeNameMap {
			if strings.Contains(v["IP"], hostPort[0]) && strings.Contains(v["ETCD_URL_HOST"], hostPort[0]) {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("found inconsistency with endpoint [%s], no matching coherent env vars found in: [%v]", endpoint, nodeNameMap)
		}
	}

	// TODO(thomas): detect inconsistency in static pod node status and actual listed nodes too

	return nil
}
