package backupcontroller

import (
	"context"
	"fmt"
	"slices"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type BackupQueueController struct {
	backupsLister       operatorv1alpha1listers.EtcdBackupLister
	nodeLister          corev1listers.NodeLister
	operatorClient      operatorv1alpha1client.OperatorV1alpha1Interface
	featureGateAccessor featuregates.FeatureGateAccess
	activeCache         activeBackupCache
}

func NewBackupQueueController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsLister operatorv1alpha1listers.EtcdBackupLister,
	nodeLister corev1listers.NodeLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	eventRecorder events.Recorder,
	accessor featuregates.FeatureGateAccess,
	backupInformer factory.Informer,
	nodeInformer cache.SharedIndexInformer) factory.Controller {

	c := &BackupQueueController{
		backupsLister:       backupsLister,
		nodeLister:          nodeLister,
		operatorClient:      operatorClient,
		featureGateAccessor: accessor,
		activeCache:         newActiveBackupCache(),
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupQueueController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithFilteredEventsInformers(func(obj interface{}) bool {
			if backup, ok := obj.(*operatorv1alpha1.EtcdBackup); ok {
				// Don't trigger sync for backups marked pending or running
				return !backuphelpers.IsBackupActive(backup)
			}
			return false
		}, backupInformer).
		WithSync(syncer.Sync).
		ToController("BackupQueueController", eventRecorder.WithComponentSuffix("backup-queue-controller"))
}

func (c *BackupQueueController) sync(ctx context.Context, _ factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupController error while checking feature flags: %v", err)
		}
		return nil
	}

	backupsQueue, err := c.listBackupsQueue(ctx)
	if err != nil {
		return err
	} else if len(backupsQueue) == 0 {
		return nil
	}

	masterNodes, err := backuphelpers.SelectBackupNodes(c.nodeLister, nil)
	if err != nil {
		return fmt.Errorf("BackupPolicyController failed to select master nodes for backup: %w", err)
	}

	backupsClient := c.operatorClient.EtcdBackups()
	for _, backup := range backupsQueue {
		nodeName := backup.Spec.NodeName
		if nodeName == "" {
			// Round robin master nodes until all in use
			for len(masterNodes) > 0 {
				node := masterNodes[0]
				masterNodes = masterNodes[1:]
				if c.activeCache.nodeInUse(node.Name) {
					nodeName = node.Name
					break
				}
			}
			if nodeName == "" {
				klog.Infof("BackupQueueController unable to start backup [%s]: all master nodes in use", backup.Name)
				continue
			}
		}
		if ok, reason := c.activeCache.canStart(backup, nodeName); !ok {
			klog.Infof("BackupQueueController unable to start backup [%s]: %s", backup.Name, reason)
			continue
		}

		backup = backup.DeepCopy()
		backup.Status.NodeName = nodeName
		backup.Status.Conditions = append(backup.Status.Conditions, metav1.Condition{
			Type:               string(operatorv1alpha1.BackupPending),
			Reason:             string(operatorv1alpha1.BackupReasonReadyToStart),
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		})
		if _, err := backupsClient.UpdateStatus(ctx, backup, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				klog.Infof("BackupQueueController conflict updating backup [%s]: %s", backup.Name, err)
				continue
			}
			return fmt.Errorf("BackupQueueController failed to promote backup [%s] to pending: %w", backup.Name, err)
		}
		klog.Infof("BackupQueueController promoted backup [%s] to pending on node [%s]", backup.Name, nodeName)
		c.activeCache.add(backup)
	}

	return nil
}

func (c *BackupQueueController) listBackupsQueue(ctx context.Context) ([]*operatorv1alpha1.EtcdBackup, error) {
	backups, err := c.backupsLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("BackupQueueController failed to list backups: %w", err)
	}

	// Filter for backups that have not been started yet, and observe changes to active backups
	observedActive := newActiveBackupCache()
	n := 0
	for _, backup := range backups {
		if backup.DeletionTimestamp == nil && len(backup.Status.Conditions) == 0 {
			backups[n] = backup
			n++
		} else if !backuphelpers.IsBackupFinished(backup) {
			observedActive.add(backup)
			c.activeCache.add(backup)
		} else {
			c.activeCache.remove(backup.Status.NodeName, backup.Name)
		}
	}
	backups = backups[:n]
	slices.SortFunc(backups[:n], func(a, b *operatorv1alpha1.EtcdBackup) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})

	// Any items in activeBackups that weren't observed this sync are either new entries that haven't synced to the
	// informer cache yet, or no longer exist. Compare against source of truth (k8s API).
	backupsClient := c.operatorClient.EtcdBackups()
	for nodeName, activeBackups := range c.activeCache.backups {
		if observedBackups, exists := observedActive.backups[nodeName]; exists {
			for backupName := range activeBackups {
				if _, exists := observedBackups[backupName]; !exists {
					if exists, err := backupExists(ctx, backupsClient, backupName); err != nil {
						return nil, err
					} else if !exists {
						c.activeCache.remove(nodeName, backupName)
					}
				}
			}
		} else {
			for backupName := range activeBackups {
				if exists, err := backupExists(ctx, backupsClient, backupName); err != nil {
					return nil, err
				} else if !exists {
					c.activeCache.remove(nodeName, backupName)
				}
			}
		}
	}
	return backups, nil
}

func backupExists(ctx context.Context, backupsClient operatorv1alpha1client.EtcdBackupInterface, name string) (bool, error) {
	if _, err := backupsClient.Get(ctx, name, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return true, fmt.Errorf("BackupQueueController failed to get backup [%s]: %w", name, err)
		}

		return false, nil
	}
	return true, nil
}

func newActiveBackupCache() activeBackupCache {
	return activeBackupCache{
		backups: map[string]map[string]string{},
		pvcs:    map[string]struct{}{},
	}
}

type activeBackupCache struct {
	backups map[string]map[string]string
	pvcs    map[string]struct{}
}

func (abm activeBackupCache) add(backup *operatorv1alpha1.EtcdBackup) {
	backups, ok := abm.backups[backup.Status.NodeName]
	if !ok {
		backups = map[string]string{}
		abm.backups[backup.Status.NodeName] = backups
	}

	if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
		backups[backup.Name] = backup.Spec.Storage.PVC.Name
		abm.pvcs[backup.Spec.Storage.PVC.Name] = struct{}{}
	} else {
		backups[backup.Name] = ""
	}
}

func (abm activeBackupCache) remove(nodeName, backupName string) {
	if backups, ok := abm.backups[nodeName]; ok {
		if pvcName, ok := backups[backupName]; ok {
			delete(abm.pvcs, pvcName)
		}
		delete(backups, backupName)
		if len(backups) == 0 {
			delete(abm.backups, nodeName)
		}
	}
}

func (abm activeBackupCache) canStart(backup *operatorv1alpha1.EtcdBackup, nodeName string) (ok bool, reason string) {
	if abm.nodeInUse(nodeName) {
		backupNames := make([]string, 0, len(abm.backups[nodeName]))
		for name := range abm.backups[nodeName] {
			backupNames = append(backupNames, name)
		}
		return false, fmt.Sprintf("Node %s has active backup(s): %v", nodeName, backupNames)
	}
	if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
		if pvcName := backup.Spec.Storage.PVC.Name; abm.pvcInUse(pvcName) {
			return false, fmt.Sprintf("PVC %s in use", pvcName)
		}
	}
	return true, ""
}

func (abm activeBackupCache) nodeInUse(nodeName string) bool {
	return len(abm.backups[nodeName]) > 0
}

func (abm activeBackupCache) pvcInUse(pvcName string) bool {
	_, ok := abm.pvcs[pvcName]
	return ok
}
