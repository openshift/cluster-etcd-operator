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
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
)

var batchv1Decoder = func() runtime.Decoder {
	scheme := runtime.NewScheme()
	utilruntime.Must(batchv1.AddToScheme(scheme))
	return serializer.NewCodecFactory(scheme).UniversalDecoder(batchv1.SchemeGroupVersion)
}()

// createBackupJob adds the backup finalizer and creates the job for a backup. It returns the latest version of the
// backup, even on error, since adding the finalizer updates it. Job names are deterministic, so if the job already
// exists (i.e. it was created by an earlier sync that the informer cache hasn't caught up with), the existing job is
// returned.
func createBackupJob(ctx context.Context,
	backup *operatorv1alpha1.EtcdBackup,
	operatorImagePullSpec string,
	jobClient batchv1client.JobInterface,
	backupClient operatorv1alpha1client.EtcdBackupInterface) (*batchv1.Job, *operatorv1alpha1.EtcdBackup, error) {

	if !slices.Contains(backup.ObjectMeta.Finalizers, backuphelpers.FinalizerEtcdBackup) {
		patchedBackup, err := addBackupFinalizer(ctx, backupClient, backup)
		if err != nil {
			return nil, backup, err
		}
		backup = patchedBackup
	}

	obj, err := runtime.Decode(batchv1Decoder, bindata.MustAsset("etcd/cluster-backup-job.yaml"))
	if err != nil {
		return nil, backup, fmt.Errorf("could not decode batchv1 job scheme: %w", err)
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
	job.Spec.TTLSecondsAfterFinished = new(int32(jobFinishedTTL.Seconds()))
	job.Spec.Template.Spec.InitContainers[0].Image = operatorImagePullSpec
	job.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec

	backupDir := backuphelpers.BackupPathMount
	volume := corev1.Volume{Name: backupVolumeName}
	volumeMount := corev1.VolumeMount{Name: backupVolumeName, MountPath: backuphelpers.BackupPathMount}
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		// Local backups write to a hostPath, so the job is pinned to the selected node.
		job.Spec.Template.Spec.NodeName = backup.Status.NodeName

		hostPath := backuphelpers.LocalBackupHostPath(backup.Spec.Storage.Local)
		backupDir = filepath.Join(backupDir, hostPath)
		volume.HostPath = &corev1.HostPathVolumeSource{
			Path: hostPath,
			Type: ptr.To(corev1.HostPathDirectoryOrCreate),
		}
		// HostPath is appended to mount so that path handling is always consistent between local and pvc storage backend
		volumeMount.MountPath = filepath.Join(volumeMount.MountPath, hostPath)
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		// PVC backups are not pinned to a node. Instead the job is constrained to eligible control plane nodes and
		// the scheduler picks one that satisfies the volume's topology (e.g. WaitForFirstConsumer). A
		// preferred pod anti-affinity spreads backup pods across nodes without hard-pinning.
		nodeSelector := map[string]string{backuphelpers.ControlPlaneNodeLabelSelector: ""}
		maps.Copy(nodeSelector, backup.Spec.NodeSelector)
		job.Spec.Template.Spec.NodeSelector = nodeSelector
		job.Spec.Template.Spec.Affinity = backupSpreadAffinity()

		storagePVC := backup.Spec.Storage.PVC
		backupDir = filepath.Join(backupDir, filepath.Clean(storagePVC.Path))
		volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: storagePVC.Name,
		}
	default:
		return nil, backup, fmt.Errorf("unknown storage backend: %s", backup.Spec.Storage.Type)
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
	if backup.Spec.Assets != nil {
		assets := make([]string, len(backup.Spec.Assets))
		for i, asset := range backup.Spec.Assets {
			assets[i] = string(asset)
		}
		job.Spec.Template.Spec.Containers[0].Args = append(job.Spec.Template.Spec.Containers[0].Args, "--assets="+strings.Join(assets, ","))
	}

	created, err := jobClient.Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, backup, fmt.Errorf("failed to create job: %w", err)
		}
		existing, getErr := jobClient.Get(ctx, jobName, metav1.GetOptions{})
		if getErr != nil {
			return nil, backup, fmt.Errorf("failed to get existing job %q: %w", jobName, getErr)
		}
		if ownerUID, ok := jobOwnerUID(existing); !ok || ownerUID != backup.UID {
			return nil, backup, fmt.Errorf("job %q already exists and is not owned by backup %q", jobName, backup.Name)
		}
		klog.V(2).Infof("BackupController job %q for backup %q already exists", jobName, backup.Name)
		return existing, backup, nil
	}
	job = created
	klog.Infof("BackupController started backup %q job %q", backup.Name, jobName)
	return job, backup, nil
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

// generateBackupJobName creates a deterministic job name for deduplication. The backup UID is included so that a backup
// recreated with the same name never collides with the job of the previous backup.
func generateBackupJobName(backup *operatorv1alpha1.EtcdBackup) string {
	prefix := "backup"
	suffix := shortHash(backup.Name, string(backup.UID))
	return joinName(prefix, backup.Name, suffix)
}

func shortHash(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "")))
	shortHash := hex.EncodeToString(hash[:])[:10]
	return shortHash
}

// joinName joins the parts of name with "-" and ensures the total length is less than maxNameLength.
// The prefix and suffix stay fixed length, with the middle "name" component shortened to fit within the length requirements.
func joinName(prefix, name, suffix string) string {
	if !strings.HasSuffix(prefix, "-") {
		prefix = prefix + "-"
	}
	if !strings.HasPrefix(suffix, "-") {
		suffix = "-" + suffix
	}
	remaining := maxNameLength - len(prefix) - len(suffix)
	if len(name) > remaining {
		name = strings.TrimRight(name[:remaining], "-")
	}
	return prefix + name + suffix
}

func jobOwnerUID(job *batchv1.Job) (types.UID, bool) {
	for _, ownerRef := range job.OwnerReferences {
		if ownerRef.Kind == "EtcdBackup" {
			return ownerRef.UID, true
		}
	}
	return "", false
}

// listJobPods returns the pods owned by the job. Job names are reused when a backup is recreated with the same name, so
// pods are matched by owner UID in addition to the job-name label.
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

// jobFinishedCondition returns the job's terminal condition (Complete or Failed with status True), or nil if the job
// has not finished. A job reaches at most one terminal condition.
func jobFinishedCondition(job *batchv1.Job) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

// isJobFinished checks whether the given Job has finished execution.
// It does not discriminate between successful and failed terminations.
func isJobFinished(job *batchv1.Job) bool {
	return jobFinishedCondition(job) != nil
}

func isJobCompleted(job *batchv1.Job) bool {
	c := jobFinishedCondition(job)
	return c != nil && c.Type == batchv1.JobComplete
}

func isJobFailed(job *batchv1.Job) bool {
	c := jobFinishedCondition(job)
	return c != nil && c.Type == batchv1.JobFailed
}

// jobFinishedTime returns when the job finished, falling back to its creation time if unknown.
func jobFinishedTime(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	if c := jobFinishedCondition(job); c != nil && !c.LastTransitionTime.IsZero() {
		return c.LastTransitionTime.Time
	}
	return job.CreationTimestamp.Time
}

// jobFailedTime returns when the job failed, falling back to the creation time if unknown.
func jobFailedTime(job *batchv1.Job) time.Time {
	if c := jobFinishedCondition(job); c != nil && c.Type == batchv1.JobFailed && !c.LastTransitionTime.IsZero() {
		return c.LastTransitionTime.Time
	}
	return job.CreationTimestamp.Time
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
	if err := jobsClient.Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete job %s: %w", job.Name, err)
	}
	return nil
}
