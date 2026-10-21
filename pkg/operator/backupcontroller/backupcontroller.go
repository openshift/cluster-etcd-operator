package backupcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	applyconfigurationsmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
)

const (
	// SSA field managers used by backup controller to set status and finalizer on EtcdBackup.
	backupStatusFieldManager    = "etcd-backup-status"
	backupFinalizerFieldManager = "etcd-backup-finalizer"

	backupAppName           = "cluster-backup-job"
	backupPathMount         = "/etc/kubernetes/cluster-backup"
	backupDirEnvName        = "CLUSTER_BACKUP_PATH"
	ttlSecondsAfterFinished = int32(4 * 60 * 60)

	jobNotFoundGracePeriod   = 2 * time.Minute
	backupDeletedGracePeriod = 1 * time.Minute

	labelJobName = "batch.kubernetes.io/job-name"

	maxNameLength = 63
)

type BackupController struct {
	backupsLister         operatorv1alpha1listers.EtcdBackupLister
	podsLister            corev1listers.PodNamespaceLister
	nodesLister           corev1listers.NodeLister
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
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	backupInformer factory.Informer,
	jobInformer cache.SharedIndexInformer,
	podInformer factory.Informer,
	nodeInformer factory.Informer) (factory.Controller, error) {
	c := &BackupController{
		backupsLister:         backupsLister,
		podsLister:            podsLister,
		nodesLister:           nodesLister,
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
		WithBareInformers(podInformer, nodeInformer).
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

type backupState int

const (
	// backupQueued is a backup that has not been started yet
	backupQueued backupState = iota
	// backupActive is a backup that has been started, or may have been started, and holds its Node or PVC
	backupActive
	// backupFinished is a backup that no longer needs to be reconciled by this controller
	backupFinished
)

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
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})

	var errs []error
	var requeueAfter time.Duration

	// Reconcile backups that have already been started, so that the resources held by active backups are known
	// before any new backups are started.
	observedActive := map[string]struct{}{}
	queue := make([]*operatorv1alpha1.EtcdBackup, 0, len(backups))
	for _, backup := range backups {
		state, delay, err := c.reconcileBackupState(ctx, backup)
		if err != nil {
			errs = append(errs, err)
		}
		if delay > 0 && (requeueAfter == 0 || delay < requeueAfter) {
			requeueAfter = delay
		}

		switch state {
		case backupActive:
			observedActive[backup.Name] = struct{}{}
			c.activeCache.add(backup)
		case backupFinished:
			c.activeCache.remove(backup.Name)
		case backupQueued:
			// Check cache in case of informer lag on a backup that was started by a previous sync
			if !c.activeCache.isActive(backup.Name) {
				queue = append(queue, backup)
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

	if requeueAfter > 0 {
		syncCtx.Queue().AddAfter(syncCtx.QueueKey(), requeueAfter)
	}
	return utilerrors.NewAggregate(errs)
}

// reconcileBackupState reconciles the status of a backup. Backups that have not been started
// are returned as queued. A non-zero delay is returned when the backup should be reconciled again after a grace
// period. On error, the backup is reported as active so that its resources are not handed to another backup.
func (c *BackupController) reconcileBackupState(ctx context.Context, backup *operatorv1alpha1.EtcdBackup) (backupState, time.Duration, error) {
	if backuphelpers.IsBackupFinished(backup) {
		return backupFinished, 0, nil
	}

	backupName := backup.Name
	backupsClient := c.operatorClient.EtcdBackups()
	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	job, err := c.getJob(backup)
	if err != nil {
		return backupActive, 0, fmt.Errorf("BackupController failed to get job for backup %q: %w", backupName, err)
	}

	if job != nil {
		klog.V(4).Infof("BackupController job %q found for backup %q, reconciling status", job.Name, backupName)
		if err := reconcileJobStatus(ctx, jobsClient, c.podsLister, backupsClient, job, backup); err != nil {
			return backupActive, 0, fmt.Errorf("BackupController could not reconcile job status for backup %q: %w", backupName, err)
		}
		if isJobFinished(job) {
			return backupFinished, 0, nil
		}
		return backupActive, 0, nil
	} else if backup.Status.JobName != "" {
		// Give informers some time to catch up. Will reconcile sooner if the job is observed.
		jobName := backup.Status.JobName
		if requeueAfter, ok := shouldDelay(backupStartTime(backup), jobNotFoundGracePeriod); ok {
			klog.Infof("BackupController requeueing backup %q with missing job %q", backupName, jobName)
			return backupActive, requeueAfter, nil
		}

		if err := reconcileJobNotFound(ctx, backupsClient, jobsClient, backup); err != nil {
			return backupActive, 0, fmt.Errorf("BackupController failed to reconcile missing job %q for backup %q: %w", jobName, backupName, err)
		}
		// Either the backup was failed, or the job was relabeled and will be reconciled on the next sync
		return backupActive, 0, nil
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

		if _, err := applyBackupFinalizer(ctx, backupsClient, backup, false); err != nil {
			return backupActive, 0, fmt.Errorf("BackupController failed to remove finalizer from deleted backup %q: %w", backupName, err)
		}
		return backupFinished, 0, nil
	}

	return backupQueued, 0, nil
}

// pruneActiveCache removes backups from the active cache that are no longer active. Any cached backups that were not
// observed as active this sync are either new entries that haven't synced to the informer cache yet, or no longer
// exist. Compare against source of truth (k8s API).
func (c *BackupController) pruneActiveCache(ctx context.Context, observedActive map[string]struct{}) error {
	backupsClient := c.operatorClient.EtcdBackups()
	for backupName := range c.activeCache.backups {
		if _, ok := observedActive[backupName]; ok {
			continue
		}
		if exists, err := backupExists(ctx, backupsClient, backupName); err != nil {
			return err
		} else if !exists {
			c.activeCache.remove(backupName)
		}
	}
	return nil
}

// startQueuedBackups starts as many of the queued backups as the concurrency limits allow, in order.
func (c *BackupController) startQueuedBackups(ctx context.Context, queue []*operatorv1alpha1.EtcdBackup) error {
	controlPlaneNodes, err := backuphelpers.SelectBackupNodes(c.nodesLister, nil)
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

	// Only Local backups are pinned to a specific node. For PVC backups the node is left unset so that the scheduler
	// can place the job on a node that satisfies the volume's topology constraints. The PVC itself is the concurrency
	// key for PVC backups (see canStart).
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		if backup.Status.NodeName == "" {
			nodeName, matched := c.selectNode(backup, controlPlaneNodes)
			if !matched {
				if _, err := applyBackupFinished(ctx, backupsClient, backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonSetupFailed, "unable to find a control plane Node matching the selector", nil); err != nil {
					if apierrors.IsNotFound(err) {
						return nil
					}
					return fmt.Errorf("BackupController failed to apply status on Local backup %q: %w", backup.Name, err)
				}
				return nil
			} else if nodeName == "" {
				klog.Infof("BackupController unable to start backup [%s]: all control plane nodes in use", backupName)
				return nil
			}

			var err error
			if backup, err = applyBackupStatus(ctx, backupsClient, backup, func(status *applyconfigurationsoperatorv1alpha1.EtcdBackupStatusApplyConfiguration) {
				status.NodeName = &nodeName
			}); err != nil {
				return fmt.Errorf("BackupController failed to assign node to Local backup: %w", err)
			}
		}
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		pvcsClient := c.kubeClient.CoreV1().PersistentVolumeClaims(operatorclient.TargetNamespace)
		pvcName := backup.Spec.Storage.PVC.Name
		if validPVC, err := isValidPVC(ctx, pvcsClient, pvcName); err != nil {
			return fmt.Errorf("BackupController failed to validate PVC %q for backup %q: %w", pvcName, backupName, err)
		} else if !validPVC {
			if _, err := applyBackupFinished(ctx, backupsClient, backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonSetupFailed, fmt.Sprintf("unable to find PVC %q", pvcName), nil); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("BackupController failed to apply status on PVC backup %q: %w", backup.Name, err)
			}
			return nil
		}
	}

	if ok, reason := c.activeCache.canStart(backup); !ok {
		klog.Infof("BackupController unable to start backup [%s]: %s", backupName, reason)
		return nil
	}

	klog.V(4).Infof("BackupController starting backup %q", backupName)
	job, err := createBackupJob(ctx, backup, c.operatorImagePullSpec, jobsClient, backupsClient)
	if err != nil {
		return fmt.Errorf("BackupController failed to start backup %q: %w", backupName, err)
	}
	// The job exists from this point, so hold its resources even if the status update fails
	c.activeCache.add(backup)

	if _, err := applyBackupRunning(ctx, backupsClient, backup, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("BackupController failed to apply running status on backup %q: %w", backupName, err)
	}
	return nil
}

// selectNode picks the first control plane node matching the backup's node selector that isn't already running a
// backup. matched reports whether any control plane node matched the node selector, regardless of whether it is in use.
func (c *BackupController) selectNode(backup *operatorv1alpha1.EtcdBackup, controlPlaneNodes []*corev1.Node) (nodeName string, matched bool) {
	selector := labels.Everything()
	if len(backup.Spec.NodeSelector) > 0 {
		selector = labels.SelectorFromSet(backup.Spec.NodeSelector)
	}

	for _, node := range controlPlaneNodes {
		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}
		matched = true
		if !c.activeCache.local.inUse(node.Name) {
			return node.Name, true
		}
	}
	return "", matched
}

func backupExists(ctx context.Context, backupsClient operatorv1alpha1client.EtcdBackupInterface, name string) (bool, error) {
	if _, err := backupsClient.Get(ctx, name, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return true, fmt.Errorf("BackupController failed to get backup [%s]: %w", name, err)
		}
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
			if job == nil {
				job = j
			} else if j.CreationTimestamp.After(job.CreationTimestamp.Time) {
				job = j
			}
		}
	}
	return job, nil
}

// createBackupJob creates the job for a backup. A nil job is returned if the job already exists.
func createBackupJob(ctx context.Context,
	backup *operatorv1alpha1.EtcdBackup,
	operatorImagePullSpec string,
	jobClient batchv1client.JobInterface,
	backupClient operatorv1alpha1client.EtcdBackupInterface) (*batchv1.Job, error) {

	if !slices.Contains(backup.ObjectMeta.Finalizers, backuphelpers.FinalizerEtcdBackup) {
		patchedBackup, err := applyBackupFinalizer(ctx, backupClient, backup, true)
		if err != nil {
			return nil, err
		}
		backup = patchedBackup
	}

	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), bindata.MustAsset("etcd/cluster-backup-job.yaml"))
	if err != nil {
		return nil, fmt.Errorf("could not decode batchv1 job scheme: %w", err)
	}

	jobName := generateBackupJobName(backup)
	job := obj.(*batchv1.Job)
	job.Name = jobName
	job.Labels[backuphelpers.LabelEtcdBackup] = backup.Name
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

	backupDir := backupPathMount
	volume := corev1.Volume{Name: "etc-kubernetes-cluster-backup"}
	volumeMount := corev1.VolumeMount{Name: "etc-kubernetes-cluster-backup", MountPath: backupPathMount}
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		// Local backups write to a hostPath, so the job is pinned to the selected node.
		job.Spec.Template.Spec.NodeName = backup.Status.NodeName

		storageLocal := backup.Spec.Storage.Local
		backupDir = filepath.Join(backupDir, storageLocal.HostPath)
		volume.HostPath = &corev1.HostPathVolumeSource{
			Path: storageLocal.HostPath,
			Type: ptr.To(corev1.HostPathDirectoryOrCreate),
		}
		// HostPath is appended to mount so that path handling is always consistent between local and pvc storage backend
		volumeMount.MountPath = filepath.Join(volumeMount.MountPath, storageLocal.HostPath)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		// PVC backups are not pinned to a node. Instead the job is constrained to eligible master nodes and
		// the scheduler picks a node that satisfies the volume's topology (e.g. WaitForFirstConsumer). A
		// preferred pod anti-affinity spreads backup pods across nodes without hard-pinning.
		job.Spec.Template.Spec.NodeSelector = pvcNodeSelector(backup.Spec.NodeSelector)
		job.Spec.Template.Spec.Affinity = backupSpreadAffinity()

		storagePVC := backup.Spec.Storage.PVC
		backupDir = filepath.Join(backupDir, storagePVC.Path)
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: storagePVC.Name,
		}
	default:
		return nil, fmt.Errorf("unknown storage backend: %s", backup.Spec.Storage.Type)
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

	job, err = jobClient.Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Job name is deterministic from backup name, assume informer cache is stale and job will be reconciled on a future sync
			klog.V(2).Infof("BackupController name conflict for backup %q on job %q", backup.Name, jobName)
			return nil, nil
		}
		return nil, fmt.Errorf("failed to create job: %w", err)
	}
	klog.Infof("BackupController started backup %q job %q", backup.Name, jobName)
	return job, nil
}

// pvcNodeSelector constrains a PVC backup job to eligible master nodes, merging the backup's node selector
// on top of the master role label. The master label always applies since the backup job reads etcd's data
// from the host and must run on a control-plane node.
func pvcNodeSelector(backupSelector map[string]string) map[string]string {
	selector := map[string]string{backuphelpers.ControlPlaneNodeLabelSelector: ""}
	maps.Copy(selector, backupSelector)
	return selector
}

// backupSpreadAffinity prefers scheduling backup pods onto different nodes so that concurrent backups
// don't all target the same etcd member.
func backupSpreadAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": backupAppName},
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}
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
		if backup.Status.JobName == "" {
			// Status update failed when job was created. Attempt to update now.
			if _, err := applyBackupRunning(ctx, backupClient, backup, job); err != nil {
				return err
			}
		}
		return nil
	}

	// If finalizing job fails for some reason, we may re-reconcile the job. Only need to update the backup once.
	if !backuphelpers.IsBackupFinished(backup) {
		backup = backup.DeepCopy()

		conditionType := operatorv1alpha1.BackupCompleted
		conditionReason := operatorv1alpha1.BackupReasonJobCompleted
		if jobFinishedState == batchv1.JobFailed {
			conditionType = operatorv1alpha1.BackupFailed
			conditionReason = operatorv1alpha1.BackupReasonJobFailed
		}

		pods, err := listJobPods(podLister, job)
		if err != nil {
			return fmt.Errorf("error listing pods for backup job %q: %w", job.Name, err)
		}

		var message string
		var files []operatorv1alpha1.EtcdBackupFile
		terminationMessage, started := findBackupTerminationMessage(pods)
		if started {
			// Backup container started, expect to be able to parse termination message
			if termLog, err := parseTerminationMessage(terminationMessage); err == nil {
				message, files = termLog.Message, termLog.Files
				if len(files) == 0 {
					// Backup container reported graceful failure, no GC is required
					conditionReason = operatorv1alpha1.BackupReasonSetupFailed
					if message == "" {
						message = "backup job failed without creating files"
					}
				}
			} else {
				klog.Infof("BackupController failed to read termination message for backup %q: %v", backup.Name, err)
			}
		} else {
			// Backup container never started, most likely the init container failed or PVC was unable to mount
			conditionReason = operatorv1alpha1.BackupReasonSetupFailed
			message = terminationMessage
			if message == "" {
				message = "failed to verify storage"
			}
		}

		if _, err := applyBackupFinished(ctx, backupClient, backup, job, conditionType, conditionReason, message, files); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		if conditionType == operatorv1alpha1.BackupCompleted {
			klog.Infof("BackupController completed backup %q", backup.Name)
		} else {
			klog.Infof("BackupController failed backup %q: job failed", backup.Name)
		}
	}

	if _, err := finalizeJob(ctx, jobClient, job, false); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func reconcileJobNotFound(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	jobsClient batchv1client.JobInterface,
	backup *operatorv1alpha1.EtcdBackup,
) error {
	jobName := backup.Status.JobName
	job, err := jobsClient.Get(ctx, jobName, metav1.GetOptions{})

	var message string
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error getting job %q for backup %q: %w", jobName, backup.Name, err)
		}
		message = fmt.Sprintf("unable to find job %q", jobName)
	} else {
		for _, ownerRef := range job.OwnerReferences {
			if ownerRef.Kind == "EtcdBackup" && ownerRef.Name == backup.Name && ownerRef.UID == backup.UID {
				// Correct job exists, make sure it is labeled appropriately. Otherwise assume it will be handled on a future sync.
				if job.Labels == nil || job.Labels[backuphelpers.LabelEtcdBackup] != backup.Name {
					patchBody, err := json.Marshal(metav1.ObjectMeta{Labels: map[string]string{backuphelpers.LabelEtcdBackup: backup.Name}})
					if err != nil {
						return fmt.Errorf("error marshalling job %q patch: %w", job.Name, err)
					}
					if _, err := jobsClient.Patch(ctx, job.Name, types.StrategicMergePatchType, patchBody, metav1.PatchOptions{}); err != nil {
						return fmt.Errorf("error adding label to job: %w", err)
					}
				}
				return nil
			}
		}

		message = fmt.Sprintf("found job %q missing owner reference for %s", jobName, backup.Name)
	}

	if _, err := applyBackupFinished(ctx, backupClient, backup, nil, operatorv1alpha1.BackupFailed, operatorv1alpha1.BackupReasonJobFailed, message, nil); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("error updating failed backup: %w", err)
	}
	klog.Infof("BackupController failed backup %q: %s", backup.Name, message)
	return nil
}

func isValidPVC(ctx context.Context, pvcClient corev1client.PersistentVolumeClaimInterface, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	if _, err := pvcClient.Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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

func findBackupTerminationMessage(pods []*corev1.Pod) (message string, started bool) {
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded {
			// Prefer termination message of successful pod
			message, started = podTerminationMessage(pod)
			break
		} else if podMessage, podStarted := podTerminationMessage(pod); podStarted {
			// If no success message is found, the latest non-empty failure message will be returned instead
			started = true
			if podMessage != "" {
				message = podMessage
			}
		}
	}
	return message, started
}

func podTerminationMessage(pod *corev1.Pod) (message string, started bool) {
	if len(pod.Status.ContainerStatuses) > 0 {
		if status := pod.Status.ContainerStatuses[0]; (status.Started != nil && *status.Started) || status.State.Terminated != nil {
			started = true
			if status.State.Terminated != nil {
				message = status.State.Terminated.Message
			}
		}
	}
	return message, started
}

func parseTerminationMessage(message string) (backuphelpers.BackupTerminationLog, error) {
	if message == "" {
		return backuphelpers.BackupTerminationLog{}, fmt.Errorf("missing termination message")
	}
	data := backuphelpers.BackupTerminationLog{}
	if err := json.Unmarshal([]byte(message), &data); err != nil {
		return backuphelpers.BackupTerminationLog{}, fmt.Errorf("error reading termination message: %w", err)
	}

	for i, file := range data.Files {
		filePath, _ := strings.CutPrefix(file.Path, backupPathMount)
		data.Files[i] = operatorv1alpha1.EtcdBackupFile{
			Path:      filePath,
			SizeBytes: file.SizeBytes,
		}
	}
	return data, nil
}

func applyBackupRunning(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
) (*operatorv1alpha1.EtcdBackup, error) {
	// Only Local backup jobs are pinned to a node
	var nodeName *string
	if job.Spec.Template.Spec.NodeName != "" {
		nodeName = &job.Spec.Template.Spec.NodeName
	}
	return applyBackupStatusConditions(ctx, backupClient, backup, job, []metav1.Condition{
		{
			Type:               string(operatorv1alpha1.BackupRunning),
			Reason:             string(operatorv1alpha1.BackupReasonJobStarted),
			Message:            "backup job is running",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: job.CreationTimestamp,
		},
	}, nil, nodeName)
}

func applyBackupFinished(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
	condition operatorv1alpha1.BackupConditionType,
	reason operatorv1alpha1.BackupConditionReason,
	message string,
	files []operatorv1alpha1.EtcdBackupFile,
) (*operatorv1alpha1.EtcdBackup, error) {
	transitionTime := metav1.Now()
	if job != nil {
		if condition == operatorv1alpha1.BackupCompleted {
			if job.Status.CompletionTime != nil {
				transitionTime = *job.Status.CompletionTime
			}
		} else {
			for _, condition := range job.Status.Conditions {
				if condition.Type == batchv1.JobFailed {
					transitionTime = condition.LastTransitionTime
					break
				}
			}
		}
	}
	conditions := []metav1.Condition{}

	switch condition {
	case operatorv1alpha1.BackupCompleted:
		if message == "" {
			message = "backup job completed"
		}
		conditions = append(conditions, metav1.Condition{
			Type:               string(condition),
			Reason:             string(reason),
			Message:            message,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: transitionTime,
		})
	case operatorv1alpha1.BackupFailed:
		if message == "" {
			message = "backup job failed"
		}
		conditions = append(conditions, metav1.Condition{
			Type:               string(condition),
			Reason:             string(reason),
			Message:            message,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: transitionTime,
		})
	default:
		return nil, fmt.Errorf("invalid finished condition %q", condition)
	}
	return applyBackupStatusConditions(ctx, backupClient, backup, job, conditions, files, nil)
}

// applyBackupFailedWithoutJob is used to mark backups that failed without successfully reporting their state
func applyBackupStatusConditions(ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	job *batchv1.Job,
	conditions []metav1.Condition,
	files []operatorv1alpha1.EtcdBackupFile,
	nodeName *string,
) (*operatorv1alpha1.EtcdBackup, error) {
	backup, err := applyBackupStatus(ctx, backupClient, backup, func(status *applyconfigurationsoperatorv1alpha1.EtcdBackupStatusApplyConfiguration) {
		if job != nil {
			status.JobName = &job.Name
		}
		if conditions != nil {
			status.Conditions = make([]applyconfigurationsmetav1.ConditionApplyConfiguration, len(conditions))
			for i, condition := range conditions {
				status.Conditions[i] = *applyconfigurationsmetav1.Condition().
					WithType(condition.Type).
					WithReason(condition.Reason).
					WithMessage(condition.Message).
					WithStatus(condition.Status).
					WithLastTransitionTime(condition.LastTransitionTime)
			}
		}
		if files != nil {
			status.Files = make([]applyconfigurationsoperatorv1alpha1.EtcdBackupFileApplyConfiguration, len(files))
			for i, file := range files {
				status.Files[i] = *applyconfigurationsoperatorv1alpha1.EtcdBackupFile().
					WithPath(file.Path).
					WithSizeBytes(file.SizeBytes)
			}
		}
		if nodeName != nil {
			status.NodeName = nodeName
		}
	})
	if err != nil {
		return nil, err
	}
	return backup, err
}

func applyBackupStatus(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	applyStatus func(status *applyconfigurationsoperatorv1alpha1.EtcdBackupStatusApplyConfiguration),
) (*operatorv1alpha1.EtcdBackup, error) {
	backupName := backup.Name
	backupApply, err := applyconfigurationsoperatorv1alpha1.ExtractEtcdBackupStatus(backup, backupStatusFieldManager)
	if err != nil {
		return nil, fmt.Errorf("error extracting backup %q status: %w", backupName, err)
	}
	if backupApply.Status == nil {
		backupApply.Status = applyconfigurationsoperatorv1alpha1.EtcdBackupStatus()
	}
	applyStatus(backupApply.Status)

	if backup, err = backupClient.ApplyStatus(ctx, backupApply, metav1.ApplyOptions{
		Force:        true,
		FieldManager: backupStatusFieldManager,
	}); err != nil {
		return nil, fmt.Errorf("error applying backup %q status: %w", backupName, err)
	}
	return backup, nil
}

func applyBackupFinalizer(
	ctx context.Context,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	backup *operatorv1alpha1.EtcdBackup,
	hasFinalizer bool,
) (*operatorv1alpha1.EtcdBackup, error) {
	backupName := backup.Name
	backupApply, err := applyconfigurationsoperatorv1alpha1.ExtractEtcdBackup(backup, backupFinalizerFieldManager)
	if err != nil {
		return nil, fmt.Errorf("error extracting backup %q finalizer: %w", backupName, err)
	}

	if !hasFinalizer {
		if idx := slices.Index(backup.Finalizers, backuphelpers.FinalizerEtcdBackup); idx >= 0 && !slices.Contains(backupApply.Finalizers, backuphelpers.FinalizerEtcdBackup) {
			// Someone accidentally took field ownership of the finalizer. Fallback to json patch to remove it.
			path := fmt.Sprintf("/metadata/finalizers/%d", idx)
			patchData, err := json.Marshal([]map[string]any{
				{"op": "test", "path": path, "value": backuphelpers.FinalizerEtcdBackup},
				{"op": "remove", "path": path},
			})
			if err != nil {
				return nil, fmt.Errorf("error marshalling backup %q patch to remove finalizer: %w", backupName, err)
			}
			backup, err = backupClient.Patch(ctx, backupName, types.JSONPatchType, patchData, metav1.PatchOptions{})
			if err != nil {
				return nil, fmt.Errorf("error patching backup %q finalizer: %w", backupName, err)
			}
			return backup, nil
		}

		backupApply.Finalizers = nil
	} else {
		backupApply.WithFinalizers(backuphelpers.FinalizerEtcdBackup)
	}

	backup, err = backupClient.Apply(ctx, backupApply, metav1.ApplyOptions{
		Force:        true,
		FieldManager: backupFinalizerFieldManager,
	})
	if err != nil {
		return nil, fmt.Errorf("error applying backup %q finalizer: %w", backupName, err)
	}
	return backup, nil
}

func backupStartTime(backup *operatorv1alpha1.EtcdBackup) time.Time {
	for _, condition := range backup.Status.Conditions {
		if condition.Type == string(operatorv1alpha1.BackupRunning) {
			return condition.LastTransitionTime.Time
		}
	}
	return backup.CreationTimestamp.Time
}

func shouldDelay(since time.Time, gracePeriod time.Duration) (time.Duration, bool) {
	remaining := gracePeriod - time.Since(since)
	if remaining >= 0 {
		return remaining, true
	}
	return 0, false
}

// generateBackupJobName creates a deterministic job name for deduplication
func generateBackupJobName(backup *operatorv1alpha1.EtcdBackup) string {
	// TODO: Consider hash-based name
	return backup.Name
}

func shortHash(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "")))
	shortHash := hex.EncodeToString(hash[:])[:10]
	return shortHash
}
