package backuppolicycontroller

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"

	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

type BackupPolicyRetentionController struct {
	backupsLister        operatorv1alpha1listers.EtcdBackupLister
	backupPoliciesLister operatorv1alpha1listers.EtcdBackupPolicyLister
	operatorClient       operatorv1alpha1client.OperatorV1alpha1Interface
	featureGateAccessor  featuregates.FeatureGateAccess
}

func NewBackupPolicyRetentionController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsLister operatorv1alpha1listers.EtcdBackupLister,
	backupPoliciesLister operatorv1alpha1listers.EtcdBackupPolicyLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	eventRecorder events.Recorder,
	accessor featuregates.FeatureGateAccess,
	etcdBackupPolicyInformer factory.Informer,
	etcdBackupInformer factory.Informer) factory.Controller {

	c := &BackupPolicyRetentionController{
		backupsLister:        backupsLister,
		backupPoliciesLister: backupPoliciesLister,
		operatorClient:       operatorClient,
		featureGateAccessor:  accessor,
	}

	syncer := health.NewCheckingSyncWrapper(c.sync, 15*time.Minute)
	livenessChecker.Add("BackupPolicyRetentionController", syncer)

	return factory.New().
		WithInformersQueueKeysFunc(
			func(o runtime.Object) []string {
				if backupPolicy, ok := o.(*operatorv1alpha1.EtcdBackupPolicy); ok {
					return []string{backupPolicy.Name}
				}
				if backup, ok := o.(*operatorv1alpha1.EtcdBackup); ok && backup.Labels != nil {
					// Only trigget sync on backups owned by an EtcdBackupPolicy when they are not deleted and have completed or failed
					backupPolicyName := backup.Labels[backuphelpers.LabelEtcdBackupPolicy]
					if backupPolicyName != "" && backup.DeletionTimestamp == nil && backuphelpers.IsBackupFinished(backup) {
						return []string{backupPolicyName}
					}
				}
				return nil
			},
			etcdBackupInformer,
			etcdBackupPolicyInformer,
		).
		WithSync(syncer.Sync).
		WithPostStartHooks(func(ctx context.Context, syncCtx factory.SyncContext) error {
			wait.UntilWithContext(ctx, func(ctx context.Context) {
				backupPolicies, err := c.backupPoliciesLister.List(labels.Everything())
				if err != nil {
					klog.Warningf("BackupPolicyRetentionController failed to list EtcdBackupPolicies for queueing: %s", err)
					return
				}

				for _, backupPolicy := range backupPolicies {
					syncCtx.Queue().Add(backupPolicy.Name)
				}
			}, 5*time.Minute)
			return nil
		}).
		ToController("BackupPolicyRetentionController", eventRecorder.WithComponentSuffix("backup-policy-retention-controller"))
}

func (c *BackupPolicyRetentionController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupPolicyRetentionController error while checking feature flags: %v", err)
		}
		return nil
	}

	backupPolicyName := syncCtx.QueueKey()
	backupPolicy, err := c.backupPoliciesLister.Get(backupPolicyName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("BackupPolicyRetentionController could not get EtcdBackupPolicy %s: %w", backupPolicyName, err)
	} else if backupPolicy.DeletionTimestamp != nil {
		return nil
	}

	if err := c.pruneBackups(ctx, backupPolicy); err != nil {
		return fmt.Errorf("BackupPolicyRetentionController failed to prune backups for EtcdBackupPolicy %s: %w", backupPolicy.Name, err)
	}
	return nil
}

func (c *BackupPolicyRetentionController) pruneBackups(ctx context.Context, backupPolicy *operatorv1alpha1.EtcdBackupPolicy) error {
	backups, err := c.listFinishedBackups(backupPolicy.Name)
	if err != nil {
		return err
	} else if len(backups) == 0 {
		return nil
	}

	backups = filterPruneableBackups(backupPolicy, backups)
	for _, backup := range backups {
		klog.Infof("BackupPolicyRetentionController deleting backup %s for policy %s", backup.Name, backupPolicy.Name)
		if err := c.operatorClient.EtcdBackups().Delete(ctx, backup.Name, v1.DeleteOptions{}); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (c *BackupPolicyRetentionController) listFinishedBackups(backupPolicyName string) ([]*operatorv1alpha1.EtcdBackup, error) {
	backups, err := c.backupsLister.List(labels.SelectorFromSet(labels.Set{backuphelpers.LabelEtcdBackupPolicy: backupPolicyName}))
	if err != nil {
		return nil, err
	}

	n := 0
	for _, backup := range backups {
		if backup.DeletionTimestamp == nil && backuphelpers.IsBackupFinished(backup) {
			backups[n] = backup
			n++
		}
	}

	return backups[:n], nil
}

// filterPruneableBackups sorts backups slice and returns only the items that should be pruned.
// The slice is modified in place.
func filterPruneableBackups(backupPolicy *operatorv1alpha1.EtcdBackupPolicy, backups []*operatorv1alpha1.EtcdBackup) []*operatorv1alpha1.EtcdBackup {
	pruneGroups := map[string]*pruneGroup{}

	// Sort by creation descending so oldest are deleted first
	slices.SortStableFunc(backups, func(a, b *operatorv1alpha1.EtcdBackup) int {
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})

	n := 0
	numFailed := 0
	for _, backup := range backups {
		// Prune failed backups by total history limit
		if backuphelpers.IsBackupFailed(backup) {
			if numFailed >= backupPolicy.Spec.FailedBackupsHistoryLimit {
				backups[n] = backup
				n++
			}

			numFailed++
			continue
		}

		// Prune completed backups by storage backend
		// 	Local backups are pruned per-node
		//  PVC backups are pruned per-pvc (there is only 1 per etcdbackuppolicy)
		groupName := ""
		if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypeLocal {
			groupName = backup.Status.NodeName
		}
		group := pruneGroups[groupName]
		if group == nil {
			group = &pruneGroup{}
			pruneGroups[groupName] = group
		}

		for _, rule := range backupPolicy.Spec.RetentionRules {
			switch rule.Type {
			case operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity:
				if rule.MaxQuantity > 0 {
					group.quantity++
					if rule.MaxQuantity < group.quantity {
						backups[n] = backup
						n++
					}
				}
			case operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxSize:
				if !rule.MaxSize.IsZero() {
					for _, file := range backup.Status.Files {
						group.size.Add(file.Size)
					}
					if rule.MaxSize.Cmp(group.size) < 0 {
						backups[n] = backup
						n++
					}
				}
				// case operatorv1alpha1.MaxAge:
				// 	if rule.MaxAge.Duration > 0 && rule.MaxAge.Duration < time.Since(backup.CreationTimestamp.Time) {
				// 		backups[n] = backup
				//		n++
				// 	}
				// }
			}
		}
	}

	return backups[:n]
}

type pruneGroup struct {
	quantity int
	size     resource.Quantity
}
