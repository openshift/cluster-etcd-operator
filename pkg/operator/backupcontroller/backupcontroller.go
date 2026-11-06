package backupcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	applyconfigurationsoperatorv1alpha1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
)

const (
	// SSA field managers used by backup controller to set status and finalizer on EtcdBackup.
	backupStatusFieldManager    = "etcd-backup-status"
	backupFinalizerFieldManager = "etcd-backup-finalizer"

	backupAppName    = "cluster-backup-job"
	backupDirEnvName = "CLUSTER_BACKUP_PATH"
	jobFinishedTTL   = 4 * time.Hour

	jobNotFoundGracePeriod = 2 * time.Minute
	// podStatusGracePeriod bounds how long a finished job waits for its pods to be observed in a terminal state. Pods
	// that were deleted will never be observed, so the outcome is eventually recorded from whatever is available.
	podStatusGracePeriod     = 1 * time.Minute
	backupDeletedGracePeriod = 1 * time.Minute
	conflictRequeueDelay     = 5 * time.Second

	labelJobName = "batch.kubernetes.io/job-name"

	backupVolumeName = "etc-kubernetes-cluster-backup"

	maxNameLength = 63
)

type BackupController struct {
	backupsLister         operatorv1alpha1listers.EtcdBackupLister
	podsLister            corev1listers.PodNamespaceLister
	nodesLister           corev1listers.NodeLister
	pvcsLister            corev1listers.PersistentVolumeClaimNamespaceLister
	jobInformer           cache.SharedIndexInformer
	operatorClient        operatorv1alpha1client.OperatorV1alpha1Interface
	kubeClient            kubernetes.Interface
	operatorImagePullSpec string
	featureGateAccessor   featuregates.FeatureGateAccess
	activeCache           activeBackupCache
}

// NewBackupController creates a controller that runs EtcdBackups. Each sync reconciles every EtcdBackup: the status
// of started backups is updated from their jobs, and new backups are started oldest first, subject to the
// concurrency limits of one active backup per Node and one active backup per PVC.
func NewBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsLister operatorv1alpha1listers.EtcdBackupLister,
	podsLister corev1listers.PodNamespaceLister,
	nodesLister corev1listers.NodeLister,
	pvcsLister corev1listers.PersistentVolumeClaimNamespaceLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	backupInformer factory.Informer,
	jobInformer cache.SharedIndexInformer,
	podInformer factory.Informer,
	nodeInformer factory.Informer,
	pvcInformer factory.Informer) (factory.Controller, error) {
	c := &BackupController{
		backupsLister:         backupsLister,
		podsLister:            podsLister,
		nodesLister:           nodesLister,
		pvcsLister:            pvcsLister,
		jobInformer:           jobInformer,
		operatorClient:        operatorClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		featureGateAccessor:   accessor,
		activeCache:           newActiveBackupCache(),
	}
	if err := c.addIndexers(); err != nil {
		return nil, err
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithInformers(backupInformer).
		WithFilteredEventsInformers(func(obj any) bool {
			// Only trigger sync on backup jobs when they have a finalizer and are finished
			if job, ok := obj.(*batchv1.Job); ok {
				return job.Namespace == operatorclient.TargetNamespace &&
					job.Labels != nil &&
					job.Labels["app"] == backupAppName &&
					slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) &&
					isJobFinished(job)
			}
			return false
		}, jobInformer).
		WithBareInformers(podInformer, nodeInformer, pvcInformer).
		WithSync(syncer.Sync).
		ToController("BackupController", eventRecorder.WithComponentSuffix("backup-controller")), nil
}

func (c *BackupController) addIndexers() error {
	err := c.jobInformer.AddIndexers(cache.Indexers{
		backuphelpers.LabelEtcdBackup: func(obj any) ([]string, error) {
			if job, ok := obj.(*batchv1.Job); ok {
				// Only index jobs with the backup label and finalizer. Once a job has been finalized, it is ignored.
				if job.Labels != nil && slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
					if backupName, ok := job.Labels[backuphelpers.LabelEtcdBackup]; ok {
						return []string{backupName}, nil
					}
				}
			}
			return nil, nil
		},
	})
	if err != nil {
		return fmt.Errorf("BackupController failed to create job indexer: %w", err)
	}
	return nil
}

func (c *BackupController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupController error while checking feature flags: %v", err)
		}
		return nil
	}

	backups, err := c.backupsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("BackupController failed to list backups: %w", err)
	}
	slices.SortFunc(backups, func(a, b *operatorv1alpha1.EtcdBackup) int {
		if cmp := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Name, b.Name)
	})

	var errs []error
	var requeueAfter time.Duration

	// Reconcile backups that have already been started, so that the resources held by active backups are known
	// before any new backups are started.
	observedActive := map[string]struct{}{}
	queue := make([]*operatorv1alpha1.EtcdBackup, 0, len(backups))
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	for _, backup := range backups {
		var state backupState
		var delay time.Duration
		job, err := c.getJob(backup)
		if err != nil {
			errs = append(errs, fmt.Errorf("BackupController failed to get job for backup %q: %w", backup.Name, err))
			// Assume active to hold reservations
			state = backupActive
		} else if state, delay, err = c.reconcileBackupState(ctx, jobsClient, backup, job); err != nil {
			errs = append(errs, err)
		}
		requeueAfter = minDelay(requeueAfter, delay)

		switch state {
		case backupActive:
			observedActive[backup.Name] = struct{}{}
			c.activeCache.add(withJobNodeName(backup, job))
		case backupFinished:
			c.activeCache.remove(backup.Name)
		case backupQueued:
			if c.activeCache.isReplaced(backup.Name, backup.UID) {
				// The cached backup was deleted and recreated with the same name, so the cache entry doesn't apply to
				// this backup. Release the reservation of the deleted backup.
				c.activeCache.remove(backup.Name)
			}
			// Check cache in case of informer lag on a backup that was started by a previous sync
			if !c.activeCache.isActive(backup.Name) {
				queue = append(queue, backup)
			} else if c.activeCache.isStale(backup.Name, jobNotFoundGracePeriod) {
				// The backup was started long enough ago that its job should have been observed. If the job no longer
				// exists (and its running status was never recorded), the backup has failed. This is the counterpart
				// of reconcileJobNotFound, for backups whose job name was never recorded in their status.
				if exists, err := c.backupJobExists(ctx, backup); err != nil {
					errs = append(errs, err)
				} else if !exists {
					if delay, err := c.failBackupWithMissingJob(ctx, backup); err != nil {
						errs = append(errs, err)
					} else {
						requeueAfter = minDelay(requeueAfter, delay)
					}
				}
			}
		}
	}

	if err := c.pruneActiveCache(ctx, observedActive); err != nil {
		errs = append(errs, err)
	}

	if len(queue) > 0 {
		if err := c.startQueuedBackups(ctx, queue); err != nil {
			errs = append(errs, err)
		}
	}

	// Jobs whose EtcdBackup no longer exists are not reached by the loop above. Release their finalizers so they can be
	// deleted.
	if err := c.reconcileOrphanedJobs(ctx, backups); err != nil {
		errs = append(errs, err)
	}

	if requeueAfter > 0 {
		syncCtx.Queue().AddAfter(syncCtx.QueueKey(), requeueAfter)
	}
	return utilerrors.NewAggregate(errs)
}

type backupState int

const (
	// backupQueued is a backup that has not been started yet
	backupQueued backupState = iota
	// backupActive is a backup that has been started, or may have been started, and holds its Node or PVC
	backupActive
	// backupFinished is a backup that no longer needs to be reconciled by this controller
	backupFinished
)

// reconcileBackupState reconciles the status of a backup. Backups that have not been started are returned as queued.
// On error, the backup is reported as active so that its resources are not handed to another backup.
func (c *BackupController) reconcileBackupState(ctx context.Context, jobsClient batchv1client.JobInterface, backup *operatorv1alpha1.EtcdBackup, job *batchv1.Job) (backupState, time.Duration, error) {
	backupName := backup.Name
	if backuphelpers.IsBackupFinished(backup) {
		if job != nil {
			if _, err := finalizeJob(ctx, jobsClient, job, false); err != nil && !apierrors.IsNotFound(err) {
				return backupFinished, 0, err
			}
		}
		return backupFinished, 0, nil
	}

	backupsClient := c.operatorClient.EtcdBackups()
	if job != nil {
		klog.V(4).Infof("BackupController job %q found for backup %q, reconciling status", job.Name, backupName)
		delay, err := reconcileJobStatus(ctx, jobsClient, c.podsLister, backupsClient, job, backup)
		if err != nil {
			if apierrors.IsConflict(err) {
				klog.V(2).Infof("BackupController conflict while reconciling status for backup %q, requeueing", backupName)
				return backupActive, conflictRequeueDelay, nil
			}
			return backupActive, 0, fmt.Errorf("BackupController failed reconciling status for backup %q: %w", backupName, err)
		}
		if delay > 0 {
			// Job is finished, but its outcome can't be recorded yet. Keep holding its resources.
			return backupActive, delay, nil
		} else if isJobFinished(job) {
			return backupFinished, 0, nil
		}
		return backupActive, 0, nil
	}

	if jobName := backup.Status.JobName; jobName != "" {
		// Give informers some time to catch up. Will reconcile sooner if the job is observed.
		if requeueAfter, ok := shouldDelay(backupStartTime(backup), jobNotFoundGracePeriod); ok {
			klog.Infof("BackupController requeueing backup %q with missing job %q", backupName, jobName)
			return backupActive, requeueAfter, nil
		}

		delay, err := reconcileJobNotFound(ctx, backupsClient, jobsClient, c.podsLister, backup)
		if err != nil {
			if apierrors.IsConflict(err) {
				klog.V(2).Infof("BackupController backup %q changed while reconciling missing job %q, requeueing", backupName, jobName)
				return backupActive, conflictRequeueDelay, nil
			}
			return backupActive, 0, fmt.Errorf("BackupController failed reconciling missing job %q for backup %q: %w", jobName, backupName, err)
		}
		// Either the backup was finished, or the job was relabeled and will be reconciled on the next sync
		return backupActive, delay, nil
	}

	if backup.DeletionTimestamp != nil {
		if !slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			// Backup deleted before it was started
			return backupFinished, 0, nil
		}

		// The finalizer is added just before the job is created. Short grace period in case the backup job was
		// started, but the backup status update failed and the job has not been observed yet.
		if requeueAfter, ok := shouldDelay(backup.DeletionTimestamp.Time, backupDeletedGracePeriod); ok {
			klog.Infof("BackupController requeueing deleted backup %q", backupName)
			return backupActive, requeueAfter, nil
		}

		if _, err := applyBackupFinalizer(ctx, backupsClient, backup, false); err != nil && !apierrors.IsNotFound(err) {
			return backupActive, 0, fmt.Errorf("BackupController failed to remove finalizer from deleted backup %q: %w", backupName, err)
		}
		return backupFinished, 0, nil
	}

	return backupQueued, 0, nil
}

// withJobNodeName returns the backup with the node name of its job, if it is a Local backup whose status is missing
// the node name due to informer lag. Otherwise the backup is returned unchanged.
func withJobNodeName(backup *operatorv1alpha1.EtcdBackup, job *batchv1.Job) *operatorv1alpha1.EtcdBackup {
	if backup.Spec.Storage.Type != operatorv1alpha1.EtcdBackupStorageTypeLocal || backup.Status.NodeName != "" ||
		job == nil || job.Spec.Template.Spec.NodeName == "" {
		return backup
	}
	backup = backup.DeepCopy()
	backup.Status.NodeName = job.Spec.Template.Spec.NodeName
	return backup
}

// pruneActiveCache removes backups from the active cache that are no longer active. Any cached backups that were not
// observed as active this sync are either new entries that haven't synced to the informer cache yet, or no longer
// exist. Compare against source of truth (k8s API).
func (c *BackupController) pruneActiveCache(ctx context.Context, observedActive map[string]struct{}) error {
	backupsClient := c.operatorClient.EtcdBackups()
	for backupName, active := range c.activeCache.backups {
		if _, ok := observedActive[backupName]; ok {
			continue
		}
		if exists, err := backupExists(ctx, backupsClient, backupName, active.uid); err != nil {
			return err
		} else if !exists {
			c.activeCache.remove(backupName)
		}
	}
	return nil
}

// startQueuedBackups starts as many of the queued backups as the concurrency limits allow, in order.
func (c *BackupController) startQueuedBackups(ctx context.Context, queue []*operatorv1alpha1.EtcdBackup) error {
	controlPlaneNodes, err := backuphelpers.ListBackupNodes(c.nodesLister, nil)
	if err != nil {
		return fmt.Errorf("BackupController failed to select control plane nodes for backup: %w", err)
	}

	var errs []error
	for _, backup := range queue {
		if err := c.startBackup(ctx, backup, controlPlaneNodes); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

func (c *BackupController) startBackup(ctx context.Context, backup *operatorv1alpha1.EtcdBackup, controlPlaneNodes []*corev1.Node) error {
	backupName := backup.Name
	backupsClient := c.operatorClient.EtcdBackups()
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)

	var ok bool
	var err error
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		backup, ok, err = c.prepareLocalBackup(ctx, backup, controlPlaneNodes)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		ok, err = c.preparePVCBackup(ctx, backup)
	default:
		// createBackupJob rejects unknown storage types
		ok = true
	}
	if err != nil || !ok {
		return err
	}

	if ok, reason := c.activeCache.canStart(backup); !ok {
		klog.V(2).Infof("BackupController unable to start backup [%s]: %s", backupName, reason)
		return nil
	}

	klog.V(4).Infof("BackupController starting backup %q", backupName)
	job, backup, err := createBackupJob(ctx, backup, c.operatorImagePullSpec, jobsClient, backupsClient)
	if err != nil {
		if apierrors.IsInvalid(err) {
			// The job will never be accepted by the API server, so retrying can't succeed.
			// No job was created, so no files can have been written.
			return c.failBackupSetup(ctx, backup, fmt.Sprintf("backup job rejected: %v", err))
		}
		return fmt.Errorf("BackupController failed to start backup %q: %w", backupName, err)
	}
	// The job exists from this point, so hold its resources even if the status update fails
	c.activeCache.add(backup)

	if _, err := applyBackupRunning(ctx, backupsClient, backup, job); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// The running status is applied when the job is observed on a future sync
			return nil
		}
		return fmt.Errorf("BackupController failed to apply running status on backup %q: %w", backupName, err)
	}
	return nil
}

// prepareLocalBackup ensures a Local backup is assigned to an eligible control plane node that is not running another
// backup, and returns the updated backup. Local backups write to a hostPath, so they are pinned to a single node. ok is
// false if the backup can't be started this sync.
func (c *BackupController) prepareLocalBackup(ctx context.Context, backup *operatorv1alpha1.EtcdBackup, controlPlaneNodes []*corev1.Node) (*operatorv1alpha1.EtcdBackup, bool, error) {
	if assignedNode := backup.Status.NodeName; assignedNode != "" {
		if !isEligibleNode(backup, assignedNode, controlPlaneNodes) {
			// Queued backups have no job, so no files can have been written to the assigned node and no GC is required.
			return backup, false, c.failBackupSetup(ctx, backup, fmt.Sprintf("assigned node %q is no longer an eligible control plane node", assignedNode))
		}
		return backup, true, nil
	}

	nodeName, matched := c.selectNode(backup, controlPlaneNodes)
	if !matched {
		return backup, false, c.failBackupSetup(ctx, backup, "unable to find a control plane Node matching the selector")
	} else if nodeName == "" {
		klog.V(2).Infof("BackupController unable to start backup [%s]: all control plane nodes in use", backup.Name)
		return backup, false, nil
	}

	// Require the version of the backup the node was selected for, so that a stale cache can't overwrite status
	updated, err := applyBackupStatus(ctx, c.operatorClient.EtcdBackups(), backup, true, func(status *applyconfigurationsoperatorv1alpha1.EtcdBackupStatusApplyConfiguration) {
		status.NodeName = &nodeName
	})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Backup was deleted or changed, re-evaluate on the next sync
			return backup, false, nil
		}
		return backup, false, fmt.Errorf("BackupController failed to assign node to Local backup %q: %w", backup.Name, err)
	}
	return updated, true, nil
}

// preparePVCBackup ensures the PVC of a PVC backup exists. PVC backups are not pinned to a node, so that the scheduler
// can place the job on a node that satisfies the volume's topology constraints. The PVC itself is the concurrency key
// for PVC backups (see canStart). ok is false if the backup can't be started this sync.
func (c *BackupController) preparePVCBackup(ctx context.Context, backup *operatorv1alpha1.EtcdBackup) (bool, error) {
	pvcsClient := c.kubeClient.CoreV1().PersistentVolumeClaims(operatorclient.TargetNamespace)
	pvcName := backup.Spec.Storage.PVC.Name
	validPVC, err := isValidPVC(ctx, c.pvcsLister, pvcsClient, pvcName)
	if err != nil {
		return false, fmt.Errorf("BackupController failed to validate PVC %q for backup %q: %w", pvcName, backup.Name, err)
	} else if !validPVC {
		return false, c.failBackupSetup(ctx, backup, fmt.Sprintf("unable to find PVC %q", pvcName))
	}
	return true, nil
}

// failBackupSetup fails a queued backup that can never be started. No job ran, so no files can have been written and
// garbage collection is skipped. If the backup was deleted or changed since it was read, it is re-evaluated on the
// next sync.
func (c *BackupController) failBackupSetup(ctx context.Context, backup *operatorv1alpha1.EtcdBackup, message string) error {
	if _, err := applyBackupFinished(ctx, c.operatorClient.EtcdBackups(), backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonSetupFailed, message, nil); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("BackupController failed to apply failed status on backup %q: %w", backup.Name, err)
	}
	klog.Infof("BackupController failed backup %q: %s", backup.Name, message)
	return nil
}

// nodeSelectorFor returns the selector for the nodes a backup may run on, in addition to being a control plane node.
func nodeSelectorFor(backup *operatorv1alpha1.EtcdBackup) labels.Selector {
	if len(backup.Spec.NodeSelector) > 0 {
		return labels.SelectorFromSet(backup.Spec.NodeSelector)
	}
	return labels.Everything()
}

// selectNode picks the first available control plane node matching the backup's node selector that isn't already
// running a backup. matched reports whether any control plane node matched the node selector, regardless of whether it
// is in use or available.
func (c *BackupController) selectNode(backup *operatorv1alpha1.EtcdBackup, controlPlaneNodes []*corev1.Node) (nodeName string, matched bool) {
	selector := nodeSelectorFor(backup)
	for _, node := range controlPlaneNodes {
		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		matched = true
		if isNodeAvailable(node) && !c.activeCache.local.inUse(node.Name) {
			return node.Name, true
		}
	}
	return "", matched
}

// isNodeAvailable reports whether a node can run a backup job. Local backup jobs are pinned to their node, bypassing
// the scheduler, so a cordoned or not ready node would hold the backup until its job deadline expires.
func isNodeAvailable(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	// Ready condition not reported yet, let the job deadline handle a node that never becomes ready
	return true
}

// isEligibleNode reports whether nodeName is a control plane node matching the backup's node selector.
func isEligibleNode(backup *operatorv1alpha1.EtcdBackup, nodeName string, controlPlaneNodes []*corev1.Node) bool {
	for _, node := range controlPlaneNodes {
		if node.Name == nodeName {
			return nodeSelectorFor(backup).Matches(labels.Set(node.Labels))
		}
	}
	return false
}

// reconcileOrphanedJobs handles backup jobs that still hold the backup finalizer, but whose owning EtcdBackup no longer
// exists. This can happen if the EtcdBackup finalizer is removed while its job exists (e.g. manually, or a deleted
// backup's job was not yet observed when its grace period expired). Kubernetes garbage collection deletes such jobs,
// but they can't be removed until the backup finalizer is released, and nothing else would reconcile them.
func (c *BackupController) reconcileOrphanedJobs(ctx context.Context, backups []*operatorv1alpha1.EtcdBackup) error {
	backupUIDs := make(map[types.UID]struct{}, len(backups))
	for _, backup := range backups {
		backupUIDs[backup.UID] = struct{}{}
	}

	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	var errs []error
	for _, obj := range c.jobInformer.GetIndexer().List() {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Namespace != operatorclient.TargetNamespace || job.Labels["app"] != backupAppName ||
			!slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			continue
		}
		ownerUID, ok := jobOwnerUID(job)
		if !ok {
			continue
		}
		// Jobs are only created for backups observed in the informer cache, and a backup only leaves the cache once it
		// is deleted. A job whose owner is not in the cache therefore belongs to a deleted backup.
		if _, ok := backupUIDs[ownerUID]; ok {
			continue
		}

		// The backup was removed without its finalizer being handled by this controller (e.g. force deleted), so
		// whoever removed it is responsible for any side effects. Release the job so garbage collection can delete it.
		if _, err := finalizeJob(ctx, jobsClient, job, false); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("BackupController failed to finalize orphaned job %q: %w", job.Name, err))
			continue
		}
		klog.Infof("BackupController finalized job %q without an owning backup", job.Name)
	}
	return utilerrors.NewAggregate(errs)
}

// failBackupWithMissingJob fails a backup that was started by this controller, but whose job no longer exists and was
// never recorded in the backup status. The backup container may have run and written files, so the generic job failure
// reason is used to ensure that garbage collection is not skipped. A non-zero delay is returned if the backup changed
// since it was read, and must be re-evaluated.
func (c *BackupController) failBackupWithMissingJob(ctx context.Context, backup *operatorv1alpha1.EtcdBackup) (time.Duration, error) {
	message := fmt.Sprintf("unable to find job %q", generateBackupJobName(backup))
	if _, err := applyBackupFinished(ctx, c.operatorClient.EtcdBackups(), backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonJobFailed, message, nil); err != nil {
		if apierrors.IsConflict(err) {
			// Keep the reservation until the latest version of the backup is observed
			klog.V(2).Infof("BackupController backup %q changed while reconciling missing job, requeueing", backup.Name)
			return conflictRequeueDelay, nil
		}
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("BackupController failed to apply failed status on backup %q with missing job: %w", backup.Name, err)
		}
	}
	klog.Infof("BackupController failed backup %q: %s", backup.Name, message)
	c.activeCache.remove(backup.Name)
	return 0, nil
}

// backupJobExists checks the API for the backup's job, bypassing the informer cache. Jobs owned by a previous EtcdBackup
// with the same name are ignored.
func (c *BackupController) backupJobExists(ctx context.Context, backup *operatorv1alpha1.EtcdBackup) (bool, error) {
	jobName := generateBackupJobName(backup)
	job, err := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("BackupController failed to get job %q for backup %q: %w", jobName, backup.Name, err)
	}
	if ownerUID, ok := jobOwnerUID(job); ok && ownerUID != backup.UID {
		// Job belongs to a previous EtcdBackup with the same name
		return false, nil
	}
	return true, nil
}

// backupExists checks the API for the backup. If uid is set, a backup with the same name but a different UID (i.e. the
// backup was deleted and recreated) is reported as not existing.
func backupExists(ctx context.Context, backupsClient operatorv1alpha1client.EtcdBackupInterface, name string, uid types.UID) (bool, error) {
	backup, err := backupsClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return true, fmt.Errorf("BackupController failed to get backup [%s]: %w", name, err)
		}
		return false, nil
	}
	if uid != "" && backup.UID != "" && backup.UID != uid {
		return false, nil
	}
	return true, nil
}

func (c *BackupController) getJob(backup *operatorv1alpha1.EtcdBackup) (*batchv1.Job, error) {
	var job *batchv1.Job
	objs, err := c.jobInformer.GetIndexer().ByIndex(backuphelpers.LabelEtcdBackup, backup.Name)
	if err != nil {
		return nil, fmt.Errorf("error reading job index %s: %w", backuphelpers.LabelEtcdBackup, err)
	}
	for _, obj := range objs {
		if j, ok := obj.(*batchv1.Job); ok {
			if ownerUID, ok := jobOwnerUID(j); ok && ownerUID != backup.UID {
				// Job belongs to a previous EtcdBackup with the same name
				continue
			}
			if job == nil {
				job = j
			} else if j.CreationTimestamp.After(job.CreationTimestamp.Time) {
				job = j
			}
		}
	}
	return job, nil
}

// reconcileJobStatus updates the backup status from its job, and finalizes the job once it is finished and the backup
// status is recorded. A non-zero delay is returned if the job is finished, but its outcome can't be recorded yet.
func reconcileJobStatus(ctx context.Context,
	jobClient batchv1client.JobInterface,
	podLister corev1listers.PodNamespaceLister,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	job *batchv1.Job,
	backup *operatorv1alpha1.EtcdBackup) (time.Duration, error) {
	if !isJobFinished(job) {
		if backup.Status.JobName == "" {
			// Status update failed when job was created. Attempt to update now.
			if _, err := applyBackupRunning(ctx, backupClient, backup, job); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}

	// If finalizing job fails for some reason, we may re-reconcile the job. Only need to update the backup once.
	if !backuphelpers.IsBackupFinished(backup) {
		backup = backup.DeepCopy()

		pods, err := listJobPods(podLister, job)
		if err != nil {
			return 0, fmt.Errorf("error listing pods for backup job %q: %w", job.Name, err)
		}
		if !podHistoryComplete(job, pods) {
			// The pod cache lags the job, e.g. a pod still appears running without its termination message. Recording
			// the outcome now would permanently lose the files reported by the backup, so wait for the pods to be
			// observed in a terminal state. Bounded, since pods that were deleted will never be observed.
			if remaining, ok := shouldDelay(jobFinishedTime(job), podStatusGracePeriod); ok && remaining > 0 {
				klog.V(2).Infof("BackupController job %q for backup %q is finished, waiting for pod status", job.Name, backup.Name)
				return remaining, nil
			}
			klog.Infof("BackupController job %q for backup %q is finished, but not all pods were observed in a terminal state", job.Name, backup.Name)
		}

		outcome := determineBackupOutcome(backup.Name, job, pods)
		if _, err := applyBackupFinished(ctx, backupClient, backup, job, outcome.condition, outcome.reason, outcome.message, outcome.files); err != nil {
			if apierrors.IsNotFound(err) {
				return 0, nil
			}
			return 0, err
		}

		if outcome.condition == operatorv1alpha1.BackupCompleted {
			klog.Infof("BackupController completed backup %q", backup.Name)
		} else {
			klog.Infof("BackupController failed backup %q: job failed", backup.Name)
		}
	}

	if _, err := finalizeJob(ctx, jobClient, job, false); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, err
		}
	}

	return 0, nil
}

// reconcileJobNotFound reconciles a started backup whose recorded job (Status.JobName) has not been observed in the
// informer cache after a grace period. Backups whose job name was never recorded are handled in sync, using the active
// cache (see failBackupWithMissingJob).
func reconcileJobNotFound(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	jobsClient batchv1client.JobInterface,
	podsLister corev1listers.PodNamespaceLister,
	backup *operatorv1alpha1.EtcdBackup,
) (time.Duration, error) {
	// Failing a backup is not reversible, so act on the latest version of the backup rather than the informer cache.
	// The cached backup may be stale, e.g. it already completed and its job has since been finalized and removed.
	backup, err := backupClient.Get(ctx, backup.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("error getting backup: %w", err)
	}
	if backuphelpers.IsBackupFinished(backup) || backup.Status.JobName == "" {
		return 0, nil
	}

	jobName := backup.Status.JobName
	job, err := jobsClient.Get(ctx, jobName, metav1.GetOptions{})

	var message string
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("error getting job %q for backup %q: %w", jobName, backup.Name, err)
		}
		message = fmt.Sprintf("unable to find job %q", jobName)
	} else if ownerUID, ok := jobOwnerUID(job); !ok || ownerUID != backup.UID {
		message = fmt.Sprintf("found job %q missing owner reference for %s", jobName, backup.Name)
	} else {
		if !slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			// Jobs without the finalizer (e.g. removed manually) are not indexed, so they will never be observed
			// through the informer. Reconcile the backup status directly from the job.
			klog.Infof("BackupController job %q for backup %q is missing finalizer, reconciling status from API", job.Name, backup.Name)
			return reconcileJobStatus(ctx, jobsClient, podsLister, backupClient, job, backup)
		}
		// Correct job exists, make sure it is labeled appropriately. Otherwise assume it will be handled on a future sync.
		if job.Labels[backuphelpers.LabelEtcdBackup] != backup.Name {
			patchBody, err := json.Marshal(batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{backuphelpers.LabelEtcdBackup: backup.Name}}})
			if err != nil {
				return 0, fmt.Errorf("error marshalling job %q patch: %w", job.Name, err)
			}
			if _, err := jobsClient.Patch(ctx, job.Name, types.StrategicMergePatchType, patchBody, metav1.PatchOptions{}); err != nil {
				return 0, fmt.Errorf("error adding label to job: %w", err)
			}
		}
		return 0, nil
	}

	if _, err := applyBackupFinished(ctx, backupClient, backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonJobFailed, message, nil); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("error updating failed backup: %w", err)
	}
	klog.Infof("BackupController failed backup %q: %s", backup.Name, message)
	return 0, nil
}

// backupOutcome is the finished status recorded on a backup from its job.
type backupOutcome struct {
	condition operatorv1alpha1.BackupConditionType
	reason    operatorv1alpha1.BackupConditionReason
	message   string
	files     []operatorv1alpha1.EtcdBackupFile
}

// determineBackupOutcome derives the finished status of a backup from its finished job and the job's pods. The reason
// is only downgraded from the generic JobFailed to SetupFailed when it is certain that no backup files were written,
// since SetupFailed skips garbage collection.
func determineBackupOutcome(backupName string, job *batchv1.Job, pods []*corev1.Pod) backupOutcome {
	outcome := backupOutcome{
		condition: operatorv1alpha1.BackupCompleted,
		reason:    operatorv1alpha1.BackupReasonJobCompleted,
	}
	if isJobFailed(job) {
		outcome.condition = operatorv1alpha1.BackupFailed
		outcome.reason = operatorv1alpha1.BackupReasonJobFailed
	}

	terminationMessage, started := backuphelpers.FindPodTerminationMessage(pods)
	if started {
		// Backup container started, expect to be able to parse termination message
		termLog, err := backuphelpers.ParseTerminationMessage(terminationMessage)
		if err != nil {
			klog.Infof("BackupController failed to read termination message for backup %q: %v", backupName, err)
		}
		outcome.message, outcome.files = termLog.Message, termLog.Files
		if len(outcome.files) == 0 && outcome.condition == operatorv1alpha1.BackupFailed {
			if startedPodsCreatedNoFiles(job, pods) {
				// Every attempt reported graceful failure, no GC is required
				outcome.reason = operatorv1alpha1.BackupReasonSetupFailed
				if outcome.message == "" {
					outcome.message = "backup job failed without creating files"
				}
			} else {
				// An earlier attempt may have written partial files. Keep the generic job reason so that
				// garbage collection is not skipped.
				klog.Infof("BackupController backup %q reported no files, but not every attempt confirmed it created no files", backupName)
			}
		}
	} else if outcome.condition != operatorv1alpha1.BackupFailed {
		// Job completed, but its pods are no longer observable. Keep the default completed message.
		klog.Infof("BackupController unable to read termination message of completed backup %q from job %q pods", backupName, job.Name)
	} else if podHistoryComplete(job, pods) {
		// Every pod of the job is terminal and none started the backup container, most likely the init
		// container failed or PVC was unable to mount. No files can have been written.
		if terminationMessage == "" && len(pods) == 0 {
			terminationMessage = "backup job failed without creating pods"
		}
		outcome.reason = operatorv1alpha1.BackupReasonSetupFailed
		outcome.message = terminationMessage
		if outcome.message == "" {
			outcome.message = "failed to verify storage"
		}
	} else {
		// Pod history is missing or stale, so the backup container may have run and written partial files.
		// Keep the generic job reason so that garbage collection is not skipped.
		klog.Infof("BackupController unable to determine execution state of backup %q from job %q pods", backupName, job.Name)
		outcome.message = "unable to determine backup job outcome"
	}
	return outcome
}

// podHistoryComplete reports whether every pod the finished job accounted for is observed in a terminal phase.
// Job pods are tracked with finalizers, so every pod that terminated is counted in the job status (or listed as
// uncounted) before it can be removed. A job that counted no pods therefore never ran one to termination.
func podHistoryComplete(job *batchv1.Job, pods []*corev1.Pod) bool {
	if job.Status.Active > 0 || ptr.Deref(job.Status.Terminating, 0) > 0 {
		// Job still accounts for non-terminal pods that must be observed first
		return false
	}
	expectedPods := int64(job.Status.Failed) + int64(job.Status.Succeeded)
	if uncounted := job.Status.UncountedTerminatedPods; uncounted != nil {
		expectedPods += int64(len(uncounted.Failed)) + int64(len(uncounted.Succeeded))
	}
	if int64(len(pods)) < expectedPods {
		// Some pods the job accounted for are not observable, either deleted or not yet in the cache
		return false
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			// The job is finished, so a non-terminal pod means the cached pod status is stale
			return false
		}
	}
	return true
}

// startedPodsCreatedNoFiles reports whether it is certain that no attempt of the job created backup files: the pod
// history is complete, and every pod that started the backup container reported, via its termination message, that
// it created no files. A pod that crashed without a parseable message may have written partial files.
func startedPodsCreatedNoFiles(job *batchv1.Job, pods []*corev1.Pod) bool {
	if !podHistoryComplete(job, pods) {
		return false
	}
	for _, pod := range pods {
		message, started := backuphelpers.PodTerminationMessage(pod)
		if !started {
			continue
		}
		termLog, err := backuphelpers.ParseTerminationMessage(message)
		if err != nil || len(termLog.Files) > 0 {
			return false
		}
	}
	return true
}

// isValidPVC checks that the PVC exists. A PVC missing from the informer cache is confirmed against the API, since it
// may have been created at the same time as the backup.
func isValidPVC(ctx context.Context, pvcsLister corev1listers.PersistentVolumeClaimNamespaceLister, pvcClient corev1client.PersistentVolumeClaimInterface, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	if _, err := pvcsLister.Get(name); err == nil {
		return true, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	if _, err := pvcClient.Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func backupStartTime(backup *operatorv1alpha1.EtcdBackup) time.Time {
	for _, condition := range backup.Status.Conditions {
		if condition.Type == string(operatorv1alpha1.BackupRunning) {
			return condition.LastTransitionTime.Time
		}
	}
	return backup.CreationTimestamp.Time
}

// minDelay returns the shortest non-zero delay, where zero means no delay was requested.
func minDelay(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

func shouldDelay(since time.Time, gracePeriod time.Duration) (time.Duration, bool) {
	remaining := gracePeriod - time.Since(since)
	if remaining >= 0 {
		return remaining, true
	}
	return 0, false
}
