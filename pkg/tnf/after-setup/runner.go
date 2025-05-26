package auth

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
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

	klog.Info("Waiting for completed setup job")
	setupDone := func(context.Context) (done bool, err error) {
		jobs, err := kubeClient.BatchV1().Jobs("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=tnf-setup",
		})
		if err != nil {
			klog.Warningf("Failed to list jobs: %v", err)
			return false, nil
		}
		if jobs.Items == nil || len(jobs.Items) != 1 {
			klog.Warningf("Expected 1 job, got %d", len(jobs.Items))
		}
		job := jobs.Items[0]
		if !tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) {
			klog.Warningf("Job %s not complete", job.Name)
			return false, nil
		}
		klog.Info("Setup job completed successfully")
		return true, nil
	}
	err = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.SetupJobCompletedTimeout, true, setupDone)
	if err != nil {
		klog.Errorf("Timed out waiting for setup job to complete: %v", err)
		return err
	}

	// disable kubelet service, it's managed by pacemaker now
	klog.Info("Disabling kubelet service")
	command := "systemctl disable kubelet"
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return err
	}

	klog.Info("TNF after setup done")

	return nil
}
