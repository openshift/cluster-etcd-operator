package controller

import (
	"context"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/pcs"
)

type TnfSetupRunner struct {
	kubeClient        kubernetes.Interface
	operatorClient    *operatorversionedclient.Clientset
	eventRecorder     events.Recorder
	etcdImagePullSpec string
}

func NewTnfSetupRunner(kubeClient kubernetes.Interface, operatorClient *operatorversionedclient.Clientset,
	eventRecorder events.Recorder, etcdImagePullSpec string) *TnfSetupRunner {

	c := &TnfSetupRunner{
		kubeClient:        kubeClient,
		operatorClient:    operatorClient,
		eventRecorder:     eventRecorder,
		etcdImagePullSpec: etcdImagePullSpec,
	}

	return c
}

func (c *TnfSetupRunner) Run(ctx context.Context) error {

	klog.Info("Running TNF setup")

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
		// give the cluster some time for sync
		time.Sleep(15 * time.Second)
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
		// give the cluster some time for sync
		time.Sleep(5 * time.Second)
	}

	// remove CEO managed etcd container
	err = etcd.RemoveStaticContainer(ctx, c.operatorClient)
	if err != nil {
		return err
	}

	// get pcs cib
	cib, err := pcs.GetCIB(ctx)
	if err != nil {
		return err
	}

	klog.Infof("HA setup done! CIB:\n%s", cib)

	return nil
}
