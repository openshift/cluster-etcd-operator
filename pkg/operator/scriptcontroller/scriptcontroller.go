package scriptcontroller

import (
	"fmt"
	"path/filepath"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const workQueueKey = "key"

type ScriptController struct {
	operatorClient v1helpers.StaticPodOperatorClient

	kubeClient      kubernetes.Interface
	configMapLister corev1listers.ConfigMapLister
	eventRecorder   events.Recorder

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue        workqueue.RateLimitingInterface
	cachesToSync []cache.InformerSynced
}

func NewScriptControllerController(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
) *ScriptController {
	c := &ScriptController{
		operatorClient:  operatorClient,
		kubeClient:      kubeClient,
		configMapLister: kubeInformersForNamespaces.ConfigMapLister(),
		eventRecorder:   eventRecorder.WithComponentSuffix("target-config-controller"),

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ScriptControllerController"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
		},
	}

	operatorClient.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c ScriptController) sync() error {
	err := c.createScriptConfigMap()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ScriptControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ScriptControllerErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "ScriptControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("ScriptControllerErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}

	return nil
}

// createScriptController takes care of creation of valid resources in a fixed name.  These are inputs to other control loops.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func (c *ScriptController) createScriptConfigMap() error {
	errors := []error{}

	_, _, err := c.manageScriptConfigMap()
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/etcd-pod", err))
	}

	return utilerrors.NewAggregate(errors)
}

func (c *ScriptController) manageScriptConfigMap() (*corev1.ConfigMap, bool, error) {
	scriptConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/scripts-cm.yaml"))
	// TODO get the env vars to produce a file that we write

	for _, filename := range []string{
		"etcd/etcd-restory-backup.sh",
	} {
		basename := filepath.Base(filename)
		scriptConfigMap.Data[basename] = string(etcd_assets.MustAsset(filename))
	}
	return resourceapply.ApplyConfigMap(c.kubeClient.CoreV1(), c.eventRecorder, scriptConfigMap)
}

// Run starts the etcd and blocks until stopCh is closed.
func (c *ScriptController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ScriptControllerController")
	defer klog.Infof("Shutting down ScriptControllerController")

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

func (c *ScriptController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ScriptController) processNextWorkItem() bool {
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
func (c *ScriptController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
