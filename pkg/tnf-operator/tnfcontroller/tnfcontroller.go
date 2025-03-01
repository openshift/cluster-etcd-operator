package tnfcontroller

import (
	"context"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf-operator/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf-operator/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf-operator/pcs"
)

type TnfController struct {
	ctx               context.Context
	kubeClient        kubernetes.Interface
	operatorClient    *operatorversionedclient.Clientset
	eventRecorder     events.Recorder
	etcdImagePullSpec string
	enqueueFn         func()
}

func NewTnfController(ctx context.Context, kubeClient kubernetes.Interface, operatorClient *operatorversionedclient.Clientset,
	eventRecorder events.Recorder, etcdImagePullSpec string) factory.Controller {
	c := &TnfController{
		ctx:               ctx,
		kubeClient:        kubeClient,
		operatorClient:    operatorClient,
		eventRecorder:     eventRecorder,
		etcdImagePullSpec: etcdImagePullSpec,
	}

	syncCtx := factory.NewSyncContext("TnfController", eventRecorder.WithComponentSuffix("tnf-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	//livenessChecker.Add("TargetConfigController", syncer)

	return factory.New().
		WithSyncContext(syncCtx).
		// TODO what's the best way to trigger a reconcile?
		// Without this it never was called...
		ResyncEvery(time.Minute).
		WithSync(syncer.Sync).
		ToController("TnfController", syncCtx.Recorder())

}

func (c *TnfController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.Info("Reconciling TNF")

	// create tnf cluster config
	cfg, err := config.GetClusterConfig(ctx, c.kubeClient, c.etcdImagePullSpec)
	if err != nil {
		return err
	}

	// configure pcs cluster
	configured, err := pcs.ConfigureCluster(ctx, cfg)
	if err != nil {
		return err
	} else if configured {
		// TODO how to delay the requeue...?
		c.enqueueFn()
		return nil
	}

	// Etcd handover

	// configure etcd resource - it won't start etcd before CEO managed etcd is removed per node
	err = pcs.ConfigureEtcd(ctx, cfg)
	if err != nil {
		return err
	}

	// configure etcd constraints
	configured, err = pcs.ConfigureConstraints(ctx)
	if err != nil {
		return err
	} else if configured {
		c.enqueueFn()
		return nil
	}

	// remove CEO managed etcd container
	etcdOp, err := etcd.RemoveStaticContainer(ctx, c.operatorClient)
	if err != nil {
		return err
	}
	if !etcd.StaticContainerRemoved(etcdOp) {
		c.enqueueFn()
		return nil
	}

	// get pcs cib
	cib, err := pcs.GetCIB(ctx)
	if err != nil {
		return err
	}

	// TODO where to store CIB for debugging?
	// just log for now
	klog.Infof("HA setup done! CIB:\n%s", cib)

	return nil
}

func (c *TnfController) Enqueue() {
	c.enqueueFn()
}
