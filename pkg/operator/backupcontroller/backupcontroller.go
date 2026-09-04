package backupcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	batchv1listers "k8s.io/client-go/listers/batch/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
)

const (
	backupAppName           = "cluster-backup-job"
	backupPathMount         = "/etc/kubernetes/cluster-backup"
	backupDirEnvName        = "CLUSTER_BACKUP_PATH"
	ttlSecondsAfterFinished = int32(4 * 60 * 60)

	labelBackupName = "operator.openshift.io/etcd-backup"
	labelJobName    = "batch.kubernetes.io/job-name"

	maxNameLength = 63
)

type BackupController struct {
	backupsLister         operatorv1alpha1listers.EtcdBackupLister
	podsLister            corev1listers.PodNamespaceLister
	jobsLister            batchv1listers.JobNamespaceLister
	operatorClient        operatorv1alpha1client.OperatorV1alpha1Interface
	kubeClient            kubernetes.Interface
	operatorImagePullSpec string
	featureGateAccessor   featuregates.FeatureGateAccess
}

func NewBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsLister operatorv1alpha1listers.EtcdBackupLister,
	podsLister corev1listers.PodNamespaceLister,
	jobsLister batchv1listers.JobNamespaceLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	backupInformer factory.Informer,
	jobInformer factory.Informer,
	podInformer factory.Informer) factory.Controller {

	c := &BackupController{
		backupsLister:         backupsLister,
		podsLister:            podsLister,
		jobsLister:            jobsLister,
		operatorClient:        operatorClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		featureGateAccessor:   accessor,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithFilteredEventsInformers(func(obj interface{}) bool {
			if backup, ok := obj.(*operatorv1alpha1.EtcdBackup); ok {
				return backuphelpers.IsBackupActive(backup)
			}
			if job, ok := obj.(*batchv1.Job); ok {
				// Only trigger sync on backup jobs when they have finalizer and are completed or failed
				return job.Namespace == operatorclient.TargetNamespace &&
					job.Labels != nil &&
					job.Labels["app"] == backupAppName &&
					slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) &&
					isJobFinished(job)
			}
			return false
		}, backupInformer, jobInformer).
		WithBareInformers(podInformer).
		WithSync(syncer.Sync).
		ToController("BackupController", eventRecorder.WithComponentSuffix("backup-controller"))
}

func (c *BackupController) sync(ctx context.Context, _ factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupController error while checking feature flags: %v", err)
		}
		return nil
	}

	jobs, err := c.jobsLister.List(labels.SelectorFromSet(labels.Set{"app": backupAppName}))
	if err != nil {
		return fmt.Errorf("BackupController could not list backup jobs, error was: %w", err)
	}
	jobIndexed := indexJobsByBackupLabelName(jobs)

	backups, err := c.backupsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("BackupController could not list etcdbackups CRDs, error was: %w", err)
	}

	backupsClient := c.operatorClient.EtcdBackups()
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	pvcsClient := c.kubeClient.CoreV1().PersistentVolumeClaims(operatorclient.TargetNamespace)
	nodesClient := c.kubeClient.CoreV1().Nodes()

	validatedNodes := map[string]bool{}
	validatedPVCs := map[string]bool{}
	var backupsToRun []*operatorv1alpha1.EtcdBackup
	for _, backup := range backups {
		if job, ok := jobIndexed[backup.Name]; ok {
			klog.V(4).Infof("BackupController backup job with name [%s] found, reconciling status", job.Name)
			err := reconcileJobStatus(ctx, jobsClient, c.podsLister, backupsClient, job, backup)
			if err != nil {
				return fmt.Errorf("BackupController could not reconcile job status for backup [%s]: %w", backup.Name, err)
			}
			continue
		}
		if backup.DeletionTimestamp != nil || !backuphelpers.IsBackupActive(backup) {
			// Ignore backups that were deleted or are not pending or running. They are handled by other controllers.
			continue
		}

		if backup.Status.Job == nil {
			// Backup has just been promoted to Pending by the queue controller
			nodeName := backup.Status.NodeName
			validNode, ok := validatedNodes[nodeName]
			if !ok {
				if validNode, err = isValidNode(ctx, nodesClient, nodeName); err != nil {
					return fmt.Errorf("BackupController could not validate Node [%s]: %w", nodeName, err)
				}
				validatedNodes[nodeName] = validNode
			}
			if !validNode {
				klog.Infof("Skipping backup [%s], node [%s] not found", backup.Name, backup.Status.NodeName)
				if err := markBackupFailed(ctx, backupsClient, backup, operatorv1alpha1.BackupReasonNodeNotFound, fmt.Sprintf("unable to find Node [%s]", nodeName), false); err != nil {
					return err
				}
				continue
			}

			if backup.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
				pvcName := backup.Spec.Storage.PVC.Name
				validPVC, ok := validatedPVCs[pvcName]
				if !ok {
					if validPVC, err = isValidPVC(ctx, pvcsClient, pvcName); err != nil {
						return fmt.Errorf("BackupController could not validate PVC [%s]: %w", pvcName, err)
					}
					validatedPVCs[pvcName] = validPVC
				}
				if !validPVC {
					klog.Infof("Skipping backup [%s], PVC [%s] not found", backup.Name, pvcName)
					if err := markBackupFailed(ctx, backupsClient, backup, operatorv1alpha1.BackupReasonPVCNotFound, fmt.Sprintf("unable to find PVC [%s]", pvcName), false); err != nil {
						return err
					}
					continue
				}
			}
			backupsToRun = append(backupsToRun, backup)
		} else {
			// Backup job was started, but it is not in the cache
			if failedReason, err := reconcileMissingJobStatus(ctx, jobsClient, backup); err != nil {
				return err
			} else if failedReason != "" {
				if err := markBackupFailed(ctx, backupsClient, backup, operatorv1alpha1.BackupReasonJobFailed, failedReason, true); err != nil {
					return err
				}
			}
		}
	}

	if len(backupsToRun) == 0 {
		klog.V(4).Infof("BackupController no backups to reconcile, skipping")
		return nil
	}

	// in case of multiple backups requested, we reconcile in order of their names (also to reduce flakiness in tests)
	slices.SortFunc(backupsToRun, func(a, b *operatorv1alpha1.EtcdBackup) int {
		return strings.Compare(a.Name, b.Name)
	})

	for _, backup := range backupsToRun {
		klog.V(4).Infof("BackupController processing EtcdBackup %s", backup.Name)
		if err := createBackupJob(ctx, backup, c.operatorImagePullSpec, jobsClient, backupsClient); err != nil {
			return fmt.Errorf("BackupController failed to create backup job: %w", err)
		}
	}

	return nil
}

func createBackupJob(ctx context.Context,
	backup *operatorv1alpha1.EtcdBackup,
	operatorImagePullSpec string,
	jobClient batchv1client.JobInterface,
	backupClient operatorv1alpha1client.EtcdBackupInterface) error {

	if !slices.Contains(backup.ObjectMeta.Finalizers, backuphelpers.FinalizerEtcdBackup) {
		backup.ObjectMeta.Finalizers = append(backup.ObjectMeta.Finalizers, backuphelpers.FinalizerEtcdBackup)
		updatedBackup, err := backupClient.Update(ctx, backup, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		backup = updatedBackup
	}

	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return fmt.Errorf("could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), bindata.MustAsset("etcd/cluster-backup-job.yaml"))
	if err != nil {
		return fmt.Errorf("could not decode batchv1 job scheme: %w", err)
	}

	jobName := backup.Name
	job := obj.(*batchv1.Job)
	job.Name = jobName
	job.Labels[labelBackupName] = backup.Name
	job.OwnerReferences = append(job.OwnerReferences, metav1.OwnerReference{
		APIVersion: operatorv1alpha1.GroupVersion.String(),
		Kind:       "EtcdBackup",
		Name:       backup.Name,
		UID:        backup.UID,
	})
	job.Finalizers = append(job.Finalizers, backuphelpers.FinalizerEtcdBackup)
	job.Spec.TTLSecondsAfterFinished = ptr.To(ttlSecondsAfterFinished)
	job.Spec.Template.Spec.InitContainers[0].Image = operatorImagePullSpec
	job.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec

	job.Spec.Template.Spec.NodeName = backup.Status.NodeName
	klog.V(4).Infof("BackupController assigned job [%s] to node [%s]", job.Name, backup.Status.NodeName)

	backupDir := backupPathMount
	volume := corev1.Volume{Name: "etc-kubernetes-cluster-backup"}
	volumeMount := corev1.VolumeMount{Name: "etc-kubernetes-cluster-backup", MountPath: backupPathMount}
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		storageLocal := backup.Spec.Storage.Local
		backupDir = filepath.Join(backupDir, storageLocal.HostPath)
		volume.HostPath = &corev1.HostPathVolumeSource{
			Path: storageLocal.HostPath,
			Type: ptr.To(corev1.HostPathDirectoryOrCreate),
		}
		// HostPath is appended to mount so that path handling is always consistent between local and pvc storage backend
		volumeMount.MountPath = filepath.Join(volumeMount.MountPath, storageLocal.HostPath)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		storagePVC := backup.Spec.Storage.PVC
		backupDir = filepath.Join(backupDir, storagePVC.Path)
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: storagePVC.Name,
		}
	default:
		return fmt.Errorf("unknown storage backend: %s", backup.Spec.Storage.Type)
	}

	backupDir = filepath.Join(backupDir, backup.Name)
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, volume)
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, volumeMount)
	job.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: backupDirEnvName, Value: backupDir},
		{Name: "ETCDCTL_CERT", Value: "/var/run/secrets/etcd-client/tls.crt"},
		{Name: "ETCDCTL_KEY", Value: "/var/run/secrets/etcd-client/tls.key"},
		{Name: "ETCDCTL_CACERT", Value: "/var/run/configmaps/etcd-ca/ca-bundle.crt"},
	}

	klog.Infof("BackupController starts with backup [%s] as job [%s]", backup.Name, jobName)
	job, err = jobClient.Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Job name is deterministic from backup name, assume informer cache is stale and job will be reconciled on a future sync
			klog.Infof("BackupController name conflict for backup [%s] on job [%s]", backup.Name, jobName)
			return nil
		}
		return fmt.Errorf("failed to create job: %w", err)
	}

	backup = backup.DeepCopy()
	setBackupRunning(backup, job)
	_, err = backupClient.UpdateStatus(ctx, backup, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error while updating backup status: %w", err)
	}

	return nil
}

func reconcileJobStatus(ctx context.Context,
	jobClient batchv1client.JobInterface,
	podLister corev1listers.PodNamespaceLister,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	job *batchv1.Job,
	backup *operatorv1alpha1.EtcdBackup) error {
	jobFinishedState := batchv1.JobConditionType("")
	for _, c := range job.Status.Conditions {
		// the types and type transitions are compatible between jobs and our backup states
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			jobFinishedState = c.Type
			break
		}
	}

	if jobFinishedState == "" {
		// Job is not finished
		if backup.Status.Job == nil {
			// Status update failed when job was created. Attempt to update now.
			backup = backup.DeepCopy()
			setBackupRunning(backup, job)
			if _, err := backupClient.UpdateStatus(ctx, backup, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("error while updating backup status: %w", err)
			}
		}

		return nil
	}

	if !backuphelpers.IsBackupFinished(backup) {
		backup = backup.DeepCopy()

		conditionType := operatorv1alpha1.BackupCompleted
		conditionReason := operatorv1alpha1.BackupReasonJobCompleted
		conditionMessage := fmt.Sprintf("backup job status %s", jobFinishedState)
		if jobFinishedState == batchv1.JobFailed {
			conditionType = operatorv1alpha1.BackupFailed
			conditionReason = operatorv1alpha1.BackupReasonJobFailed
		}
		now := metav1.Now()
		backup.Status.Conditions = []metav1.Condition{{
			Type:               string(conditionType),
			Reason:             string(conditionReason),
			Message:            conditionMessage,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
		}}

		pods, err := listJobPods(podLister, job)
		if err != nil {
			return fmt.Errorf("error listing pods for backup job [%s]: %w", job.Name, err)
		}

		if terminationMessage, err := findBackupTerminationMessage(pods); err != nil {
			return fmt.Errorf("error finding termination message for backup job [%s]: %w", job.Name, err)
		} else if files, err := parseTerminationMessage(terminationMessage); err != nil {
			klog.Infof("BackupController failed to read termination message for backup [%s]: %v", backup.Name, err)
			if conditionType == operatorv1alpha1.BackupFailed {
				// If no termination message is found or it is invalid, it's possible a backup pod crashed without being able to write out the status of files it created.
				// Assume that GC is required, file paths can be inferred based on storage backend and EtcdBackup name
				backup.Status.Conditions = append(backup.Status.Conditions, metav1.Condition{
					Type:               string(operatorv1alpha1.BackupGarbageCollectionRequired),
					Reason:             string(operatorv1alpha1.BackupReasonFileStateUnknown),
					Message:            "unable to determine if backup job created files before failing",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				})
			}
		} else {
			backup.Status.Files = files
			if conditionType == operatorv1alpha1.BackupFailed {
				if len(files) > 0 {
					backup.Status.Conditions = append(backup.Status.Conditions, metav1.Condition{
						Type:               string(operatorv1alpha1.BackupGarbageCollectionRequired),
						Reason:             string(operatorv1alpha1.BackupReasonFilesPartiallyCreated),
						Message:            "backup job created some files before failing",
						Status:             metav1.ConditionTrue,
						LastTransitionTime: now,
					})
				} else {
					backup.Status.Conditions = append(backup.Status.Conditions, metav1.Condition{
						Type:               string(operatorv1alpha1.BackupGarbageCollectionRequired),
						Reason:             string(operatorv1alpha1.BackupReasonFilesNotCreated),
						Message:            "backup job didn't create any files before failing",
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
					})
				}
			}
		}

		// In case etcdbackup status update failed previously
		if backup.Status.Job == nil {
			backup.Status.Job = &operatorv1alpha1.EtcdBackupJobReference{
				Name:      job.Name,
				Namespace: job.Namespace,
				UID:       string(job.UID)}
		}

		if _, err := backupClient.UpdateStatus(ctx, backup, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error while updating backup status: %w", err)
		}
	}

	if slices.Contains(job.Finalizers, backuphelpers.FinalizerEtcdBackup) {
		job = job.DeepCopy()
		job.Finalizers = slices.DeleteFunc(job.Finalizers, func(finalizer string) bool { return finalizer == backuphelpers.FinalizerEtcdBackup })
		if _, err := jobClient.Update(ctx, job, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error while updating job finalizer [%s]: %w", job.Name, err)
		}
	}

	return nil
}

func reconcileMissingJobStatus(ctx context.Context, jobsClient batchv1client.JobInterface, backup *operatorv1alpha1.EtcdBackup) (failedReason string, err error) {
	if time.Since(backupStartTime(backup)) > 2*time.Minute {
		// Informers given reasonable amount of time to catch up, most likely job was deleted.
		// Fall back to live GET for latest info on the job
		jobName := backup.Status.Job.Name
		job, err := jobsClient.Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Sprintf("unable to find Job [%s]", jobName), nil
			}
			return "", fmt.Errorf("BackupController failed to get job [%s] for backup [%s]", jobName, backup.Name)
		} else if string(job.UID) != backup.Status.Job.UID {
			return fmt.Sprintf("found Job [%s] with incorrect UID [%s]", jobName, job.UID), nil
		} else {
			// Correct job exists, make sure it is labeled appropriately. Otherwise assume it will be handled on a future sync.
			if job.Labels[labelBackupName] == "" {
				job.Labels[labelBackupName] = backup.Name
				if _, err := jobsClient.Update(ctx, job, metav1.UpdateOptions{}); err != nil {
					return "", fmt.Errorf("BackupController failed to update mislabeled job [%s] for backup [%s]", jobName, backup.Name)
				}
			}
		}
	}
	return "", nil
}

// markBackupFailed is used to mark backups that failed without successfully reporting their state
func markBackupFailed(ctx context.Context,
	client operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	reason operatorv1alpha1.BackupConditionReason,
	message string,
	requireGC bool) error {
	now := metav1.Now()
	backup = backup.DeepCopy()

	var gcReason, gcMessage string
	var gcStatus metav1.ConditionStatus
	if requireGC {
		gcReason = string(operatorv1alpha1.BackupReasonFileStateUnknown)
		gcMessage = "unable to determine if backup job created files before failing"
		gcStatus = metav1.ConditionTrue
	} else {
		gcReason = string(operatorv1alpha1.BackupReasonFilesNotCreated)
		gcMessage = "backup is invalid"
		gcStatus = metav1.ConditionFalse
	}
	backup.Status.Conditions = []metav1.Condition{{
		Type:               string(operatorv1alpha1.BackupFailed),
		Reason:             string(reason),
		Message:            message,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
	}, {
		Type:               string(operatorv1alpha1.BackupGarbageCollectionRequired),
		Reason:             gcReason,
		Message:            gcMessage,
		Status:             gcStatus,
		LastTransitionTime: now,
	}}

	// Could remove finalizer here, but since that requires two API calls
	// leave it to the garbage collector in order to reduce contention.
	_, err := client.UpdateStatus(ctx, backup, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			klog.Infof("BackupController unable to update status of invalid backup [%s]: %s", backup.Name, err)
		} else {
			return fmt.Errorf("BackupController could not update status of invalid backup [%s]: %w", backup.Name, err)
		}
	}
	return nil
}

func indexJobsByBackupLabelName(jobs []*batchv1.Job) map[string]*batchv1.Job {
	m := map[string]*batchv1.Job{}
	for _, j := range jobs {
		if j.Labels != nil {
			backupCrdName := j.Labels[labelBackupName]
			if backupCrdName != "" {
				m[backupCrdName] = j
			}
		}
	}

	return m
}

// isJobFinished checks whether the given Job has finished execution.
// It does not discriminate between successful and failed terminations.
func isJobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isValidNode(ctx context.Context, nodeClient corev1client.NodeInterface, name string) (bool, error) {
	if _, err := nodeClient.Get(ctx, name, metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isValidPVC(ctx context.Context, pvcClient corev1client.PersistentVolumeClaimInterface, name string) (bool, error) {
	if _, err := pvcClient.Get(ctx, name, metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("BackupController could not get PVC [%s]: %w", name, err)
	}
	return true, nil
}

func listJobPods(podLister corev1listers.PodNamespaceLister, job *batchv1.Job) ([]*corev1.Pod, error) {
	pods, err := podLister.List(labels.SelectorFromSet(labels.Set{labelJobName: job.Name}))
	if err != nil {
		return nil, err
	}

	n := 0
	for _, pod := range pods {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "Job" && owner.Name == job.Name && owner.UID == job.UID {
				pods[n] = pod
				n++
				break
			}
		}
	}
	return pods[:n], nil
}

func findBackupTerminationMessage(pods []*corev1.Pod) (string, error) {
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	var failed string
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded {
			// Prefer termination message of successful pod
			return podTerminationMessage(pod), nil
		} else if message := podTerminationMessage(pod); message != "" {
			// If no success message is found, the latest non-empty failure message will be returned instead
			failed = message
		}
	}
	return failed, nil
}

func podTerminationMessage(pod *corev1.Pod) string {
	if len(pod.Status.ContainerStatuses) > 0 {
		status := pod.Status.ContainerStatuses[0]
		if status.State.Terminated != nil {
			return status.State.Terminated.Message
		}
	}
	return ""
}

func parseTerminationMessage(message string) ([]operatorv1alpha1.EtcdBackupFile, error) {
	if message == "" {
		return nil, fmt.Errorf("missing termination message")
	}
	data := backuphelpers.BackupTerminationLog{}
	if err := json.Unmarshal([]byte(message), &data); err != nil {
		return nil, fmt.Errorf("error reading termination message: %w", err)
	}

	files := make([]operatorv1alpha1.EtcdBackupFile, len(data.Files))
	for i, file := range data.Files {
		filePath, _ := strings.CutPrefix(file.Path, backupPathMount)
		files[i] = operatorv1alpha1.EtcdBackupFile{
			Path: filePath,
			Size: file.Size,
		}
	}
	return files, nil
}

func setBackupRunning(backup *operatorv1alpha1.EtcdBackup, job *batchv1.Job) {
	backup.Status.Job = &operatorv1alpha1.EtcdBackupJobReference{
		Name:      job.Name,
		Namespace: job.Namespace,
		UID:       string(job.UID),
	}
	backup.Status.Conditions = []metav1.Condition{{
		Type:               string(operatorv1alpha1.BackupRunning),
		Reason:             string(operatorv1alpha1.BackupReasonJobStarted),
		Message:            "backup job is running",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: job.CreationTimestamp,
	}}
}

func backupStartTime(backup *operatorv1alpha1.EtcdBackup) time.Time {
	startedAt := backup.CreationTimestamp.Time
	for _, condition := range backup.Status.Conditions {
		switch condition.Type {
		case string(operatorv1alpha1.BackupRunning):
			return condition.LastTransitionTime.Time
		case string(operatorv1alpha1.BackupPending):
			// There shouldn't be both Pending and Running conditions, but just in case.
			startedAt = condition.LastTransitionTime.Time
		}
	}
	return startedAt
}

// generateBackupJobName creates a hash-based name for deduplication
// TODO: Delete if not needed
func generateBackupJobName(backup *operatorv1alpha1.EtcdBackup) string {
	prefix := "backup-"
	name := backup.Name
	var suffix string
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		suffix = "-" + shortHash(string(backup.Spec.Storage.Type), backup.Status.NodeName)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		suffix = "-" + shortHash(string(backup.Spec.Storage.Type), backup.Spec.Storage.PVC.Name)
	default:
		suffix = "-" + shortHash("unknown")
	}
	remaining := maxNameLength - len(prefix) - len(suffix)
	if len(name) > remaining {
		name = name[:remaining]
	}
	return prefix + name + suffix
}

func shortHash(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "")))
	shortHash := hex.EncodeToString(hash[:])[:10]
	return shortHash
}
