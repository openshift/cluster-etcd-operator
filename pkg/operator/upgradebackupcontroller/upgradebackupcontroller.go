package upgradebackupcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	configv1helpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/mcuadros/go-version"
)

// UpgradeBackupController responds to an upgrade request to 4.9 by attempting
// to ensure the cluster is backed up. The CVO is expected to wait on a ceo
// condition indicating successful backup before responding to the upgrade
// request. This is intended to ensure that an upgrade from 4.8 to 4.9 does not
// proceed without a recent backup to restore a cluster that is not healthy after
// upgrade to 4.9.

const (
	backupConditionType      = "RecentBackup"
	backupSuccess            = "UpgradeBackupSuccessful"
	clusterBackupPodName     = "cluster-backup"
	recentBackupPath         = "/etc/kubernetes/cluster-backup"
	failedPodBackoffDuration = 30 * time.Second
	backupDirEnvName         = "CLUSTER_BACKUP_PATH"
)

type UpgradeBackupController struct {
	operatorClient        v1helpers.OperatorClient
	clusterOperatorClient configv1client.ClusterOperatorsGetter
	kubeClient            kubernetes.Interface
	etcdClient            etcdcli.EtcdClient
	podLister             corev1listers.PodLister
	clusterVersionLister  configv1listers.ClusterVersionLister
	clusterOperatorLister configv1listers.ClusterOperatorLister
	targetImagePullSpec   string
	operatorImagePullSpec string
}

func NewUpgradeBackupController(
	operatorClient v1helpers.OperatorClient,
	clusterOperatorClient configv1client.ClusterOperatorsGetter,
	kubeClient kubernetes.Interface,
	etcdClient etcdcli.EtcdClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	clusterVersionInformer configv1informers.ClusterVersionInformer,
	clusterOperatorInformer configv1informers.ClusterOperatorInformer,
	eventRecorder events.Recorder,
	targetImagePullSpec string,
	operatorImagePullSpec string,
) factory.Controller {
	c := &UpgradeBackupController{
		operatorClient:        operatorClient,
		clusterOperatorClient: clusterOperatorClient,
		clusterOperatorLister: clusterOperatorInformer.Lister(),
		kubeClient:            kubeClient,
		etcdClient:            etcdClient,
		podLister:             kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		clusterVersionLister:  clusterVersionInformer.Lister(),
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,
	}
	return factory.New().ResyncEvery(1*time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		operatorClient.Informer(),
		clusterVersionInformer.Informer(),
		clusterOperatorInformer.Informer(),
	).WithSync(c.sync).ToController("ClusterBackupController", eventRecorder.WithComponentSuffix("cluster-backup-controller"))
}

func (c *UpgradeBackupController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	originalClusterOperatorObj, err := c.clusterOperatorLister.Get("etcd")
	if err != nil && !apierrors.IsNotFound(err) {
		syncCtx.Recorder().Warningf("StatusFailed", "Unable to get current operator status for clusteroperator/etcd: %v", err)
		return err
	}
	if err != nil {
		return err
	}

	clusterOperatorObj := originalClusterOperatorObj.DeepCopy()
	if !isBackupConditionExist(originalClusterOperatorObj.Status.Conditions) {
		configv1helpers.SetStatusCondition(&clusterOperatorObj.Status.Conditions, configv1.ClusterOperatorStatusCondition{
			Type:   "RecentBackup",
			Status: configv1.ConditionUnknown,
			Reason: "ControllerStarted",
		})
		if _, err := c.clusterOperatorClient.ClusterOperators().UpdateStatus(ctx, clusterOperatorObj, metav1.UpdateOptions{}); err != nil {
			syncCtx.Recorder().Warning("ClusterBackupControllerUpdatingStatus", err.Error())
			return err
		}
	}

	recentBackupCondition, err := c.ensureRecentBackup(ctx, &clusterOperatorObj.Status, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "UpgradeBackupControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("ClusterBackupControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}
	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "UpgradeBackupControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}))
	if updateErr != nil {
		syncCtx.Recorder().Warning("ClusterBackupControllerUpdatingStatus", updateErr.Error())
		return updateErr
	}

	// If recentBackupCondition is nil work is done and no need to update status.
	if recentBackupCondition == nil {
		return nil
	}

	configv1helpers.SetStatusCondition(&clusterOperatorObj.Status.Conditions, *recentBackupCondition)
	if _, err := c.clusterOperatorClient.ClusterOperators().UpdateStatus(ctx, clusterOperatorObj, metav1.UpdateOptions{}); err != nil {
		syncCtx.Recorder().Warning("ClusterBackupControllerUpdatingStatus", err.Error())
		return err
	}

	return nil
}

// ensureRecentBackup ensures that a new backup pod is created if one does not exist, returns the appropriate RecentBackup condition.
func (c *UpgradeBackupController) ensureRecentBackup(ctx context.Context, clusterOperatorStatus *configv1.ClusterOperatorStatus, recorder events.Recorder) (*configv1.ClusterOperatorStatusCondition, error) {
	clusterVersion, err := c.clusterVersionLister.Get("version")
	if err != nil {
		return nil, err
	}

	currentVersion, err := getCurrentClusterVersion(clusterVersion)
	if err != nil {
		return nil, err
	}

	// Check cluster version status for backup condition.
	if !isRequireRecentBackup(clusterVersion, clusterOperatorStatus) {
		return nil, nil
	}

	backupPod, err := c.podLister.Pods(operatorclient.TargetNamespace).Get(clusterBackupPodName)
	// No backup found, attempt to create one.
	if err != nil && apierrors.IsNotFound(err) {
		// Check nodes for backup preconditions
		backupNodeName, err := c.getBackupNodeName(ctx)
		if err != nil {
			return nil, err
		}

		recentBackupName := fmt.Sprintf("upgrade-backup-%s-%s", currentVersion, time.Now().Format("2006-01-02_150405"))

		if err := createBackupPod(ctx, backupNodeName, recentBackupName, c.operatorImagePullSpec, c.targetImagePullSpec, c.kubeClient.CoreV1(), recorder); err != nil {
			return nil, fmt.Errorf("pod/%s: %v", clusterBackupPodName, err)
		}

		return &configv1.ClusterOperatorStatusCondition{
			Status:  configv1.ConditionUnknown,
			Type:    backupConditionType,
			Reason:  "UpgradeBackupInProgress",
			Message: fmt.Sprintf("Upgrade backup pod created on node %s", backupNodeName),
		}, nil
	}
	if err != nil {
		return nil, err
	}

	// Check the existing Pod Status, reconcile pod and update condition respectively.
	// PodSucceeded => ConditionTrue
	// PodFailed =>  ConditionFalse
	// everything else ConditionUnknown
	switch {
	case backupPod.Status.Phase == corev1.PodSucceeded:
		backupCondition := configv1helpers.FindStatusCondition(clusterOperatorStatus.Conditions, backupConditionType)
		if backupCondition.Reason != backupSuccess {

			// Read the backup directory name from the backup pod's env
			backupDirName := ""
			for _, env := range backupPod.Spec.Containers[0].Env {
				if env.Name == backupDirEnvName {
					backupDirName = env.Value
				}
			}
			if backupDirName == "" {
				klog.Warningf("failed to get backup directory name from backup pod: %s/%s", backupPod.Namespace, backupPod.Name)
			}

			return &configv1.ClusterOperatorStatusCondition{
				Status:  configv1.ConditionTrue,
				Type:    backupConditionType,
				Reason:  backupSuccess,
				Message: fmt.Sprintf("UpgradeBackup for %v is located at path %s on node %q", currentVersion, backupDirName, backupPod.Spec.NodeName),
			}, nil
		}
		return nil, nil
	case backupPod.Status.Phase == corev1.PodFailed && !isBackoffDuration(backupPod.CreationTimestamp.Time, failedPodBackoffDuration):
		// Pod must be older than retry duration before any delete action is taken.
		return operatorStatusBackupPodFailed("pod failed within retry duration: delete skipped"), nil
	case backupPod.Status.Phase == corev1.PodFailed:
		// Delete pod
		err := c.kubeClient.CoreV1().Pods(operatorclient.TargetNamespace).Delete(ctx, backupPod.Name, metav1.DeleteOptions{})
		if err != nil {
			return operatorStatusBackupPodFailed(fmt.Sprintf("failed to delete pod openshift-etcd/%s: %v", clusterBackupPodName, err)), nil
		}
		podDeletedMessage := fmt.Sprintf("Successful deletion of failed pod openshift-etcd/%s", clusterBackupPodName)
		recorder.Event("UpgradeBackupFailed", podDeletedMessage)
		return operatorStatusBackupPodFailed(podDeletedMessage), nil
	}

	return &configv1.ClusterOperatorStatusCondition{
		Status:  configv1.ConditionUnknown,
		Type:    backupConditionType,
		Reason:  "UpgradeBackupInProgress",
		Message: fmt.Sprintf("Backup pod phase: %q", backupPod.Status.Phase),
	}, nil
}

// getBackupNodeName checks all etcd pods and verifies the etcd member on that
// pod is healthy if yes returns the node name it is scheduled on and errors if
// no healthy members exist.
func (c *UpgradeBackupController) getBackupNodeName(ctx context.Context) (string, error) {
	pods, err := c.podLister.List(labels.Set{"app": "etcd"}.AsSelector())
	if err != nil {
		return "", err
	}

	var errs []error
	for _, pod := range pods {
		if !strings.HasPrefix(pod.Name, "etcd-") {
			continue
		}
		member, err := c.etcdClient.GetMember(ctx, pod.Spec.NodeName)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		isHealthy, err := c.etcdClient.IsMemberHealthy(ctx, member)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if isHealthy {
			return pod.Spec.NodeName, nil
		}
		klog.V(4).Infof("etcd member failed health check: %s", pod.Spec.NodeName)
	}

	if len(errs) > 0 {
		return "", utilerrors.NewAggregate(errs)
	}

	return "", fmt.Errorf("no valid node found to schedule backup pod")
}

func createBackupPod(ctx context.Context, nodeName, recentBackupName, operatorImagePullSpec, targetImagePullSpec string, client coreclientv1.PodsGetter, recorder events.Recorder) error {
	pod := resourceread.ReadPodV1OrDie(etcd_assets.MustAsset("etcd/cluster-backup-pod.yaml"))
	pod.Spec.NodeName = nodeName
	pod.Spec.InitContainers[0].Image = operatorImagePullSpec
	pod.Spec.Containers[0].Image = targetImagePullSpec
	pod.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: backupDirEnvName, Value: fmt.Sprintf("%s/%s", recentBackupPath, recentBackupName)},
	}
	_, _, err := resourceapply.ApplyPod(ctx, client, recorder, pod)
	if err != nil {
		return err
	}

	return nil
}

// isRequireRecentBackup checks the conditions of ClusterVersion to verify if a backup is required.
func isRequireRecentBackup(config *configv1.ClusterVersion, clusterOperatorStatus *configv1.ClusterOperatorStatus) bool {
	for _, condition := range config.Status.Conditions {
		// Check if ReleaseAccepted is false and Message field containers the string RecentBackup.
		if condition.Type == "ReleaseAccepted" && condition.Status == configv1.ConditionFalse {
			if strings.Contains(condition.Message, backupConditionType) {
				return true
			}
		}
	}

	// consecutive upgrades case
	if backupRequired, err := isNewBackupRequired(config, clusterOperatorStatus); err == nil && backupRequired {
		return true
	}
	return false
}

// isBackoffDuration returns true if the pod is older than backoff duration.
func isBackoffDuration(createdTimeStamp time.Time, duration time.Duration) bool {
	return metav1.Now().Add(-duration).After(createdTimeStamp)
}

func operatorStatusBackupPodFailed(message string) *configv1.ClusterOperatorStatusCondition {
	return &configv1.ClusterOperatorStatusCondition{
		Status:  configv1.ConditionFalse,
		Type:    backupConditionType,
		Reason:  "UpgradeBackupFailed",
		Message: message,
	}
}

func isBackupConditionExist(conditions []configv1.ClusterOperatorStatusCondition) bool {
	for _, condition := range conditions {
		if condition.Type == backupConditionType {
			return true
		}
	}
	return false
}

func getCurrentClusterVersion(cv *configv1.ClusterVersion) (string, error) {
	if cv == nil {
		return "", fmt.Errorf("ClusterVersion type is nil: %v", cv)
	}
	for _, c := range cv.Status.History {
		if c.State == configv1.CompletedUpdate {
			return c.Version, nil
		}
	}
	return "", fmt.Errorf("unable to retrieve cluster version, no completed update was found in status history: %v", cv.Status.History)
}

func isNewBackupRequired(config *configv1.ClusterVersion, clusterOperatorStatus *configv1.ClusterOperatorStatus) (bool, error) {
	currentVersion, err := getCurrentClusterVersion(config)
	if err != nil {
		return false, err
	}

	// check in ClusterVersion update history for update in progress (i.e. state: Partial)
	// then check the condition of etcd operator
	// if backup for current cluster version was taken, return false
	// otherwise, if the update version greater than current version, return true
	for _, update := range config.Status.History {
		if update.State == configv1.PartialUpdate {
			klog.V(4).Infof("in progress update is detected, cluster current version is %v, progressing towards cluster version %v", currentVersion, update.Version)
			currentBackupVersion := getCurrentBackupVersion(clusterOperatorStatus)
			if currentBackupVersion == "" || currentBackupVersion == currentVersion {
				return false, nil
			}
			if cmp := version.CompareSimple(update.Version, currentVersion); cmp > 0 {
				return true, nil
			}
		}
	}

	return false, nil
}

func getCurrentBackupVersion(clusterOperatorStatus *configv1.ClusterOperatorStatus) string {
	backupCondition := configv1helpers.FindStatusCondition(clusterOperatorStatus.Conditions, backupConditionType)
	if backupCondition == nil {
		return ""
	}
	if backupCondition.Reason == backupSuccess {
		backupVersion := strings.Fields(backupCondition.Message)[2]
		return backupVersion
	}
	return ""
}
