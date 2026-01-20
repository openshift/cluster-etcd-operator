package auth

import (
	"context"

	configversionedclient "github.com/openshift/client-go/config/clientset/versioned"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
)

func RunTnfAuth() error {

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

	configClient, err := configversionedclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	klog.Info("Running TNF auth")

	// create tnf cluster config
	cfg, err := config.GetClusterConfig(ctx, kubeClient)
	if err != nil {
		klog.Errorf("Failed to get cluster config: %v", err)
		return err
	}

	// run pcs authentication
	_, err = pcs.Authenticate(ctx, configClient, cfg)
	if err != nil {
		klog.Errorf("Failed to authenticate: %v", err)
		return err
	}

	klog.Info("TNF auth done")

	return nil
}
