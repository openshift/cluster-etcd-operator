package fencing

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
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

	klog.Info("Waiting for completed setup job")
	setupDone := func(context.Context) (done bool, err error) {
		setupJobs, err := kubeClient.BatchV1().Jobs("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeSetup.GetNameLabelValue()),
		})
		if err != nil {
			klog.Warningf("Failed to list setupJobs: %v", err)
			return false, nil
		}
		if setupJobs.Items == nil {
			klog.Warningf("Expected 1 job, found none")
			return false, nil
		}
		if len(setupJobs.Items) != 1 {
			klog.Warningf("Expected 1 job, found %d", len(setupJobs.Items))
			return false, nil
		}
		job := setupJobs.Items[0]
		if !jobs.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) {
			klog.Warningf("Job %s not complete", job.Name)
			return false, nil
		}
		klog.Info("Setup job completed successfully")
		return true, nil
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
