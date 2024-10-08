package backupcontroller

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const cronJobBackupAppLabel = "cluster-backup-cronjob"

// BackupRemovalController is necessary to clean up dangling etcdbackups that have no existing owner refs anymore.
// This scenario arises from periodically triggered backups, which are not automatically garbage collected by kube-controller-manager.
// The reason is that etcdbackups are global, whereas the Job/Pods that create them are namespaced.
// This is a known and documented limitation, which requires us to build this garbage collection below.
type BackupRemovalController struct {
	operatorClient      v1helpers.OperatorClient
	backupsClient       operatorv1alpha1client.EtcdBackupsGetter
	kubeClient          kubernetes.Interface
	featureGateAccessor featuregates.FeatureGateAccess
}

func NewBackupRemovalController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsClient operatorv1alpha1client.EtcdBackupsGetter,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	accessor featuregates.FeatureGateAccess,
	backupInformer factory.Informer,
	jobInformer factory.Informer) factory.Controller {

	c := &BackupRemovalController{
		backupsClient:       backupsClient,
		kubeClient:          kubeClient,
		featureGateAccessor: accessor,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupRemovalController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithInformers(backupInformer, jobInformer).
		WithSync(syncer.Sync).
		ToController("BackupRemovalController", eventRecorder.WithComponentSuffix("backup-removal-controller"))
}

func (c *BackupRemovalController) sync(ctx context.Context, _ factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupRemovalController error while checking feature flags: %v", err)
		}
		return nil
	}

	jobsClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	cronJobJobs, err := jobsClient.List(ctx, v1.ListOptions{LabelSelector: "app=" + cronJobBackupAppLabel})
	if err != nil {
		return fmt.Errorf("BackupRemovalController could not list backup jobs, error was: %w", err)
	}

	jobsByUID := indexJobsByUID(cronJobJobs)

	backupsClient := c.backupsClient.EtcdBackups()
	backups, err := backupsClient.List(ctx, v1.ListOptions{})
	if err != nil {
		return fmt.Errorf("BackupRemovalController could not list etcdbackups CRDs, error was: %w", err)
	}

	for _, backup := range backups.Items {
		for _, ownerRef := range backup.OwnerReferences {
			if ownerRef.APIVersion != "batch/v1" && ownerRef.Kind != "Job" {
				continue
			}

			if _, ok := jobsByUID[ownerRef.UID]; !ok {
				klog.Infof("BackupRemovalController backup job with name [%s] has no owning job [%v] anymore, removing", backup.Name, ownerRef)

				err := backupsClient.Delete(ctx, backup.Name, v1.DeleteOptions{})
				if err != nil {
					return fmt.Errorf("BackupRemovalController could not remove etcdbackup [%s], error was: %w", backup.Name, err)
				}

				break
			}
		}
	}

	return nil
}

func indexJobsByUID(jobs *batchv1.JobList) map[types.UID]batchv1.Job {
	m := map[types.UID]batchv1.Job{}
	if jobs == nil {
		return m
	}

	for _, j := range jobs.Items {
		objectUid := j.ObjectMeta.UID
		m[objectUid] = *j.DeepCopy()
	}

	return m
}
