package periodicbackupcontroller

import (
	"context"
	"fmt"
	"time"

	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configv1alpha1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
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

type PeriodicBackupController struct {
	operatorClient        v1helpers.OperatorClient
	clusterOperatorClient configv1client.ClusterOperatorsGetter
	backupsClient         configv1alpha1client.BackupsGetter

	kubeClient kubernetes.Interface
}

func NewPeriodicBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	clusterOperatorClient configv1client.ClusterOperatorsGetter,
	backupsClient configv1alpha1client.BackupsGetter,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	clusterVersionInformer configv1informers.ClusterVersionInformer,
	clusterOperatorInformer configv1informers.ClusterOperatorInformer,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &PeriodicBackupController{
		operatorClient:        operatorClient,
		clusterOperatorClient: clusterOperatorClient,
		backupsClient:         backupsClient,
		kubeClient:            kubeClient,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("PeriodicBackupController", syncer)

	return factory.New().ResyncEvery(5*time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		operatorClient.Informer(),
		clusterVersionInformer.Informer(),
		clusterOperatorInformer.Informer(),
	).WithSync(syncer.Sync).ToController("PeriodicBackupController", eventRecorder.WithComponentSuffix("periodic-backup-controller"))
}

func (c *PeriodicBackupController) sync(ctx context.Context, syncCtx factory.SyncContext) error {

	backupCrd, err := c.backupsClient.Backups().Get(ctx, "cluster", v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not get cluster backup CRD, error was: %w", err)
	}

	if backupCrd != nil {
		klog.V(4).Infof("PeriodicBackupController backup: %v", backupCrd)
	}

	return nil
}
