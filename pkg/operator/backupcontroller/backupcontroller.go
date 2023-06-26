package backupcontroller

import (
	"context"
	"fmt"
	"time"

	backupv1client "github.com/openshift/client-go/backup/clientset/versioned/typed/backup/v1alpha1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type BackupController struct {
	operatorClient        v1helpers.OperatorClient
	clusterOperatorClient configv1client.ClusterOperatorsGetter
	backupsClient         backupv1client.EtcdBackupsGetter

	kubeClient kubernetes.Interface
}

func NewBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	clusterOperatorClient configv1client.ClusterOperatorsGetter,
	backupsClient backupv1client.EtcdBackupsGetter,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	clusterVersionInformer configv1informers.ClusterVersionInformer,
	clusterOperatorInformer configv1informers.ClusterOperatorInformer,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &BackupController{
		operatorClient:        operatorClient,
		clusterOperatorClient: clusterOperatorClient,
		backupsClient:         backupsClient,
		kubeClient:            kubeClient,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupController", syncer)

	return factory.New().ResyncEvery(5*time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		operatorClient.Informer(),
		clusterVersionInformer.Informer(),
		clusterOperatorInformer.Informer(),
	).WithSync(syncer.Sync).ToController("BackupController", eventRecorder.WithComponentSuffix("backup-controller"))
}

func (c *BackupController) sync(ctx context.Context, syncCtx factory.SyncContext) error {

	backups, err := c.backupsClient.EtcdBackups().List(ctx, v1.ListOptions{})
	if err != nil {
		return fmt.Errorf("could not list etcdbackups CRDs, error was: %w", err)
	}

	if backups != nil {
		klog.V(4).Infof("BackupController backup: %v", backups)
	}

	return nil
}
