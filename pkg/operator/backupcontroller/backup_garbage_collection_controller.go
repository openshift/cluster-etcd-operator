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

	// retryQueue tracks failed GC jobs per storage backend for exponential backoff on retries
	// Backoff is in-memory only, so retries may happen earlier than expected after an operator restart.
	retryQueue gcRetryQueue
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

	jobsByStorage, activeBackupGC, err := c.collectJobs()
	if err != nil {
		return err
	}

	var errs []error
	newGC, awaitingFinalizer, err := c.syncBackups(ctx, activeBackupGC)
	if err != nil {
		errs = append(errs, err)
	}

	// Finalize completed jobs so their TTL can clean them up. Failed jobs keep the finalizer until a retry replaces them.
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	var requeueAfter time.Duration
	for key, job := range jobsByStorage {
		var shouldFinalize bool
		if isJobCompleted(job) {
			// Finalizing drops the job's owner references, which record the backups it collected. Keep them until the
			// removal of every owning backup's finalizer is observed, otherwise those backups would be garbage collected
			// again.
			shouldFinalize = !ownsAnyBackup(job, awaitingFinalizer)
			if !shouldFinalize && slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
				requeueAfter = minDelay(requeueAfter, gcFinalizeRequeueDelay)
			}
		} else if isJobFailed(job) {
			if !ownsAnyBackup(job, awaitingFinalizer) {
				// None of the job's backups still require GC, so it will never be retried
				shouldFinalize = true
			} else if storage, ok := parseStorageKey(key); ok {
				storageExists, err := storageBackendExists(c.nodesLister, c.pvcsLister, storage)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				shouldFinalize = !storageExists
			} else {
				shouldFinalize = true
			}
		}

		if shouldFinalize {
			c.retryQueue.Forget(key)
			finalizedJob, err := finalizeJob(ctx, jobsClient, job, true)
			if err != nil {
				if apierrors.IsNotFound(err) {
					delete(jobsByStorage, key)
					continue
				}
				errs = append(errs, fmt.Errorf("BackupGarbageCollectionController error: %w", err))
				continue
			}
			jobsByStorage[key] = finalizedJob
		}
	}

	// Create new GC jobs
	for storage, backups := range newGC {
		slices.SortFunc(backups, func(a, b *operatorv1alpha1.EtcdBackup) int {
			return strings.Compare(a.Name, b.Name)
		})

		storageKey := storage.String()
		if job, ok := jobsByStorage[storageKey]; ok {
			if isJobCompleted(job) {
				if ownsAnyBackup(job, awaitingFinalizer) {
					// Completed job still records backups whose finalizer removal has not been observed, replace it once it is
					continue
				}
			} else if isJobFailed(job) {
				// Exponential backoff on failed GC jobs
				if duration := c.retryQueue.backoffDuration(storageKey, job); duration > 0 {
					klog.Infof("BackupGarbageCollectionController backoff failed job %s, retry in %s", job.Name, duration.String())
					syncCtx.Queue().AddAfter(syncCtx.QueueKey(), duration)
					continue
				}
			} else {
				// Job is still active. Only one GC job is allowed on a storage backend at a time using deterministically generated name.
				continue
			}

			// Job is finished and no backoff required, delete before creating a new one
			if err := deleteJob(ctx, jobsClient, job); err != nil {
				errs = append(errs, fmt.Errorf("BackupGarbageCollectionController error: %w", err))
				continue
			}
			klog.Infof("BackupGarbageCollectionController deleted job %s for storage backend %s", job.Name, storageKey)
		}

		if err := createGarbageCollectionJob(ctx, c.operatorImagePullSpec, jobsClient, storage, backups); err != nil {
			errs = append(errs, err)
		}
	}

	// Drop backoff state for storage backends that no longer have any GC job
	for key := range c.retryQueue.backoffs {
		if _, ok := jobsByStorage[key]; !ok {
			c.retryQueue.Forget(key)
		}
	}
	if requeueAfter > 0 {
		syncCtx.Queue().AddAfter(syncCtx.QueueKey(), requeueAfter)
	}
	return utilerrors.NewAggregate(errs)
}

// collectJobs indexes GC jobs by storage backend, and reports whether GC of each backup in a completed or active job
// has completed. Job names are deterministic per storage backend, so there is at most one job per backend.
func (c *BackupGarbageCollectionController) collectJobs() (jobsByStorage map[string]*batchv1.Job, activeBackupGC map[types.UID]bool, err error) {
	jobs, err := c.jobsLister.List(labels.Set{"app": backupGCAppName}.AsSelector())
	if err != nil {
		return nil, nil, fmt.Errorf("BackupGarbageCollectionController could not list jobs: %w", err)
	}

	jobsByStorage = make(map[string]*batchv1.Job, len(jobs))
	activeBackupGC = map[types.UID]bool{}
	for _, job := range jobs {
		jobsByStorage[job.Annotations[backuphelpers.AnnotationBackupStorage]] = job
		if isJobFailed(job) {
			// Backups in a failed job are queued for GC again
			continue
		}
		completed := isJobCompleted(job)
		for _, ownerRef := range job.OwnerReferences {
			if ownerRef.Kind == "EtcdBackup" {
				activeBackupGC[ownerRef.UID] = completed
			}
		}
	}
	return jobsByStorage, activeBackupGC, nil
}

// syncBackups removes the finalizer from deleted backups that need no further garbage collection, and returns the
// backups that require a new GC job grouped by storage backend. awaitingFinalizer holds the UIDs of every deleted
// backup observed with the backup finalizer, including those whose finalizer was removed by this sync, since the
// removal has not been observed yet.
func (c *BackupGarbageCollectionController) syncBackups(ctx context.Context, activeGC map[types.UID]bool) (newGC map[storageBackend][]*operatorv1alpha1.EtcdBackup, awaitingFinalizer map[types.UID]struct{}, err error) {
	newGC = map[storageBackend][]*operatorv1alpha1.EtcdBackup{}
	awaitingFinalizer = map[types.UID]struct{}{}
	backupsClient := c.operatorClient.EtcdBackups()
	backups, err := c.backupsLister.List(labels.Everything())
	if err != nil {
		return nil, nil, err
	}

	var errs []error
	for _, backup := range backups {
		if backup.DeletionTimestamp == nil || !slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			continue
		}
		awaitingFinalizer[backup.UID] = struct{}{}

		if completed, ok := activeGC[backup.UID]; ok {
			// Remove backup finalizer if GC completed successfully
			if completed {
				if err := removeBackupFinalizer(ctx, backupsClient, backup); err != nil && !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("BackupGarbageCollectionController failed to remove backup finalizer after GC completed: %w", err))
				}
			}
		} else if backuphelpers.IsBackupFinished(backup) {
			if storage, requiresGC, err := isGarbageCollectionRequired(c.nodesLister, c.pvcsLister, backup); err != nil {
				errs = append(errs, err)
			} else if requiresGC {
				newGC[storage] = append(newGC[storage], backup)
			} else if err := removeBackupFinalizer(ctx, backupsClient, backup); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("BackupGarbageCollectionController failed to remove backup finalizer: %w", err))
			}
		}
	}
	return newGC, awaitingFinalizer, utilerrors.NewAggregate(errs)
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

	// Job names are generated deterministically by storage location to avoid issues with informer lag.
	// Only one job per storage location can run at a time.
	job := obj.(*batchv1.Job)
	job.Name = generateBackupGCJobName(storage)
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[backuphelpers.AnnotationBackupStorage] = storage.String()
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	job.Labels["app"] = backupGCAppName
	job.Finalizers = append(job.Finalizers, backuphelpers.FinalizerEtcdBackup)
	job.Spec.TTLSecondsAfterFinished = new(int32(gcJobFinishedTTL.Seconds()))
	job.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec
	job.Spec.Template.Spec.Containers[0].Args = append(job.Spec.Template.Spec.Containers[0].Args, "--backupPath="+backuphelpers.BackupPathMount)

	if len(backups) > gcMaxBackupsPerJob {
		// Remaining backups will be garbage collected after the current job completes
		backups = backups[:gcMaxBackupsPerJob]
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
			klog.Infof("BackupGarbageCollectionController name conflict on job [%s]", job.Name)
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
		TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](gcBackoffBase, gcBackoffMax),
		backoffs:         map[string]gcBackoff{},
	}
}

type gcRetryQueue struct {
	workqueue.TypedRateLimiter[string]
	// backoffs is keyed by storage backend. Access is not synchronized, the controller runs a single worker.
	backoffs map[string]gcBackoff
}

func (grq *gcRetryQueue) Forget(storageKey string) {
	grq.TypedRateLimiter.Forget(storageKey)
	delete(grq.backoffs, storageKey)
}

func (grq *gcRetryQueue) add(storageKey string, job *batchv1.Job) gcBackoff {
	backoff := gcBackoff{
		jobUID:  job.UID,
		retryAt: jobFailedTime(job).Add(grq.When(storageKey)),
	}
	grq.backoffs[storageKey] = backoff
	return backoff
}

// backoffDuration returns the remaining backoff before a failed GC job on the storage backend may be replaced.
// Each failed job is counted against the rate limiter once, identified by UID.
func (grq *gcRetryQueue) backoffDuration(storageKey string, job *batchv1.Job) time.Duration {
	if !isJobFailed(job) {
		return 0
	}
	backoff, ok := grq.backoffs[storageKey]
	if !ok || backoff.jobUID != job.UID {
		backoff = grq.add(storageKey, job)
	}
	return time.Until(backoff.retryAt)
}

// gcBackoff records the failed GC job that was counted against the rate limiter, so that a failed job is only
// counted once no matter how many times it is observed.
type gcBackoff struct {
	jobUID  types.UID
	retryAt time.Time
}

func parseStorageKey(storageKey string) (storageBackend, bool) {
	parts := strings.SplitN(storageKey, "/", 2)
	if len(parts) == 2 {
		switch parts[0] {
		case "local":
			return storageBackend{
				storageType: operatorv1alpha1.EtcdBackupStorageTypeLocal,
				nodeName:    parts[1],
			}, true
		case "pvc":
			return storageBackend{
				storageType: operatorv1alpha1.EtcdBackupStorageTypePVC,
				pvcName:     parts[1],
			}, true
		}
	}
	return storageBackend{}, false
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
