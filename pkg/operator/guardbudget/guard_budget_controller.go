package guardbudget

import (
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	policylisters "k8s.io/client-go/listers/policy/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey                = "key"
	configMapName               = "config"
	configMapKey                = "config.yaml"
	conditionQuorumGuardRemoved = "QuorumGuardRemoved"
)

type GuardBudgetController struct {
	operatorClient v1helpers.OperatorClient
	kubeClient     kubernetes.Interface

	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces

	machineConfigOperatorPodDisruptionBudgetLister policylisters.PodDisruptionBudgetLister
	etcdPodDisruptionBudgetLister                  policylisters.PodDisruptionBudgetLister
	machineConfigOperatorDeploymentLister          appsv1listers.DeploymentLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewGuardBudgetController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
) *GuardBudgetController {
	openshiftMachineConfigOperatorNamespacedInformers := kubeInformersForNamespaces.InformersFor("openshift-machine-config-operator")
	openshiftEtcdNamespacedInformers := kubeInformersForNamespaces.InformersFor("openshift-etcd")
	c := &GuardBudgetController{
		operatorClient: operatorClient,
		kubeClient:     kubeClient,
		machineConfigOperatorPodDisruptionBudgetLister: openshiftMachineConfigOperatorNamespacedInformers.Policy().V1beta1().PodDisruptionBudgets().Lister(),
		machineConfigOperatorDeploymentLister:          openshiftMachineConfigOperatorNamespacedInformers.Apps().V1().Deployments().Lister(),
		etcdPodDisruptionBudgetLister:                  openshiftEtcdNamespacedInformers.Policy().V1beta1().PodDisruptionBudgets().Lister(),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("openshift-machine-config-operator").Policy().V1beta1().PodDisruptionBudgets().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("openshift-machine-config-operator").Apps().V1().Deployments().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("openshift-etcd").Policy().V1beta1().PodDisruptionBudgets().Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "GuardBudgetController"),
		eventRecorder: eventRecorder.WithComponentSuffix("guard-budget-controller"),
	}

	kubeInformersForNamespaces.InformersFor("openshift-machine-config-operator").Policy().V1beta1().PodDisruptionBudgets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor("openshift-etcd").Policy().V1beta1().PodDisruptionBudgets().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForNamespaces.InformersFor("openshift-machine-config-operator").Apps().V1().Deployments().Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *GuardBudgetController) sync() error {
	// nerf this controller for now
	if true {
		return nil
	}

	err := c.checkGuardBudget()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "GuardBudgetDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("GuardBudgetErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "GuardBudgetDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *GuardBudgetController) checkGuardBudget() error {
	// checks if quorum-guard pdb exists in mco namespace
	machineConfigOperatorPDB, err := c.machineConfigOperatorPodDisruptionBudgetLister.PodDisruptionBudgets("openshift-machine-config-operator").Get("etcd-quorum-guard")
	if errors.IsNotFound(err) {
		// removed condition
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    conditionQuorumGuardRemoved,
			Status:  operatorv1.ConditionTrue,
			Reason:  "GuardBudgetRemoved",
			Message: "Quorum guard pod disruption budget has been removed from MCO namespace",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("GuardBudgetErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
		// return because no work left to do
		return nil
	}
	if err != nil {
		return err
	}

	_, _, _ = v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    conditionQuorumGuardRemoved,
		Status:  operatorv1.ConditionFalse,
		Reason:  "GuardBudgetNotRemoved",
		Message: fmt.Sprintf("Quorum guard pod disruption budget is not removed yet: machineConfigOperatorPDB %#v", machineConfigOperatorPDB),
	}))

	pdbErr := c.kubeClient.PolicyV1beta1().PodDisruptionBudgets("openshift-machine-config-operator").Delete("etcd-quorum-guard", nil)
	if errors.IsNotFound(pdbErr) {
		c.eventRecorder.Event("GuardBudgetController", "pdb etcd-quorum-guard does not exist")
	}
	if pdbErr != nil {
		c.eventRecorder.Event("GuardBudgetController", "failed to remove guard budget")
	}

	depoyErr := c.kubeClient.AppsV1().Deployments("openshift-machine-config-operator").Delete("etcd-quorum-guard", nil)
	if errors.IsNotFound(depoyErr) {
		c.eventRecorder.Event("GuardBudgetController", "deployment etcd-quorum-guard does not exist")
	}
	if depoyErr != nil {
		c.eventRecorder.Event("GuardBudgetController", "failed to remove quorum-guard deployment")
	}

	return nil
}

func (c *GuardBudgetController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting GuardBudgetController")
	defer klog.Infof("Shutting down GuardBudgetController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		return
	}
	klog.V(2).Infof("caches synced GuardBudgetController")

	go wait.Until(c.runWorker, time.Second, stopCh)

	// add time based trigger
	go wait.PollImmediateUntil(time.Minute, func() (bool, error) {
		c.queue.Add(workQueueKey)
		return false, nil
	}, stopCh)

	<-stopCh
}

func (c *GuardBudgetController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *GuardBudgetController) processNextWorkItem() bool {
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
func (c *GuardBudgetController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
