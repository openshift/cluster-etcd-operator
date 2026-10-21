package backupcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
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
	backupGCAppName           = "cluster-backup-gc-job"
	backupGCFilesEnvName      = "CLUSTER_BACKUP_GC_FILES"
	gcTtlSecondsAfterFinished = int32(2 * 60 * 60)
	gcMaxBackupsPerJob        = 100
	gcBackoffBase             = 30 * time.Second
	gcBackoffMax              = 30 * time.Minute
)

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

	newGC, err := c.syncBackups(ctx, activeBackupGC)
	if err != nil {
		return err
	}

	// Finalize completed jobs so their TTL can clean them up. Failed jobs keep the finalizer until a retry replaces them.
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	for key, job := range jobsByStorage {
		if isJobCompleted(job) {
			c.retryQueue.Forget(key)
			finalizedJob, err := finalizeJob(ctx, jobsClient, job, true)
			if err != nil {
				if apierrors.IsNotFound(err) {
					delete(jobsByStorage, key)
					continue
				}
				return fmt.Errorf("BackupGarbageCollectionController error: %w", err)
			}
			jobsByStorage[key] = finalizedJob
		}
	}

	// Create new GC jobs
	for storage, backups := range newGC {
		slices.SortFunc(backups, func(a, b *operatorv1alpha1.EtcdBackup) int {
			return strings.Compare(a.Name, b.Name)
		})
		if len(backups) > gcMaxBackupsPerJob {
			// Remaining backups will be garbage collected after the current job completes
			backups = backups[:gcMaxBackupsPerJob]
		}

		storageKey := storage.String()
		if job, ok := jobsByStorage[storageKey]; ok {
			// Only one GC job is allowed on a storage backend at a time using deterministically generated name.
			if !isJobFinished(job) {
				// Remaining backups will be garbage collected after the active job completes
				continue
			}
			if isJobFailed(job) {
				// Exponential backoff on failed GC jobs
				if duration := c.retryQueue.backoffDuration(storageKey, job); duration > 0 {
					klog.Infof("BackupGarbageCollectionController backoff failed job %s, retry in %s", job.Name, duration.String())
					syncCtx.Queue().AddAfter(syncCtx.QueueKey(), duration)
					continue
				}
			}
			// Job is finished and no backoff required, delete before creating a new one
			if err := deleteJob(ctx, jobsClient, job); err != nil {
				return fmt.Errorf("BackupGarbageCollectionController error: %w", err)
			}
			klog.Infof("BackupGarbageCollectionController deleted job %s for storage backend %s", job.Name, storageKey)
		}

		if err := createGarbageCollectionJob(ctx, c.operatorImagePullSpec, jobsClient, storage, backups); err != nil {
			return err
		}
	}

	// Drop backoff state for storage backends that no longer have any GC job
	for key := range c.retryQueue.backoffs {
		if _, ok := jobsByStorage[key]; !ok {
			c.retryQueue.Forget(key)
		}
	}
	return nil
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

func (c *BackupGarbageCollectionController) syncBackups(ctx context.Context, activeGC map[types.UID]bool) (map[storageBackend][]*operatorv1alpha1.EtcdBackup, error) {
	newGC := map[storageBackend][]*operatorv1alpha1.EtcdBackup{}
	backupsClient := c.operatorClient.EtcdBackups()
	backups, err := c.backupsLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, backup := range backups {
		if backup.DeletionTimestamp == nil || !slices.Contains(backup.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			continue
		}

		if completed, ok := activeGC[backup.UID]; ok {
			// Remove backup finalizer if GC completed successfully
			if completed {
				if _, err := applyBackupFinalizer(ctx, backupsClient, backup, false); err != nil {
					return nil, fmt.Errorf("BackupGarbageCollectionController failed to remove backup finalizer after GC completed: %w", err)
				}
			}
		} else if backuphelpers.IsBackupFinished(backup) {
			if storage, requiresGC, err := isGarbageCollectionRequired(c.nodesLister, c.pvcsLister, backup); err != nil {
				return nil, err
			} else if requiresGC {
				newGC[storage] = append(newGC[storage], backup)
			} else if _, err := applyBackupFinalizer(ctx, backupsClient, backup, false); err != nil {
				return nil, fmt.Errorf("BackupGarbageCollectionController failed to remove backup finalizer: %w", err)
			}
		}
	}
	return newGC, nil
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
	for _, backup := range backups {
		job.OwnerReferences = append(job.OwnerReferences, v1.OwnerReference{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "EtcdBackup",
			Name:       backup.Name,
			UID:        backup.UID,
		})
	}
	job.Finalizers = append(job.Finalizers, backuphelpers.FinalizerEtcdBackup)
	job.Spec.TTLSecondsAfterFinished = new(gcTtlSecondsAfterFinished)
	job.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec

	gcFiles := make([]string, 0, 2*len(backups))
	switch storage.storageType {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		job.Spec.Template.Spec.NodeName = storage.nodeName
		delete(job.Spec.Template.Spec.NodeSelector, backuphelpers.ControlPlaneNodeLabelSelector)
		klog.V(4).Infof("BackupGarbageCollectionController assigned job [%s] to node [%s]", job.Name, storage.nodeName)

		paths := map[string]struct{}{}
		for _, backup := range backups {
			hostPath := backup.Spec.Storage.Local.HostPath
			if len(backup.Status.Files) > 0 {
				for _, file := range backup.Status.Files {
					gcFiles = append(gcFiles, filepath.Join(backupPathMount, file.Path))
				}
			} else {
				// Infer file directory from host path and backup name
				gcFiles = append(gcFiles, filepath.Join(backupPathMount, hostPath, backup.Name))
			}

			if _, ok := paths[hostPath]; !ok {
				name := fmt.Sprintf("etc-kubernetes-cluster-backup-%d", len(paths))
				paths[hostPath] = struct{}{}
				job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
					Name: name,
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: hostPath,
							Type: ptr.To(corev1.HostPathDirectoryOrCreate),
						},
					},
				})
				job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
					Name:      name,
					MountPath: filepath.Join(backupPathMount, hostPath),
				})
			}
		}
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		for _, backup := range backups {
			if len(backup.Status.Files) > 0 {
				for _, file := range backup.Status.Files {
					gcFiles = append(gcFiles, filepath.Join(backupPathMount, file.Path))
				}
			} else {
				// Infer file directory from pvc path and backup name
				gcFiles = append(gcFiles, filepath.Join(backupPathMount, backup.Spec.Storage.PVC.Path, backup.Name))
			}
		}

		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "etc-kubernetes-cluster-backup",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: storage.pvcName,
				},
			},
		})
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      "etc-kubernetes-cluster-backup",
			MountPath: backupPathMount,
		})
	default:
		return fmt.Errorf("unknown storage backend: %s", storage.storageType)
	}

	job.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: backupGCFilesEnvName, Value: strings.Join(gcFiles, " ")}}

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
		// Check if node still exists
		if _, err := nodesLister.Get(backup.Status.NodeName); err != nil {
			if apierrors.IsNotFound(err) {
				klog.Infof("BackupGarbageCollectionController node %s not found, ignoring GC for etcdbackup %s", backup.Status.NodeName, backup.Name)
				err = nil
			}
			return storage, false, err
		}
		storage.nodeName = backup.Status.NodeName
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		// Check if PVC still exists
		if _, err := pvcsLister.Get(backup.Spec.Storage.PVC.Name); err != nil {
			if apierrors.IsNotFound(err) {
				klog.Infof("BackupGarbageCollectionController PVC %s not found, ignoring GC for etcdbackup %s", backup.Spec.Storage.PVC.Name, backup.Name)
				err = nil
			}
			return storage, false, err
		}
		storage.pvcName = backup.Spec.Storage.PVC.Name
	default:
		klog.Infof("BackupGarbageCollectionController unknown storage type %s, ignoring GC for etcdbackup %s", backup.Spec.Storage.Type, backup.Name)
		return storage, false, nil
	}

	return storage, true, nil
}

// finalizeJob removes the backup finalizer from the job, returning the patched job.
func finalizeJob(ctx context.Context, jobsClient batchv1client.JobInterface, job *batchv1.Job, removeOwnerRefs bool) (*batchv1.Job, error) {
	if slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
		metadata := map[string]any{
			"$deleteFromPrimitiveList/finalizers": []string{backuphelpers.FinalizerEtcdBackup},
		}
		if removeOwnerRefs {
			ownerReferencesPatch := []map[string]any{}
			for _, ownerRef := range job.OwnerReferences {
				if ownerRef.Kind == "EtcdBackup" {
					ownerReferencesPatch = append(ownerReferencesPatch, map[string]any{
						"uid":    ownerRef.UID,
						"$patch": "delete",
					})
				}
			}
			if len(ownerReferencesPatch) > 0 {
				metadata["ownerReferences"] = ownerReferencesPatch
			}
		}

		patchData, err := json.Marshal(map[string]any{"metadata": metadata})
		if err != nil {
			return nil, fmt.Errorf("error marshalling job %q patch: %w", job.Name, err)
		}
		patchedJob, err := jobsClient.Patch(ctx, job.Name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
		if err != nil {
			return nil, fmt.Errorf("error finalizing job: %w", err)
		}
		return patchedJob, nil
	}
	return job, nil
}

func deleteJob(ctx context.Context, jobsClient batchv1client.JobInterface, job *batchv1.Job) (err error) {
	if _, err := finalizeJob(ctx, jobsClient, job, true); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := jobsClient.Delete(ctx, job.Name, v1.DeleteOptions{PropagationPolicy: ptr.To(v1.DeletePropagationBackground)}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete job %s: %w", job.Name, err)
	}
	return nil
}

func isJobCompleted(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobFailedTime returns when the job failed, falling back to the creation time if unknown.
func jobFailedTime(job *batchv1.Job) time.Time {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() {
			return condition.LastTransitionTime.Time
		}
	}
	return job.CreationTimestamp.Time
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
	prefix := "backup-gc-"
	var name, suffix string
	switch storage.storageType {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		name = "local-" + strings.ReplaceAll(storage.nodeName, ".", "-")
		suffix = "-" + shortHash(string(storage.storageType), storage.nodeName)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		name = "pvc-" + storage.pvcName
		suffix = "-" + shortHash(string(storage.storageType), storage.pvcName)
	default:
		name = "unknown"
		suffix = "-" + shortHash("unknown")
	}
	remaining := maxNameLength - len(prefix) - len(suffix)
	if len(name) > remaining {
		name = strings.TrimRight(name[:remaining], "-")
	}
	return prefix + name + suffix
}
