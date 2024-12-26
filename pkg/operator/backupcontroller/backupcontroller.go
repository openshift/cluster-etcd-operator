package backupcontroller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/storage/names"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	backupLabel      = "cluster-backup-job"
	backupJobLabel   = "backup-name"
	recentBackupPath = "/var/lib/etcd-auto-backup"
	backupDirEnvName = "CLUSTER_BACKUP_PATH"
)

type BackupController struct {
	operatorClient        v1helpers.OperatorClient
	backupsClient         operatorv1alpha1client.EtcdBackupsGetter
	kubeClient            kubernetes.Interface
	operatorImagePullSpec string
	featureGateAccessor   featuregates.FeatureGateAccess
}

func NewBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsClient operatorv1alpha1client.EtcdBackupsGetter,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	backupInformer factory.Informer,
	jobInformer factory.Informer) factory.Controller {

	c := &BackupController{
		backupsClient:         backupsClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		featureGateAccessor:   accessor,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithInformers(backupInformer, jobInformer).
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

	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	currentJobs, err := jobsClient.List(ctx, v1.ListOptions{LabelSelector: "app=" + backupLabel + ",state!=processed"})
	if err != nil {
		return fmt.Errorf("BackupController could not list backup jobs, error was: %w", err)
	}

	// we only allow to run one at a time, if there's currently a job running then we will skip it in this reconciliation step
	runningJobs := findRunningJobs(currentJobs)
	if len(runningJobs) > 0 {
		klog.V(4).Infof("BackupController already found [%d] running jobs, skipping", len(runningJobs))
		return nil
	}

	jobIndexed := indexJobsByBackupLabelName(currentJobs)
	backupsClient := c.backupsClient.EtcdBackups()
	backups, err := backupsClient.List(ctx, v1.ListOptions{LabelSelector: "state!=processed"})
	if err != nil {
		return fmt.Errorf("BackupController could not list etcdbackups CRDs, error was: %w", err)
	}

	if backups == nil {
		return nil
	}

	var backupsToRun []operatorv1alpha1.EtcdBackup
	for _, item := range backups.Items {
		if backupJob, ok := jobIndexed[item.Name]; ok {
			klog.V(4).Infof("BackupController backup job with name [%s] found, reconciling status", backupJob.Name)
			err := reconcileJobStatus(ctx, jobsClient, backupsClient, backupJob, item)
			if err != nil {
				return fmt.Errorf("BackupController could not reconcile job status: %w", err)
			}

			delete(jobIndexed, item.Name)
			continue
		}

		backupsToRun = append(backupsToRun, *item.DeepCopy())
	}

	if len(jobIndexed) > 0 {
		klog.V(4).Infof("BackupController found dangling jobs without corresponding backup, removing")
		for _, job := range jobIndexed {
			err := jobsClient.Delete(ctx, job.Name, v1.DeleteOptions{})
			if err != nil {
				return fmt.Errorf("BackupController could not delete danging job [%s]: %w", job.Name, err)
			}
		}
	}

	if len(backupsToRun) == 0 {
		klog.V(4).Infof("BackupController no backups to reconcile, skipping")
		return nil
	}

	// in case of multiple backups requested, we're trying to reconcile in order of their names (also to reduce flakiness in tests)
	sort.Slice(backupsToRun, func(i, j int) bool {
		return strings.Compare(backupsToRun[i].Name, backupsToRun[j].Name) < 0
	})

	klog.V(4).Infof("BackupController backupsToRun: %v, chooses %v", backupsToRun, backupsToRun[0])
	valid, err := validateBackup(ctx, backupsToRun[0], c.kubeClient, backupsClient)
	if err != nil {
		return err
	}

	if !valid {
		klog.V(4).Infof("BackupController deems: %v invalid, skipping", backupsToRun[0])
		return nil
	}

	err = createBackupJob(ctx, backupsToRun[0], c.operatorImagePullSpec, jobsClient, backupsClient)
	if err != nil {
		return err
	}

	if len(backupsToRun) > 1 {
		for _, backup := range backupsToRun[1:] {
			err := markBackupSkipped(ctx, backupsClient, backup)
			if err != nil {
				return err
			}
			klog.V(4).Infof("BackupController marked as skipped: %v", backup)
		}
	}

	return nil
}

func validateBackup(ctx context.Context,
	backup operatorv1alpha1.EtcdBackup,
	kubeClient kubernetes.Interface,
	backupsClient operatorv1alpha1client.EtcdBackupInterface) (bool, error) {

	_, err := kubeClient.CoreV1().PersistentVolumeClaims(operatorclient.TargetNamespace).Get(ctx, backup.Spec.PVCName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			markFailedErr := markBackupFailed(ctx, backupsClient, backup, fmt.Sprintf("unable to find PVC [%s]", backup.Spec.PVCName))
			if markFailedErr != nil {
				return false, fmt.Errorf("BackupController could not get PVC [%s]: %w, failed to mark as failed: %w", backup.Spec.PVCName, err, markFailedErr)
			}

			return false, nil
		}

		return false, fmt.Errorf("BackupController could not get PVC [%s]: %w", backup.Spec.PVCName, err)
	}

	return true, nil
}

func markBackupSkipped(ctx context.Context, client operatorv1alpha1client.EtcdBackupInterface, backup operatorv1alpha1.EtcdBackup) error {
	// mark all previous conditions as false, only BackupSkipped should be true
	for i := 0; i < len(backup.Status.Conditions); i++ {
		backup.Status.Conditions[i].Status = v1.ConditionFalse
	}

	backup.Status.Conditions = append(backup.Status.Conditions, v1.Condition{
		Type:               string(operatorv1alpha1.BackupSkipped),
		Reason:             string(operatorv1alpha1.BackupSkipped),
		Message:            "skipped due too many simultaneous backups",
		Status:             v1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})

	_, err := client.UpdateStatus(ctx, &backup, v1.UpdateOptions{})
	if err != nil {
		return err
	}

	bp, err := client.Get(ctx, backup.Name, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error while getting backup for updating skipped status [%s]: %w", backup.Name, err)
	}

	// update the backup label, so we don't have to reconcile it anymore later
	if bp.Labels == nil {
		bp.Labels = map[string]string{}
	}

	bp.Labels["state"] = "processed"
	_, err = client.Update(ctx, bp, v1.UpdateOptions{})
	return err
}

func markBackupFailed(ctx context.Context,
	client operatorv1alpha1client.EtcdBackupInterface,
	backup operatorv1alpha1.EtcdBackup,
	failedMessage string) error {

	// mark all previous conditions as false, only BackupFailed should be true
	for i := 0; i < len(backup.Status.Conditions); i++ {
		backup.Status.Conditions[i].Status = v1.ConditionFalse
	}

	backup.Status.Conditions = append(backup.Status.Conditions, v1.Condition{
		Type:               string(operatorv1alpha1.BackupFailed),
		Reason:             string(operatorv1alpha1.BackupFailed),
		Message:            failedMessage,
		Status:             v1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})

	_, err := client.UpdateStatus(ctx, &backup, v1.UpdateOptions{})
	if err != nil {
		return err
	}

	bp, err := client.Get(ctx, backup.Name, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error while getting backup for updating skipped status [%s]: %w", backup.Name, err)
	}

	// update the backup label, so we don't have to reconcile it anymore later
	if bp.Labels == nil {
		bp.Labels = map[string]string{}
	}

	bp.Labels["state"] = "processed"
	_, err = client.Update(ctx, bp, v1.UpdateOptions{})
	return err
}

func indexJobsByBackupLabelName(jobs *batchv1.JobList) map[string]batchv1.Job {
	m := map[string]batchv1.Job{}
	if jobs == nil {
		return m
	}

	for _, j := range jobs.Items {
		backupCrdName := j.Labels[backupJobLabel]
		if backupCrdName != "" {
			m[backupCrdName] = *j.DeepCopy()
		}
	}

	return m
}

func findRunningJobs(jobs *batchv1.JobList) []batchv1.Job {
	var running []batchv1.Job
	if jobs == nil {
		return running
	}

	for _, j := range jobs.Items {
		if !isJobFinished(&j) {
			running = append(running, *j.DeepCopy())
		}
	}

	return running
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

func reconcileJobStatus(ctx context.Context,
	jobClient batchv1client.JobInterface,
	backupClient operatorv1alpha1client.EtcdBackupInterface,
	job batchv1.Job,
	backup operatorv1alpha1.EtcdBackup) error {

	jobState := batchv1.JobConditionType("")
	for _, c := range job.Status.Conditions {
		// the types and type transitions are compatible between jobs and our backup states
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			jobState = c.Type
			break
		}
	}

	// we only reconcile completed/failed jobs
	if string(jobState) == "" {
		return nil
	}

	if backup.Labels == nil {
		backup.Labels = map[string]string{}
	}

	backup.Labels["state"] = "processed"

	// we're updating the backup and its status first, in case of a failure we can still reconcile the job without it
	_, err := backupClient.Update(ctx, &backup, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error while updating backup [%s]: %w", backup.Name, err)
	}

	// update above has changed the CRD, to avoid conflicts we grab the latest version from the API
	bp, err := backupClient.Get(ctx, backup.Name, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error while getting backup for updating status [%s]: %w", backup.Name, err)
	}

	statusReason := operatorv1alpha1.BackupCompleted
	if jobState == batchv1.JobFailed {
		statusReason = operatorv1alpha1.BackupFailed
	}

	// only the new completed condition should stay true, mark all others false
	for i := 0; i < len(bp.Status.Conditions); i++ {
		bp.Status.Conditions[i].Status = v1.ConditionFalse
	}

	bp.Status.Conditions = append(bp.Status.Conditions,
		v1.Condition{
			Type:               string(statusReason),
			Reason:             string(statusReason),
			Message:            fmt.Sprintf("%s", jobState),
			Status:             v1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(time.Now()),
		})

	// TODO(thomas): no way to update both status and the CRD in one call?
	_, err = backupClient.UpdateStatus(ctx, bp, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error while updating backup status [%s]: %w", backup.Name, err)
	}

	if job.Labels == nil {
		job.Labels = map[string]string{}
	}

	job.Labels["state"] = "processed"
	_, err = jobClient.Update(ctx, &job, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error while updating job labels [%s]: %w", job.Name, err)
	}

	return nil
}

func createBackupJob(ctx context.Context,
	backup operatorv1alpha1.EtcdBackup,
	operatorImagePullSpec string,
	jobClient batchv1client.JobInterface,
	backupClient operatorv1alpha1client.EtcdBackupInterface) error {

	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return fmt.Errorf("BackupController could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), etcd_assets.MustAsset("etcd/cluster-backup-job.yaml"))
	if err != nil {
		return fmt.Errorf("BackupController could not decode batchv1 job scheme: %w", err)
	}

	backupFileName := fmt.Sprintf("backup-%s-%s", backup.Name, time.Now().Format("2006-01-02_150405"))

	job := obj.(*batchv1.Job)
	job.Name = names.SimpleNameGenerator.GenerateName(job.Name + "-")
	job.Labels[backupJobLabel] = backup.Name
	job.OwnerReferences = append(job.OwnerReferences, v1.OwnerReference{
		APIVersion: backup.APIVersion,
		Kind:       backup.Kind,
		Name:       backup.Name,
		UID:        backup.UID,
	})

	// we also inject owner references from periodic backups
	for _, r := range backup.OwnerReferences {
		job.OwnerReferences = append(job.OwnerReferences, r)
	}

	job.Spec.Template.Spec.InitContainers[0].Image = operatorImagePullSpec
	job.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec

	job.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: backupDirEnvName, Value: fmt.Sprintf("%s/%s", recentBackupPath, backupFileName)},
		{Name: "ETCDCTL_CERT", Value: "/var/run/secrets/etcd-client/tls.crt"},
		{Name: "ETCDCTL_KEY", Value: "/var/run/secrets/etcd-client/tls.key"},
		{Name: "ETCDCTL_CACERT", Value: "/var/run/configmaps/etcd-ca/ca-bundle.crt"},
	}

	klog.Infof("BackupController starts with backup [%s] as job [%s], writing to filename [%s]", backup.Name, job.Name, backupFileName)
	_, err = jobClient.Create(ctx, job, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("BackupController failed to create job: %w", err)
	}

	backup.Status = operatorv1alpha1.EtcdBackupStatus{
		Conditions: []v1.Condition{
			{
				Type:               string(operatorv1alpha1.BackupPending),
				Reason:             string(operatorv1alpha1.BackupPending),
				Message:            fmt.Sprintf("Executing job %s to save backup file %s", job.Name, backupFileName),
				Status:             v1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now()),
			},
		},
		BackupJob: &operatorv1alpha1.BackupJobReference{
			Namespace: job.Namespace,
			Name:      job.Name,
		},
	}

	_, err = backupClient.UpdateStatus(ctx, &backup, v1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("BackupController error while updating status: %w", err)
	}

	return nil
}
