package fencing

import (
	"context"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	operatorclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func RunFencingSetup() error {

	klog.Info("Setting up clients etc. for TNF fencing job")

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

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	klog.Info("Running TNF pacemaker fencing configuration")

	operatorClient, err := operatorclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	setupDone := func(ctx context.Context) (bool, error) {
		return generation.SetupReady(ctx, kubeClient, operatorClient.Etcds(), "", "")
	}
	err = wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.SetupJobCompletedTimeout, true, setupDone)
	if err != nil {
		klog.Errorf("Timed out waiting for setup job to complete: %v", err)
		return err
	}

	klog.Info("Running TNF pacemaker fencing configuration")

	// create tnf cluster config
	cfg, err := config.GetClusterConfigIgnoreMissingNode(ctx, kubeClient)
	if err != nil {
		return err
	}

	err = pcs.ConfigureFencing(ctx, kubeClient, []string{cfg.NodeName1, cfg.NodeName2})
	if err != nil {
		return err
	}

	// get pcs cib
	cib, err := pcs.GetCIB(ctx)
	if err != nil {
		return err
	}

	klog.Infof("TNF pacemaker fencing configuration done! CIB:\n%s", cib)

	return nil
}
