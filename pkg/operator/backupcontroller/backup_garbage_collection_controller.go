package backupcontroller

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"

	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	batchv1listers "k8s.io/client-go/listers/batch/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	backupGCAppName      = "cluster-backup-gc-job"
	backupGCFilesEnvName = "CLUSTER_BACKUP_GC_FILES"
	gcJobFinishedTTL     = 2 * time.Hour
	gcMaxBackupsPerJob   = 100
	// MAX_ARG_STRLEN in linux is 128KiB, limiting the max length of a singel argument/env.
	// Limit GC files to 64KiB to ensure it stays well under the single arg limit, and that all args are within the total ARG_MAX limit (min 128KiB, max 6MiB depending on the system)
	gcMaxFileArgBytes = 64 * 1024
	gcBackoffBase     = 30 * time.Second
	gcBackoffMax      = 30 * time.Minute
)

// gcFinalizeRequeueDelay is how soon to re-check a completed GC job that is kept until the removal of its owning
// backups' finalizers is observed. Variable for tests.
var gcFinalizeRequeueDelay = 10 * time.Second

// BackupGarbageCollectionController cleans up files associated with deleted EtcdBackup objects
type BackupGarbageCollectionController struct {
	backupsLister         operatorv1alpha1listers.EtcdBackupLister
	jobsLister            batchv1listers.JobNamespaceLister
	nodesLister           corev1listers.NodeLister
	pvcsLister            corev1listers.PersistentVolumeClaimNamespaceLister
	operatorClient        operatorv1alpha1client.OperatorV1alpha1Interface
	kubeClient            kubernetes.Interface
	featureGateAccessor   featuregates.FeatureGateAccess
	operatorImagePullSpec string

	// retryQueue tracks failed GC per backup for exponential backoff on retries.
	// Backoff is in-memory only, so retries may happen earlier than expected after an operator restart.
	retryQueue gcRetryQueue
	metrics               *backupMetrics
}

func NewBackupGarbageCollectionController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsLister operatorv1alpha1listers.EtcdBackupLister,
	jobsLister batchv1listers.JobNamespaceLister,
	nodesLister corev1listers.NodeLister,
	pvcsLister corev1listers.PersistentVolumeClaimNamespaceLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	metrics *backupMetrics,
	backupInformer factory.Informer,
	jobInformer factory.Informer,
	nodeInformer cache.SharedIndexInformer,
	pvcInformer cache.SharedIndexInformer) factory.Controller {

	c := &BackupGarbageCollectionController{
		backupsLister:         backupsLister,
		jobsLister:            jobsLister,
		nodesLister:           nodesLister,
		pvcsLister:            pvcsLister,
		operatorClient:        operatorClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		featureGateAccessor:   accessor,
		retryQueue:            newGCRetryQueue(),
		metrics:               metrics,
	}

	syncer := health.NewCheckingSyncWrapper(c.sync, 30*time.Minute) // GC runs infrequently if no backups are deleted
	livenessChecker.Add("BackupGarbageCollectionController", syncer)

	return factory.New().
		WithFilteredEventsInformers(func(o any) bool {
			if backup, ok := o.(*operatorv1alpha1.EtcdBackup); ok {
				return backup.DeletionTimestamp != nil && slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup)
			}
			if job, ok := o.(*batchv1.Job); ok {
				// Only trigger sync on GC jobs when they have finalizer and are completed or failed
				return job.Namespace == operatorclient.TargetNamespace &&
					job.Labels != nil &&
					job.Labels["app"] == backupGCAppName &&
					slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) &&
					isJobFinished(job)
			}
			return false
		}, backupInformer, jobInformer).
		WithBareInformers(nodeInformer, pvcInformer).
		ResyncEvery(10*time.Minute).
		WithSync(syncer.Sync).
		ToController("BackupGarbageCollectionController", eventRecorder.WithComponentSuffix("backup-garbage-collection-controller"))
}

func (c *BackupGarbageCollectionController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupGarbageCollectionController error while checking feature flags: %v", err)
		}
		return nil
	}

	backups, err := c.backupsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("BackupGarbageCollectionController could not list backups: %w", err)
	}

	jobs, err := c.jobsLister.List(labels.Set{"app": backupGCAppName}.AsSelector())
	if err != nil {
		return fmt.Errorf("BackupGarbageCollectionController could not list jobs: %w", err)
	}

	newGC, awaitingFinalizer, errs := c.syncBackups(ctx, backups, jobs)
	jobsByName, requeueAfter, jobErrs := c.syncJobs(ctx, jobs, awaitingFinalizer)
	errs = append(errs, jobErrs...)

	for _, backup := range backups {
		if backoff := c.retryQueue.backoffDuration(backup.UID); backoff > 0 {
			requeueAfter = minDelay(requeueAfter, backoff)
		}
	}

	// Create new GC jobs
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	for storage, backups := range newGC {
		backups = c.filterBackupsForGC(backups)
		if len(backups) == 0 {
			continue
		}

		// Job names are generated deterministically by storage location to avoid issues with informer lag.
		// Only one job per storage location can run at a time.
		jobName := generateBackupGCJobName(storage)
		job, ok := jobsByName[jobName]
		if ok {
			if isJobCompleted(job) {
				if ownsAnyBackup(job, awaitingFinalizer) {
					// Completed job still records backups whose finalizer removal has not been observed, replace once all of its backups are finalized
					continue
				}
			} else if !isJobFailed(job) {
				// Job is still active. Only one GC job is allowed on a storage backend at a time using deterministically generated name.
				continue
			}

			// Job is finished, delete before creating a new one
			if err := deleteJob(ctx, jobsClient, job); err != nil {
				errs = append(errs, fmt.Errorf("BackupGarbageCollectionController error: %w", err))
				continue
			}
			klog.Infof("BackupGarbageCollectionController deleted job %s for storage backend %s", job.Name, storage.String())
		}

		if err := createGarbageCollectionJob(ctx, c.operatorImagePullSpec, jobsClient, jobName, storage, backups); err != nil {
			errs = append(errs, err)
		}
	}

	// Drop backoff state for backups that no longer exist
	for backupUID := range c.retryQueue.backoffs {
		if _, ok := awaitingFinalizer[backupUID]; !ok {
			c.retryQueue.Forget(backupUID)
		}
	}
	if requeueAfter > 0 {
		syncCtx.Queue().AddAfter(syncCtx.QueueKey(), requeueAfter)
	}
	return utilerrors.NewAggregate(errs)
}

// syncBackups removes the finalizer from deleted backups that need no further garbage collection, and returns the
// backups that require a new GC job grouped by storage backend. awaitingFinalizer holds the UIDs of every deleted
// backup observed with the backup finalizer, including those whose finalizer was removed by this sync, since the
// removal has not been observed yet.
func (c *BackupGarbageCollectionController) syncBackups(ctx context.Context, backups []*operatorv1alpha1.EtcdBackup, jobs []*batchv1.Job) (newGC map[storageBackend][]*operatorv1alpha1.EtcdBackup, awaitingFinalizer map[types.UID]struct{}, errs []error) {
	newGC = map[storageBackend][]*operatorv1alpha1.EtcdBackup{}
	awaitingFinalizer = map[types.UID]struct{}{}
	backupsClient := c.operatorClient.EtcdBackups()
	activeBackupGC := map[types.UID]bool{}
	for _, job := range jobs {
		// Backups in a failed job are queued for GC again, not considered active
		if !isJobFailed(job) {
			completed := isJobCompleted(job)
			for _, ownerRef := range job.OwnerReferences {
				if ownerRef.Kind == "EtcdBackup" {
					activeBackupGC[ownerRef.UID] = completed
				}
			}
		}
	}

	for _, backup := range backups {
		if backup.DeletionTimestamp == nil || !slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			continue
		}
		awaitingFinalizer[backup.UID] = struct{}{}

		if completed, ok := activeBackupGC[backup.UID]; ok {
			// Remove backup finalizer if GC completed successfully
			if completed {
				if err := removeBackupFinalizer(ctx, backupsClient, backup); err != nil && !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("BackupGarbageCollectionController failed to remove backup finalizer after GC completed: %w", err))
				}
				c.metrics.deleteBackup(*backup)
			}
		} else if backuphelpers.IsBackupFinished(backup) {
			if storage, requiresGC, err := isGarbageCollectionRequired(c.nodesLister, c.pvcsLister, backup); err != nil {
				errs = append(errs, err)
			} else if requiresGC {
				newGC[storage] = append(newGC[storage], backup)
			} else if err := removeBackupFinalizer(ctx, backupsClient, backup); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("BackupGarbageCollectionController failed to remove backup finalizer: %w", err))
			} else {
				c.metrics.deleteBackup(*backup)
			}
		}
	}
	return newGC, awaitingFinalizer, errs
}

func (c *BackupGarbageCollectionController) syncJobs(ctx context.Context, jobs []*batchv1.Job, awaitingFinalizer map[types.UID]struct{}) (jobsByName map[string]*batchv1.Job, requeueAfter time.Duration, errs []error) {
	// Finalize completed jobs so their TTL can clean them up. Failed jobs keep the finalizer until a retry replaces them.
	jobsByName = map[string]*batchv1.Job{}
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	for _, job := range jobs {
		var shouldFinalize bool
		if isJobCompleted(job) {
			// Finalizing drops the job's owner references, which record the backups it collected. Keep them until the
			// removal of every owning backup's finalizer is observed, otherwise those backups would be garbage collected
			// again.
			shouldFinalize = !ownsAnyBackup(job, awaitingFinalizer)
			if !shouldFinalize && slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
				requeueAfter = minDelay(requeueAfter, gcFinalizeRequeueDelay)
			}
		} else if failedTime, isFailed := jobFailedTime(job); isFailed {
			if !ownsAnyBackup(job, awaitingFinalizer) {
				// None of the job's backups still require GC, so it will never be retried
				shouldFinalize = true
			} else {
				// Add backoff for any backups belonging to a failed job
				delay := c.backoffFailedBackupGC(job.UID, failedTime, job.OwnerReferences)
				requeueAfter = minDelay(requeueAfter, delay)
			}
		}

		if shouldFinalize {
			for _, ownerRef := range job.OwnerReferences {
				if ownerRef.Kind == "EtcdBackup" {
					c.retryQueue.Forget(ownerRef.UID)
				}
			}
			if finalizedJob, err := finalizeJob(ctx, jobsClient, job, true); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				errs = append(errs, fmt.Errorf("BackupGarbageCollectionController error: %w", err))
			} else {
				job = finalizedJob
			}
		}
		jobsByName[job.Name] = job
	}
	return jobsByName, requeueAfter, errs
}

func (c *BackupGarbageCollectionController) backoffFailedBackupGC(jobUID types.UID, failedTime time.Time, ownerRefs []v1.OwnerReference) (requeueAfter time.Duration) {
	for _, ownerRef := range ownerRefs {
		if ownerRef.Kind == "EtcdBackup" {
			// Check if backoff was already recorded for this job failure, else increment
			if backoff, ok := c.retryQueue.backoffs[ownerRef.UID]; !ok || backoff.jobUID != jobUID {
				duration := c.retryQueue.add(ownerRef.UID, jobUID, failedTime)
				klog.V(2).Infof("BackupGarbageCollectionController backoff failed backup GC %s, retry in %s", ownerRef.Name, duration.String())
				requeueAfter = minDelay(requeueAfter, duration)
			}
		}
	}
	return requeueAfter
}

// filterBackupsForGC removes backups that are in backoff, and separates the backups that have tried and failed GC from those that haven't.
// New unattempted GC is always prioritized as a batch, so that many files can be deleted on the same storage backend together.
// If the only backups remaining previously failed GC, they are attempted one at a time to reduce the scope of failure.
func (c *BackupGarbageCollectionController) filterBackupsForGC(backups []*operatorv1alpha1.EtcdBackup) []*operatorv1alpha1.EtcdBackup {
	n := 0
	for _, backup := range backups {
		if c.retryQueue.backoffDuration(backup.UID) <= 0 {
			backups[n] = backup
			n++
		}
	}
	if n == 0 {
		return nil
	}
	backups = backups[:n]

	// Sort backups by number of requeues, so backups that have never attempted GC run first.
	// Fallback to deletion time, then name for deterministic sort order.
	slices.SortFunc(backups, func(a, b *operatorv1alpha1.EtcdBackup) int {
		aRequeues := c.retryQueue.NumRequeues(a.UID)
		bRequeues := c.retryQueue.NumRequeues(b.UID)
		if aRequeues < bRequeues {
			return -1
		} else if aRequeues > bRequeues {
			return 1
		}
		if cmp := a.DeletionTimestamp.Compare(b.DeletionTimestamp.Time); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Name, b.Name)
	})
	if c.retryQueue.NumRequeues(backups[0].UID) > 0 {
		// Backups that previously failed GC are retried individually
		backups = backups[:1]
	} else {
		// Backups that haven't previously failed GC run as a group
		if len(backups) > gcMaxBackupsPerJob {
			backups = backups[:gcMaxBackupsPerJob]
		}
		for i, backup := range backups {
			if c.retryQueue.NumRequeues(backup.UID) > 0 {
				backups = backups[:i]
				break
			}
		}
	}
	return backups
}

// ownsAnyBackup reports whether any of the job's EtcdBackup owner references is in backupUIDs.
func ownsAnyBackup(job *batchv1.Job, backupUIDs map[types.UID]struct{}) bool {
	for _, ownerRef := range job.OwnerReferences {
		if _, ok := backupUIDs[ownerRef.UID]; ok && ownerRef.Kind == "EtcdBackup" {
			return true
		}
	}
	return false
}

func createGarbageCollectionJob(ctx context.Context,
	operatorImagePullSpec string,
	jobsClient batchv1client.JobInterface,
	name string,
	storage storageBackend,
	backups []*operatorv1alpha1.EtcdBackup) error {
	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return fmt.Errorf("BackupGarbageCollectionController could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), bindata.MustAsset("etcd/cluster-backup-gc-job.yaml"))
	if err != nil {
		return fmt.Errorf("BackupGarbageCollectionController could not decode batchv1 job scheme: %w", err)
	}

	job := obj.(*batchv1.Job)
	job.Name = name
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	job.Labels["app"] = backupGCAppName
	job.Finalizers = append(job.Finalizers, backuphelpers.FinalizerEtcdBackup)
	job.Spec.TTLSecondsAfterFinished = new(int32(gcJobFinishedTTL.Seconds()))
	job.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec
	job.Spec.Template.Spec.Containers[0].Args = append(job.Spec.Template.Spec.Containers[0].Args, "--backupPath="+backuphelpers.BackupPathMount)
	if storage.storageType == operatorv1alpha1.EtcdBackupStorageTypeLocal {
		job.Spec.Template.Spec.NodeName = storage.nodeName
	}

	var env corev1.EnvVar
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	backups, env, volumes, volumeMounts = backupFileMounts(storage, backups)
	job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, env)
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, volumes...)
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, volumeMounts...)
	for _, backup := range backups {
		job.OwnerReferences = append(job.OwnerReferences, v1.OwnerReference{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "EtcdBackup",
			Name:       backup.Name,
			UID:        backup.UID,
		})
	}

	klog.Infof("BackupGarbageCollectionController starts backup GC as job [%s]", job.Name)
	_, err = jobsClient.Create(ctx, job, v1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(2).Infof("BackupGarbageCollectionController name conflict on job [%s]", job.Name)
			return nil
		}
		return fmt.Errorf("BackupGarbageCollectionController failed to create job: %w", err)
	}

	return nil
}

func backupFileMounts(storage storageBackend, backups []*operatorv1alpha1.EtcdBackup) ([]*operatorv1alpha1.EtcdBackup, corev1.EnvVar, []corev1.Volume, []corev1.VolumeMount) {
	env := corev1.EnvVar{Name: backupGCFilesEnvName}
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	// Add 1 to max arg bytes since leading "," is trimmmed at the end.
	maxArgLen := gcMaxFileArgBytes + 1

	mountedHostPaths := map[string]int{}
	var hostPaths []string
	for i, backup := range backups {
		var dir, hostPath string
		switch backup.Spec.Storage.Type {
		case operatorv1alpha1.EtcdBackupStorageTypeLocal:
			hostPath = backuphelpers.LocalBackupHostPath(backup.Spec.Storage.Local)
			idx, ok := mountedHostPaths[hostPath]
			if !ok {
				// hostPath is added to mountedHostPaths after verifying this backup's files fit in maxArgLen
				idx = len(hostPaths)
			}
			dir = filepath.Join(strconv.Itoa(idx), backup.Name)
		case operatorv1alpha1.EtcdBackupStorageTypePVC:
			dir = filepath.Join(filepath.Clean(backup.Spec.Storage.PVC.Path), backup.Name)
		}

		backupFiles := ""
		if len(backup.Status.Files) > 0 {
			for _, file := range backup.Status.Files {
				filePath := filepath.Join(dir, filepath.Base(file.Path))
				backupFiles += "," + filePath
			}
		} else {
			// Backup files will be discovered inside dir
			backupFiles = "," + dir
		}

		if i > 0 && len(env.Value)+len(backupFiles) > maxArgLen {
			backups = backups[:i]
			break
		} else {
			env.Value += backupFiles
			if hostPath != "" {
				if _, ok := mountedHostPaths[hostPath]; !ok {
					mountedHostPaths[hostPath] = len(hostPaths)
					hostPaths = append(hostPaths, hostPath)
				}
			}
		}
	}

	switch storage.storageType {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		for i, hostPath := range hostPaths {
			strIdx := strconv.Itoa(i)
			name := "etc-kubernetes-cluster-backup-" + strIdx
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: hostPath,
						Type: ptr.To(corev1.HostPathDirectoryOrCreate),
					},
				},
			})
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      name,
				MountPath: filepath.Join(backuphelpers.BackupPathMount, strIdx),
			})
		}
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		volumes = append(volumes, corev1.Volume{
			Name: "etc-kubernetes-cluster-backup",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: storage.pvcName,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "etc-kubernetes-cluster-backup",
			MountPath: backuphelpers.BackupPathMount,
		})
	}

	env.Value = strings.TrimPrefix(env.Value, ",")
	return backups, env, volumes, volumeMounts
}

func isGarbageCollectionRequired(
	nodesLister corev1listers.NodeLister,
	pvcsLister corev1listers.PersistentVolumeClaimNamespaceLister,
	backup *operatorv1alpha1.EtcdBackup,
) (storageBackend, bool, error) {
	// Check if new GC Job should be created
	storage := storageBackend{storageType: backup.Spec.Storage.Type}

	// Skip GC if backup failed gracefully and created no files
	// If unknown, assume that GC should be run.
	if len(backup.Status.Files) == 0 {
		for _, condition := range backup.Status.Conditions {
			if condition.Type == string(operatorv1alpha1.BackupFailed) {
				if condition.Status == metav1.ConditionTrue && condition.Reason == string(operatorv1alpha1.BackupReasonSetupFailed) {
					return storage, false, nil
				}
				break
			}
		}
	}

	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		storage.nodeName = backup.Status.NodeName
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		storage.pvcName = backup.Spec.Storage.PVC.Name
	}

	gcRequired, err := storageBackendExists(nodesLister, pvcsLister, storage)
	return storage, gcRequired, err
}

func storageBackendExists(
	nodesLister corev1listers.NodeLister,
	pvcsLister corev1listers.PersistentVolumeClaimNamespaceLister,
	storage storageBackend) (bool, error) {
	switch storage.storageType {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		// Check if node still exists
		if _, err := nodesLister.Get(storage.nodeName); err != nil {
			if apierrors.IsNotFound(err) {
				klog.Infof("BackupGarbageCollectionController node %s not found, no GC required", storage.nodeName)
				err = nil
			}
			return false, err
		}
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		// Check if PVC still exists
		if _, err := pvcsLister.Get(storage.pvcName); err != nil {
			if apierrors.IsNotFound(err) {
				klog.Infof("BackupGarbageCollectionController PVC %s not found, no GC required", storage.pvcName)
				err = nil
			}
			return false, err
		}
	default:
		klog.Infof("BackupGarbageCollectionController unknown storage type %s, no GC required", storage.storageType)
		return false, nil
	}
	return true, nil
}

func newGCRetryQueue() gcRetryQueue {
	return gcRetryQueue{
		TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[types.UID](gcBackoffBase, gcBackoffMax),
		backoffs:         map[types.UID]gcBackoff{},
	}
}

type gcRetryQueue struct {
	workqueue.TypedRateLimiter[types.UID]
	// backoffs is keyed by backup UID. Access is not synchronized, the controller runs a single worker.
	backoffs map[types.UID]gcBackoff
}

func (grq *gcRetryQueue) Forget(backupUID types.UID) {
	grq.TypedRateLimiter.Forget(backupUID)
	delete(grq.backoffs, backupUID)
}

func (grq *gcRetryQueue) add(backupUID, jobUID types.UID, failedAt time.Time) time.Duration {
	retryAt := failedAt.Add(grq.When(backupUID))
	grq.backoffs[backupUID] = gcBackoff{jobUID: jobUID, retryAt: retryAt}
	return time.Until(retryAt)
}

// backoffDuration returns the remaining backoff for running GC on an EtcdBackup
func (grq *gcRetryQueue) backoffDuration(backupUID types.UID) time.Duration {
	if backoff, ok := grq.backoffs[backupUID]; ok {
		return time.Until(backoff.retryAt)
	}
	return 0
}

// gcBackoff records the failed GC job that was counted against the rate limiter, so that a failed job is only
// counted once no matter how many times it is observed.
type gcBackoff struct {
	jobUID  types.UID
	retryAt time.Time
}

type storageBackend struct {
	storageType operatorv1alpha1.EtcdBackupStorageType
	nodeName    string
	pvcName     string
}

func (sb storageBackend) String() string {
	switch sb.storageType {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		return "local/" + sb.nodeName
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		return "pvc/" + sb.pvcName
	default:
		return "unknown"
	}
}

// generateBackupGCJobName creates a hash-based name for deduplication
func generateBackupGCJobName(storage storageBackend) string {
	prefix := "backup-gc"
	var name, suffix string
	switch storage.storageType {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		name = "local-" + strings.ReplaceAll(storage.nodeName, ".", "-")
		suffix = shortHash(string(storage.storageType), storage.nodeName)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		name = "pvc-" + storage.pvcName
		suffix = shortHash(string(storage.storageType), storage.pvcName)
	default:
		name = "unknown"
		suffix = shortHash("unknown")
	}
	return joinName(prefix, name, suffix)
}
