package setup

import (
	"context"
	"fmt"
	"os"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/pcs"
)

func RunTnfSetup() error {

	klog.Info("Setting up clients etc. for TNF setup")

	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	protoConfig := rest.CopyConfig(clientConfig)
	protoConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	protoConfig.ContentType = "application/vnd.kubernetes.protobuf"

	// This kube client use protobuf, do not use it for CR
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	operatorConfigClient, err := operatorversionedclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	etcdImagePullSpec := os.Getenv("ETCD_IMAGE_PULLSPEC")
	if etcdImagePullSpec == "" {
		return fmt.Errorf("ETCD_IMAGE_PULLSPEC environment variable not set")
	}

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	klog.Info("Running TNF setup")

	// create tnf cluster config
	cfg, err := config.GetClusterConfig(ctx, kubeClient, etcdImagePullSpec)
	if err != nil {
		return err
	}

	// configure pcs cluster
	configured, err := pcs.ConfigureCluster(ctx, cfg)
	if err != nil {
		return err
	} else if configured {
		// give the cluster some time for sync
		time.Sleep(5 * time.Second)
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
	err = etcd.RemoveStaticContainer(ctx, operatorConfigClient)
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
