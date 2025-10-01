package auth

import (
	"context"
	"fmt"

	configversionedclient "github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
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
		return err
	}

	// create token file
	// use cluster id as immutable token
	clusterVersion, err := configClient.ConfigV1().ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return err
	}
	tokenValue := clusterVersion.Spec.ClusterID
	tokenPath := "/var/lib/pcsd/token"
	command := fmt.Sprintf("echo %q > %s", tokenValue, tokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return err
	}
	command = fmt.Sprintf("chmod 0600 %s", tokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return err
	}

	// run pcs auth
	command = fmt.Sprintf("/usr/sbin/pcs pcsd accept_token %s", tokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return err
	}

	command = fmt.Sprintf("/usr/sbin/pcs host auth %s addr=%s %s addr=%s --token %s --debug", cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2, tokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return err
	}

	klog.Info("TNF auth done")

	return nil
}
