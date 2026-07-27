package etcdenvvar

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ghodss/yaml"
)

const workQueueKey = "key"

type EnvVar interface {
	AddListener(listener Enqueueable)
	GetEnvVars() map[string]string
}

type EnvVarController struct {
	operatorClient       v1helpers.StaticPodOperatorClient
	operatorConfigClient *operatorversionedclient.Clientset

	envVarMapLock       sync.Mutex
	envVarMap           map[string]string
	targetImagePullSpec string
	listeners           []Enqueueable

	infrastructureLister    configv1listers.InfrastructureLister
	networkLister           configv1listers.NetworkLister
	configmapLister         corev1listers.ConfigMapLister
	secretLister            corev1listers.SecretLister
	masterNodeLister        corev1listers.NodeLister
	masterNodeLabelSelector labels.Selector
	etcdLister              operatorv1listers.EtcdLister
	featureGateAccessor     featuregates.FeatureGateAccess

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
	masterNodeInformer cache.SharedIndexInformer,
	masterNodeLister corev1listers.NodeLister,
	masterNodeLabelSelector labels.Selector,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	eventRecorder events.Recorder,
	etcdsInformer operatorv1informers.EtcdInformer,
	featureGateAccessor featuregates.FeatureGateAccess,
) *EnvVarController {
	c := &EnvVarController{
		operatorClient:          operatorClient,
		infrastructureLister:    infrastructureInformer.Lister(),
		networkLister:           networkInformer.Lister(),
		configmapLister:         kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister(),
		secretLister:            kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
		masterNodeLister:        masterNodeLister,
		masterNodeLabelSelector: masterNodeLabelSelector,
		targetImagePullSpec:     targetImagePullSpec,
		etcdLister:              etcdsInformer.Lister(),
		featureGateAccessor:     featureGateAccessor,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EnvVarController"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			infrastructureInformer.Informer().HasSynced,
			networkInformer.Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer().HasSynced,
			masterNodeInformer.HasSynced,
			etcdsInformer.Informer().HasSynced,
		},
		eventRecorder: eventRecorder.WithComponentSuffix("env-var-controller"),
	}

	operatorClient.Informer().AddEventHandler(c.eventHandler())
	infrastructureInformer.Informer().AddEventHandler(c.eventHandler())
	networkInformer.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	etcdsInformer.Informer().AddEventHandler(c.eventHandler())
	masterNodeInformer.AddEventHandler(c.eventHandler())

	return c
}

func (c *EnvVarController) AddListener(listener Enqueueable) {
	c.listeners = append(c.listeners, listener)
}

func (c *EnvVarController) GetEnvVars() map[string]string {
	c.envVarMapLock.Lock()
	defer c.envVarMapLock.Unlock()

	ret := map[string]string{}
	maps.Copy(ret, c.envVarMap)
	return ret
}

func (c *EnvVarController) sync(ctx context.Context) error {
	// During bootstrap the config observation controller has not converged yet,
	// so servingInfo (cipherSuites, minTLSVersion) will be absent from the
	// observedConfig.  This is a transient state that resolves once the config
	// observer runs—not a degraded condition.  The bootstrap etcd member is
	// already configured via the render path (pkg/cmd/render/env.go) which
	// hardcodes TLSProfileIntermediateType defaults.
	//
	// We intentionally do NOT fall back to default ciphers inside
	// getCipherSuites() because at runtime an empty observedConfig would
	// indicate a real problem that should surface as degraded, not be
	// silently papered over with a potentially less-secure default set.
	operatorSpec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}
	hasCiphers, cipherErr := observedConfigHasCipherSuites(operatorSpec.ObservedConfig.Raw)
	if cipherErr != nil {
		// Malformed observedConfig or type error — this is a real problem, surface as degraded.
		return cipherErr
	}
	if !hasCiphers {
		klog.V(2).Infof("EnvVarController: observedConfig does not yet contain servingInfo.cipherSuites, waiting for config observer to converge")
		return fmt.Errorf("observedConfig has not yet converged: servingInfo.cipherSuites is missing")
	}

	err = c.checkEnvVars()
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

// observedConfigHasCipherSuites checks whether the observedConfig contains a
// non-empty servingInfo.cipherSuites list.  During bootstrap the config
// observation controller has not converged yet, so this field will be absent.
//
// Returns (false, nil) for empty/nil input or valid YAML that simply lacks
// the cipherSuites key—these are transient bootstrap conditions that resolve
// on their own.  Returns (false, error) for malformed YAML or type errors so
// that the caller can surface them as degraded.
func observedConfigHasCipherSuites(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var observedConfig map[string]any
	if err := yaml.Unmarshal(raw, &observedConfig); err != nil {
		return false, fmt.Errorf("failed to parse observedConfig: %w", err)
	}
	cipherSuites, found, err := unstructured.NestedStringSlice(observedConfig, "servingInfo", "cipherSuites")
	if err != nil {
		return false, fmt.Errorf("observedConfig servingInfo.cipherSuites has wrong type: %w", err)
	}
	return found && len(cipherSuites) > 0, nil
}

func (c *EnvVarController) checkEnvVars() error {
	operatorSpec, operatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	currEnvVarMap, err := getEtcdEnvVars(envVarContext{
		targetImagePullSpec:     c.targetImagePullSpec,
		spec:                    *operatorSpec,
		status:                  *operatorStatus,
		configmapLister:         c.configmapLister,
		masterNodeLister:        c.masterNodeLister,
		masterNodeLabelSelector: c.masterNodeLabelSelector,
		infrastructureLister:    c.infrastructureLister,
		networkLister:           c.networkLister,
		etcdLister:              c.etcdLister,
		featureGateAccessor:     c.featureGateAccessor,
	})
	if err != nil {
		return err
	}

	err = c.validateEnvVars(currEnvVarMap)
	if err != nil {
		return err
	}

	updated := false
	func() {
		c.envVarMapLock.Lock()
		defer c.envVarMapLock.Unlock()

		if !reflect.DeepEqual(c.envVarMap, currEnvVarMap) {
			c.envVarMap = currEnvVarMap
			updated = true
		}
	}()

	if !updated {
		return nil
	}

	// update listeners outside the lock in-case they are synchronously retrieving via GetEnvVars within the listener
	for _, listener := range c.listeners {
		listener.Enqueue()
	}

	return nil
}

func (c *EnvVarController) validateEnvVars(varMap map[string]string) error {
	// Below ensures that the certificates for all the configured nodes exist in the bundle configmap already.
	// Otherwise, this controller might render a new pod/revision faster than the CertSignerController could generate
	// the dynamic certificates on vertical scaling.
	allSecrets, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(tlshelpers.EtcdAllCertsSecretName)
	if err != nil {
		return fmt.Errorf("EnvVarController: could not retrieve secret %s, err: %w", tlshelpers.EtcdAllCertsSecretName, err)
	}
	for k, v := range varMap {
		if strings.HasPrefix(k, "NODE_") && strings.HasSuffix(k, "_ETCD_NAME") {
			// it's enough to check for the serving cert, the CertSignerController takes care of
			// atomically creating all of them at the same time inside the bundle
			secretDataKey := fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode(v))
			if _, ok := allSecrets.Data[secretDataKey]; !ok {
				return fmt.Errorf("could not find serving cert for node [%s] and key [%s]", v, secretDataKey)
			}
		}
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
		AddFunc:    func(obj any) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new any) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj any) { c.queue.Add(workQueueKey) },
	}
}
