package bootstrapteardown

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1getters "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey              = "key"
	configMapName             = "config"
	ConditionBootstrapRemoved = "BootstrapRemoved"
	configMapKey              = "config.yaml"
)

type BootstrapTeardownController struct {
	operatorConfigClient        v1helpers.OperatorClient
	clusterMemberShipController *clustermembercontroller.ClusterMemberController

	etcdOperatorLister  operatorv1listers.EtcdLister
	kubeAPIServerLister operatorv1listers.KubeAPIServerLister
	configMapsGetter    corev1getters.ConfigMapsGetter

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

// TODO wire a triggering lister
func NewBootstrapTeardownController(
	operatorConfigClient v1helpers.OperatorClient,
	kubeClient *kubernetes.Clientset,
	clusterMemberShipController *clustermembercontroller.ClusterMemberController,

	operatorInformers operatorv1informers.SharedInformerFactory,

	eventRecorder events.Recorder,
) *BootstrapTeardownController {
	c := &BootstrapTeardownController{
		operatorConfigClient:        operatorConfigClient,
		clusterMemberShipController: clusterMemberShipController,

		etcdOperatorLister:  operatorInformers.Operator().V1().Etcds().Lister(),
		kubeAPIServerLister: operatorInformers.Operator().V1().KubeAPIServers().Lister(),
		configMapsGetter:    kubeClient.CoreV1(),

		cachesToSync: []cache.InformerSynced{
			operatorConfigClient.Informer().HasSynced,
			operatorInformers.Operator().V1().Etcds().Informer().HasSynced,
			operatorInformers.Operator().V1().KubeAPIServers().Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "BootstrapTeardownController"),
		eventRecorder: eventRecorder.WithComponentSuffix("cluster-member-controller"),
	}

	operatorInformers.Operator().V1().KubeAPIServers().Informer().AddEventHandler(c.eventHandler())
	operatorInformers.Operator().V1().Etcds().Informer().AddEventHandler(c.eventHandler())
	operatorConfigClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *BootstrapTeardownController) sync() error {
	err := c.removeBootstrap()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BootstrapTeardownDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("BootstrapTeardownErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "BootstrapTeardownDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}),
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    ConditionBootstrapRemoved,
			Status:  operatorv1.ConditionTrue,
			Reason:  "BootstrapNodeRemoved",
			Message: "Etcd operator has scaled",
		}))
	return updateErr
}

func (c *BootstrapTeardownController) removeBootstrap() error {
	currentEtcdOperator, err := c.etcdOperatorLister.Get("cluster")
	if err != nil {
		return err
	}
	if currentEtcdOperator.Spec.ManagementState != operatorv1.Managed {
		return nil
	}

	etcdReady, err := isEtcdAvailable(currentEtcdOperator)
	if err != nil {
		return err
	}

	if !etcdReady {
		klog.Infof("Still waiting for the cluster-etcd-operator to bootstrap...")
		return fmt.Errorf("waiting for cluster-etcd-operator to bootstrap")
	}

	kubeAPIServer, err := c.kubeAPIServerLister.Get("cluster")
	if err != nil {
		return err
	}

	kasReady := isKASReady(kubeAPIServer, c.configMapsGetter)
	if !kasReady {
		klog.Infof("Still waiting for the kube-apiserver to be ready...")
		return fmt.Errorf("waiting for kube-apiserver to be ready")
	}

	if !c.clusterMemberShipController.IsMember("etcd-bootstrap") {
		return nil
	}

	c.eventRecorder.Event("BootstrapTeardownController", "safe to remove bootstrap")
	if err := c.clusterMemberShipController.RemoveBootstrap(); err != nil {
		return err
	}

	// set bootstrap removed condition
	_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    ConditionBootstrapRemoved,
		Status:  operatorv1.ConditionTrue,
		Reason:  "BootstrapNodeRemoved",
		Message: "Etcd operator has scaled",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("BootstrapTeardownErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func isKASReady(kasOperator *operatorv1.KubeAPIServer, configMapsGetter corev1getters.ConfigMapsGetter) bool {
	revisionMap := map[int32]struct{}{}
	uniqueRevisions := []int32{}

	for _, nodeStatus := range kasOperator.Status.NodeStatuses {
		revision := nodeStatus.CurrentRevision
		if _, ok := revisionMap[revision]; !ok {
			revisionMap[revision] = struct{}{}
			uniqueRevisions = append(uniqueRevisions, revision)
		}
	}

	// For each revision, check that the configmap for that revision contains the
	// appropriate storageConfig
	for _, revision := range uniqueRevisions {
		configMapNameWithRevision := fmt.Sprintf("%s-%d", configMapName, revision)
		configMap, err := configMapsGetter.ConfigMaps("openshift-kube-apiserver").Get(configMapNameWithRevision, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("doneApiServer: error getting configmap: %#v", err)
			return false
		}
		if configMapHasRequiredValues(configMap) {
			// if any 1 kube-apiserver pod has more than 1
			klog.Info("kube-apiserver has required values")
			return true
		}
	}
	return false
}

type ConfigData struct {
	StorageConfig struct {
		Urls []string
	}
}

func configMapHasRequiredValues(configMap *corev1.ConfigMap) bool {
	config, ok := configMap.Data[configMapKey]
	if !ok {
		klog.Errorf("configMapHasRequiredValues: config.yaml key missing")
		return false
	}
	var configData ConfigData
	err := json.Unmarshal([]byte(config), &configData)
	if err != nil {
		klog.Errorf("configMapHasRequiredValues: error unmarshalling configmap data: %#v", err)
		return false
	}
	if len(configData.StorageConfig.Urls) == 0 {
		klog.Infof("configMapHasRequiredValues: length of storageUrls %#v is 0", configData.StorageConfig.Urls)
		return false
	}
	if len(configData.StorageConfig.Urls) == 1 &&
		!strings.Contains(configData.StorageConfig.Urls[0], "etcd") {
		klog.Infof("configMapHasRequiredValues: config %s/%s has a single IP: %#v", configMap.Namespace, configMap.Name, strings.Join(configData.StorageConfig.Urls, ", "))
		return false
	}
	return true
}

func (c *BootstrapTeardownController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting BootstrapTeardownController")
	defer klog.Infof("Shutting down BootstrapTeardownController")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	// add time based trigger
	go wait.PollImmediateUntil(time.Minute, func() (bool, error) {
		c.queue.Add(workQueueKey)
		return false, nil
	}, stopCh)

	<-stopCh
}

// this function checks if etcd has scaled the initial bootstrap cluster
// with all 3 masters
func isEtcdAvailable(currentEtcdOperator *operatorv1.Etcd) (bool, error) {
	if currentEtcdOperator.Spec.ManagementState == operatorv1.Unmanaged {
		klog.Info("Cluster etcd operator is in Unmanaged mode")
		return true, nil
	}
	if operatorv1helpers.IsOperatorConditionTrue(currentEtcdOperator.Status.Conditions, clustermembercontroller.ConditionBootstrapSafeToRemove) {
		klog.Info("Cluster etcd operator bootstrapped successfully")
		return true, nil
	}
	klog.Infof("Still waiting for the cluster-etcd-operator to bootstrap")
	return false, nil
}

func (c *BootstrapTeardownController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *BootstrapTeardownController) processNextWorkItem() bool {
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
func (c *BootstrapTeardownController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
