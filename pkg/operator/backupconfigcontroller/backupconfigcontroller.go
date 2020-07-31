package backupconfigcontroller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/revisioncontroller"

	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/resources/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/resources/kubeapiserver"
	"github.com/openshift/cluster-etcd-operator/pkg/resources/kubecontrollermanager"
	"github.com/openshift/cluster-etcd-operator/pkg/resources/kubescheduler"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	HostResourceDirDir = "/etc/kubernetes/static-pod-resources"
	HostPodManifestDir = "/tmp/manifests" // we dont want the actual manifests
	YamlPad            = "      "
)

var ControlPlaneOperators = []string{"etcd", "kubeAPIServer", "kubeControllerManager", "kubeScheduler"}

type BackupConfigController struct {
	operatorImagePullSpec string
	operatorClient        v1helpers.StaticPodOperatorClient
	operatorV1Client      *operatorv1client.OperatorV1Client
	configMapLister       corev1listers.ConfigMapLister
	kubeClient            kubernetes.Interface
	enqueueFn             func()
}

func NewBackupConfigController(
	operatorImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	operatorV1Client *operatorv1client.OperatorV1Client,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &BackupConfigController{
		operatorImagePullSpec: operatorImagePullSpec,
		operatorClient:        operatorClient,
		operatorV1Client:      operatorV1Client,
		kubeClient:            kubeClient,
		configMapLister:       kubeInformersForNamespaces.ConfigMapLister(),
	}

	syncCtx := factory.NewSyncContext("BackupConfigController", eventRecorder.WithComponentSuffix("backup-config-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}

	return factory.New().WithSyncContext(syncCtx).ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-kube-apiserver").Core().V1().ConfigMaps().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-kube-controller-manager").Core().V1().ConfigMaps().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-kube-scheduler").Core().V1().ConfigMaps().Informer(),
	).WithSync(c.sync).ToController("BackupConfigController", syncCtx.Recorder())
}

func (c BackupConfigController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}
	requeue, err := createBackupConfig(c, syncCtx.Recorder(), operatorSpec)
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "BackupConfigControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("BackupConfigControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	if requeue {
		return fmt.Errorf("synthetic requeue request")
	}
	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "BackupConfigControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c BackupConfigController) getLatestOperatorStatusRevisions() (map[string]string, error) {
	revisionMap := make(map[string]string)

	kubeAPIServerOperator, err := c.operatorV1Client.KubeAPIServers().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getLatestRevisions failed: %w", err)
	}
	kubeSchedulerOperator, err := c.operatorV1Client.KubeSchedulers().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getLatestRevisions failed: %w", err)
	}
	etcdOperator, err := c.operatorV1Client.Etcds().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getLatestRevisions failed: %w", err)
	}
	kubeControllerManagerOperator, err := c.operatorV1Client.KubeControllerManagers().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getLatestRevisions failed: %w", err)
	}

	revisionMap["kubeScheduler"] = fmt.Sprint(kubeSchedulerOperator.Status.LatestAvailableRevision)
	revisionMap["kubeAPIServer"] = fmt.Sprint(kubeAPIServerOperator.Status.LatestAvailableRevision)
	revisionMap["etcd"] = fmt.Sprint(etcdOperator.Status.LatestAvailableRevision)
	revisionMap["kubeControllerManager"] = fmt.Sprint(kubeControllerManagerOperator.Status.LatestAvailableRevision)

	return revisionMap, nil
}

func (c BackupConfigController) getLatestBackupRevisions(recorder events.Recorder) (map[string]string, error) {
	if err := c.ensureBackupRevisionsConfigMap(recorder); err != nil {
		return nil, err
	}

	config, err := c.configMapLister.ConfigMaps(operatorclient.OperatorNamespace).Get("backup-revisions")
	if err != nil {
		return nil, err
	}

	revisionMap := make(map[string]string)
	for _, operator := range ControlPlaneOperators {
		revisionMap[operator] = config.Data[operator]
	}

	return revisionMap, nil
}

// ensureBackupRevisionsConfigMap ensures the backup-revisions configmap exists. If it doesn't create it with default values.
func (c BackupConfigController) ensureBackupRevisionsConfigMap(recorder events.Recorder) error {
	_, err := c.configMapLister.ConfigMaps(operatorclient.OperatorNamespace).Get("backup-revisions")
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("couldn't get configmap %s/%s: %w", operatorclient.OperatorNamespace, "backup-revisions", err)
	}

	if errors.IsNotFound(err) {
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "backup-revisions",
				Namespace:   operatorclient.OperatorNamespace,
				Annotations: map[string]string{},
			},
			Data: map[string]string{
				"etcd":                  "0",
				"kubeAPIServer":         "0",
				"kubeControllerManager": "0",
				"kubeScheduler":         "0",
			},
		}
		_, _, err := resourceapply.ApplyConfigMap(c.kubeClient.CoreV1(), recorder, configMap)
		if err != nil {
			return fmt.Errorf("couldn't create configmap %s/%s: %w", operatorclient.OperatorNamespace, "backup-revisions", err)
		}
	}
	return nil
}

func createBackupConfig(c BackupConfigController, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (bool, error) {
	errors := []error{}

	operatorRevisionMap, err := c.getLatestOperatorStatusRevisions()
	if err != nil {
		return false, err
	}

	backupPod := c.ensureBackupPod(c.operatorImagePullSpec, operatorRevisionMap)
	_, _, err = c.ensureBackupPodConfigMap(operatorRevisionMap, backupPod, c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/backup-pod", err))
	}

	_, _, err = c.manageBackupRevisionsConfigMap(operatorRevisionMap, c.kubeClient.CoreV1(), recorder)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/backup-revisions", err))
	}

	if len(errors) > 0 {
		condition := operatorv1.OperatorCondition{
			Type:    "BackupConfigControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SynchronizationError",
			Message: v1helpers.NewMultiLineAggregate(errors).Error(),
		}
		if _, _, err := v1helpers.UpdateStaticPodStatus(c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
			return true, err
		}
		return true, nil
	}

	return false, nil
}

// ensureBackupPod creates and populates a backup-pod
func (c *BackupConfigController) ensureBackupPod(imagePullSpec string, revisionMap map[string]string) *corev1.Pod {
	pod := resourceread.ReadPodV1OrDie(etcd_assets.MustAsset("etcd/backup-pod.yaml"))

	etcdStaticPodInstallerArgs := getStaticPodInstallerArgs(
		"etcd",
		revisionMap["etcd"],
		etcd.RevisionConfigMaps,
		etcd.RevisionSecrets,
		etcd.CertConfigMaps,
		etcd.CertSecrets,
	)
	pod.Spec.InitContainers[1].Args = etcdStaticPodInstallerArgs
	pod.Spec.InitContainers[1].Image = imagePullSpec

	kubeAPIServerStaticPodInstallerArgs := getStaticPodInstallerArgs(
		"kube-apiserver",
		revisionMap["kubeAPIServer"],
		kubeapiserver.RevisionConfigMaps,
		kubeapiserver.RevisionSecrets,
		kubeapiserver.CertConfigMaps,
		kubeapiserver.CertSecrets,
	)
	pod.Spec.InitContainers[2].Args = kubeAPIServerStaticPodInstallerArgs
	pod.Spec.InitContainers[2].Image = imagePullSpec

	kubeControllerManagerStaticPodInstallerArgs := getStaticPodInstallerArgs(
		"kube-controller-manager",
		revisionMap["kubeControllerManager"],
		kubecontrollermanager.RevisionConfigMaps,
		kubecontrollermanager.RevisionSecrets,
		kubecontrollermanager.CertConfigMaps,
		kubecontrollermanager.CertSecrets,
	)
	pod.Spec.InitContainers[3].Args = kubeControllerManagerStaticPodInstallerArgs
	pod.Spec.InitContainers[3].Image = imagePullSpec

	kubeSchedulerStaticPodInstallerArgs := getStaticPodInstallerArgs(
		"kube-scheduler",
		revisionMap["kubeScheduler"],
		kubescheduler.RevisionConfigMaps,
		kubescheduler.RevisionSecrets,
		kubescheduler.CertConfigMaps,
		kubescheduler.CertSecrets,
	)
	pod.Spec.InitContainers[4].Args = kubeSchedulerStaticPodInstallerArgs
	pod.Spec.InitContainers[4].Image = imagePullSpec

	return pod
}

func getStaticPodInstallerArgs(operand, revision string, secrets, configMaps, certSecrets, certConfigMaps []revisioncontroller.RevisionResource) []string {
	args := []string{
		fmt.Sprintf("-v=%d", 2),
		fmt.Sprintf("--revision=%s", revision),
		fmt.Sprintf("--namespace=%s", fmt.Sprintf("openshift-%s", operand)),
		fmt.Sprintf("--pod=%s", fmt.Sprintf("%s-pod", operand)),
		fmt.Sprintf("--resource-dir=%s", HostResourceDirDir),
		fmt.Sprintf("--pod-manifest-dir=%s", HostPodManifestDir),
	}
	for _, cm := range configMaps {
		if cm.Optional {
			args = append(args, fmt.Sprintf("--optional-configmaps=%s", cm.Name))
		} else {
			args = append(args, fmt.Sprintf("--configmaps=%s", cm.Name))
		}
	}
	for _, s := range secrets {
		if s.Optional {
			args = append(args, fmt.Sprintf("--optional-secrets=%s", s.Name))
		} else {
			args = append(args, fmt.Sprintf("--secrets=%s", s.Name))
		}
	}
	for _, cm := range certConfigMaps {
		if cm.Optional {
			args = append(args, fmt.Sprintf("--optional-cert-configmaps=%s", cm.Name))
		} else {
			args = append(args, fmt.Sprintf("--cert-configmaps=%s", cm.Name))
		}
	}
	for _, s := range certSecrets {
		if s.Optional {
			args = append(args, fmt.Sprintf("--optional-cert-secrets=%s", s.Name))
		} else {
			args = append(args, fmt.Sprintf("--cert-secrets=%s", s.Name))
		}
	}
	return args
}

func (c *BackupConfigController) ensureBackupPodConfigMap(operatorRevisionMap map[string]string, backupPod *v1.Pod, client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	var podByte []byte
	backupPod.Unmarshal(podByte)

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/backup-pod-cm.yaml"))
	podConfigMap.Data["backup-pod.yaml"] = string(podByte)
	return resourceapply.ApplyConfigMap(client, recorder, podConfigMap)
}

func (c *BackupConfigController) manageBackupRevisionsConfigMap(revisionMap map[string]string, client coreclientv1.ConfigMapsGetter, recorder events.Recorder) (*corev1.ConfigMap, bool, error) {
	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/backup-revisions-cm.yaml"))
	for _, operator := range ControlPlaneOperators {
		podConfigMap.Data[operator] = revisionMap[operator]
	}
	return resourceapply.ApplyConfigMap(client, recorder, podConfigMap)
}

func (c *BackupConfigController) Enqueue() {
	c.enqueueFn()
}

func (c *BackupConfigController) namespaceEventHandler() cache.ResourceEventHandler {
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
