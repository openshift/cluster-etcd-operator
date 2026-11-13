package after_setup

import (
	"context"
	"k8s.io/apimachinery/pkg/types"
	"os"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	operatorclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/kubelet"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func RunTnfAfterSetup() error {

	klog.Info("Setting up clients etc. for TNF after setup job")

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

	klog.Info("Running TNF after setup")

	operatorClient, err := operatorclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	setupDone := func(ctx context.Context) (bool, error) {
		return generation.SetupReady(ctx, kubeClient, operatorClient.Etcds(), os.Getenv("MY_NODE_NAME"), types.UID(os.Getenv(generation.NodeUIDEnv)))
	}
	err = wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.SetupJobCompletedTimeout, true, setupDone)
	if err != nil {
		klog.Errorf("Timed out waiting for setup job to complete: %v", err)
		return err
	}

	// disable kubelet service, it's managed by pacemaker now
	err = kubelet.Disable(ctx)
	if err != nil {
		klog.Errorf("Failed to disable kubelet service: %v", err)
		return err
	}

	klog.Info("TNF after setup done")

	return nil
}
