package setup

import (
	"context"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
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

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	klog.Info("Waiting for completed auth jobs")
	authDone := func(context.Context) (done bool, err error) {
		jobs, err := kubeClient.BatchV1().Jobs("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=tnf-auth",
		})
		if err != nil {
			klog.Warningf("Failed to list jobs: %v", err)
			return false, nil
		}
		if jobs.Items == nil || len(jobs.Items) != 2 {
			klog.Warningf("Expected 2 jobs, got %d", len(jobs.Items))
		}
		for _, job := range jobs.Items {
			if !tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) {
				klog.Warningf("Job %s not complete", job.Name)
				return false, nil
			}
		}
		klog.Info("Auth jobs completed successfully")
		return true, nil
	}
	err = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.AuthJobCompletedTimeout, true, authDone)
	if err != nil {
		klog.Errorf("Timed out waiting for auth jobs to complete: %v", err)
		return err
	}

	klog.Info("Running TNF setup")

	// create tnf cluster config
	cfg, err := config.GetClusterConfig(ctx, kubeClient)
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
