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

	controlPlaneNodes, err := backuphelpers.SelectBackupNodes(c.nodeLister, nil)
	if err != nil {
		return fmt.Errorf("BackupPolicyController failed to select control plane nodes for backup: %w", err)
	}

	backupsClient := c.operatorClient.EtcdBackups()
	for _, backup := range backupsQueue {
		if backup.DeletionTimestamp != nil {
			// Backup deleted before it could be queued.
			// Shouldn't have finalizer at this stage, but check just in case.
			if slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup) {
				if _, err := applyBackupFinalizer(ctx, backupsClient, backup, false); err != nil {
					return fmt.Errorf("BackupQueueController failed to finalize deleted backup %q: %w", backup.Name, err)
				}
			}
			continue
		}

		backupName := backup.Name
		nodeName := backup.Status.NodeName
		// Only Local backups are pinned to a specific node. For PVC backups the node is left unset so that
		// the scheduler can place the job on a node that satisfies the volume's topology constraints. The
		// PVC itself is the concurrency key for PVC backups (see canStart).
		if nodeName == "" && backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypeLocal {
			selector := labels.Everything()
			if len(backup.Spec.NodeSelector) > 0 {
				selector = labels.SelectorFromSet(backup.Spec.NodeSelector)
			}

			// Round robin control plane nodes: pick the first matching node that isn't already running a backup,
			// and consume it so subsequent backups in this sync are assigned to a different node.
			for idx, node := range controlPlaneNodes {
				if selector.Matches(labels.Set(node.Labels)) && !c.activeCache.nodes.inUse(node.Name) {
					nodeName = node.Name
					controlPlaneNodes = append(controlPlaneNodes[:idx], controlPlaneNodes[idx+1:]...)
					break
				}
			}
			if nodeName == "" {
				klog.Infof("BackupQueueController unable to start backup [%s]: all control plane nodes in use", backupName)
				continue
			}
		}
		if ok, reason := c.activeCache.canStart(backup, nodeName); !ok {
			klog.Infof("BackupQueueController unable to start backup [%s]: %s", backupName, reason)
			continue
		}

		if backup, err = applyBackupPending(ctx, backupsClient, backup, nodeName); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("BackupQueueController failed to promote backup %q to pending: %w", backupName, err)
		}
		klog.Infof("BackupQueueController promoted backup [%s] to pending on node [%s]", backupName, nodeName)
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
		if backuphelpers.IsBackupActive(backup) {
			observedActive.add(backup)
			c.activeCache.add(backup)
		} else if !backuphelpers.IsBackupFinished(backup) {
			// Queue new backups. Check cache in case of informer lag
			if !c.activeCache.isActive(backup.Name) {
				backups[n] = backup
				n++
			}
		} else {
			c.activeCache.remove(backup.Name)
		}
	}
	backups = backups[:n]
	slices.SortFunc(backups[:n], func(a, b *operatorv1alpha1.EtcdBackup) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})

	// Any items in activeBackups that weren't observed this sync are either new entries that haven't synced to the
	// informer cache yet, or no longer exist. Compare against source of truth (k8s API).
	backupsClient := c.operatorClient.EtcdBackups()
	for backupName := range c.activeCache.backups {
		if _, exists := observedActive.backups[backupName]; !exists {
			if exists, err := backupExists(ctx, backupsClient, backupName); err != nil {
				return nil, err
			} else if !exists {
				c.activeCache.remove(backupName)
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
		backups: map[string]activeBackup{},
		// Limit 1 active Local backup per Node
		nodes: activeResources{},
		// Limit 1 active PVC backup per PVC
		pvcs: activeResources{},
	}
}

type activeBackupCache struct {
	// map active backups by name
	backups map[string]activeBackup
	// map nodes to the active backups running on the node. Should be at most 1 unless something unexpected happens.
	nodes activeResources
	// map active pvcs. Should be at most 1 unless something unexpected happens.
	pvcs activeResources
}

type activeBackup struct {
	nodeName, pvcName string
}

func (abc activeBackupCache) isActive(backupName string) bool {
	_, ok := abc.backups[backupName]
	return ok
}

func (abc activeBackupCache) add(backup *operatorv1alpha1.EtcdBackup) {
	nodeName := backup.Status.NodeName
	var pvcName string
	if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
		pvcName = backup.Spec.Storage.PVC.Name
	}

	if active, ok := abc.backups[backup.Name]; ok {
		// Backup already cached. Make sure nothing has changed.
		if active.nodeName != nodeName {
			if active.nodeName != "" {
				abc.nodes.remove(active.nodeName, backup.Name)
			}
			if nodeName != "" {
				abc.nodes.add(nodeName, backup.Name)
			}
		}
		if pvcName != active.pvcName {
			if active.pvcName != "" {
				abc.pvcs.remove(active.pvcName, backup.Name)
			}
			if pvcName != "" {
				abc.pvcs.add(pvcName, backup.Name)
			}
		}
	} else {
		if nodeName != "" {
			abc.nodes.add(nodeName, backup.Name)
		}
		if pvcName != "" {
			abc.pvcs.add(pvcName, backup.Name)
		}
	}
	abc.backups[backup.Name] = activeBackup{nodeName: nodeName, pvcName: pvcName}
}

func (abc activeBackupCache) remove(backupName string) {
	if activeBackup, ok := abc.backups[backupName]; ok {
		abc.nodes.remove(activeBackup.nodeName, backupName)
		if activeBackup.pvcName != "" {
			abc.pvcs.remove(activeBackup.pvcName, backupName)
		}
		delete(abc.backups, backupName)
	}
}

func (abc activeBackupCache) canStart(backup *operatorv1alpha1.EtcdBackup, nodeName string) (ok bool, reason string) {
	if nodeName != "" && abc.nodes.inUse(nodeName) {
		backupNames := make([]string, 0, len(abc.nodes[nodeName]))
		for name := range abc.nodes[nodeName] {
			backupNames = append(backupNames, name)
		}
		return false, fmt.Sprintf("Node %s has active backup(s): %v", nodeName, backupNames)
	}
	if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
		if pvcName := backup.Spec.Storage.PVC.Name; abc.pvcs.inUse(pvcName) {
			return false, fmt.Sprintf("PVC %s in use", pvcName)
		}
	}
	return true, ""
}

type activeResources map[string]map[string]struct{}

func (ar activeResources) inUse(name string) bool {
	if _, ok := ar[name]; ok {
		return ok
	}
	return false
}

func (ar activeResources) add(name, backupName string) {
	if backups, ok := ar[name]; ok {
		backups[backupName] = struct{}{}
	} else {
		ar[name] = map[string]struct{}{backupName: {}}
	}
}

func (ar activeResources) remove(name, backupName string) {
	if backups, ok := ar[name]; ok {
		delete(backups, backupName)
		if len(backups) == 0 {
			delete(ar, name)
		}
	}
}
