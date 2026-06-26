package fencing

import (
	"context"
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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

	// Set up operator client to check transition status
	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(
		clock.RealClock{},
		clientConfig,
		operatorv1.GroupVersion.WithResource("etcds"),
		operatorv1.GroupVersion.WithKind("Etcd"),
		operator.ExtractStaticPodOperatorSpec,
		operator.ExtractStaticPodOperatorStatus,
	)
	if err != nil {
		return err
	}
	dynamicInformers.Start(ctx.Done())
	dynamicInformers.WaitForCacheSync(ctx.Done())

	// Check if update-setup ConfigMap exists to determine what to wait for
	configMaps, err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=" + tools.TnfUpdateSetupComponentValue,
	})
	if err != nil {
		klog.Errorf("Failed to list update-setup ConfigMaps: %v", err)
		return fmt.Errorf("failed to list update-setup ConfigMaps: %w", err)
	}

	if len(configMaps.Items) > 0 {
		// Post-transition: update-setup ConfigMap exists, wait for job to complete
		klog.Info("Update-setup ConfigMap found, waiting for update-setup job to complete")
		err = waitForUpdateSetupCompletion(ctx, kubeClient)
		if err != nil {
			return err
		}
	} else {
		// Pre-transition: no ConfigMap, wait for transition
		klog.Info("No update-setup ConfigMap found, waiting for transition to complete")
		err = waitForTransitionComplete(ctx, operatorClient)
		if err != nil {
			return err
		}
	}

	// Get current ready control plane nodes from K8s
	nodes, err := ceohelpers.ListReadyNodes(ctx, kubeClient)
	if err != nil {
		klog.Errorf("Failed to list ready control plane nodes: %v", err)
		return fmt.Errorf("failed to list ready control plane nodes: %w", err)
	}

	if len(nodes) == 0 {
		klog.Errorf("No ready control plane nodes found")
		return fmt.Errorf("no ready control plane nodes found")
	}

	nodeNames := make([]string, len(nodes))
	for i, node := range nodes {
		nodeNames[i] = node.Name
	}

	klog.Infof("Configuring fencing for nodes: %v", nodeNames)
	err = pcs.ConfigureFencing(ctx, kubeClient, nodeNames)
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

// waitForTransitionComplete waits for the ExternalEtcdTransitionCompleted flag to be set (bootstrap phase).
func waitForTransitionComplete(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	klog.Info("Waiting for external etcd transition to complete")
	transitionDone := func(context.Context) (done bool, err error) {
		complete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
		if err != nil {
			klog.Warningf("Failed to check transition status: %v", err)
			return false, nil
		}
		if !complete {
			klog.V(4).Info("Transition not yet complete")
			return false, nil
		}
		klog.Info("External etcd transition completed successfully")
		return true, nil
	}
	err := wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.SetupJobCompletedTimeout, true, transitionDone)
	if err != nil {
		klog.Errorf("Timed out waiting for transition to complete: %v", err)
		return fmt.Errorf("timed out waiting for transition to complete: %w", err)
	}
	return nil
}

// waitForUpdateSetupCompletion waits for the update-setup job to complete (post-transition phase).
func waitForUpdateSetupCompletion(ctx context.Context, kubeClient kubernetes.Interface) error {
	klog.Info("Waiting for update-setup job to complete")
	updateSetupDone := func(context.Context) (done bool, err error) {
		updateSetupJobName := tools.JobTypeUpdateSetup.GetJobName(nil)
		job, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, updateSetupJobName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// No job exists - nothing to wait for
				klog.V(4).Info("Update-setup job not found, proceeding")
				return true, nil
			}
			klog.Warningf("Failed to get update-setup job: %v", err)
			return false, nil
		}

		if jobs.IsComplete(*job) {
			klog.Info("Update-setup job completed successfully")
			return true, nil
		}

		if jobs.IsFailed(*job) {
			klog.Warningf("Update-setup job failed, but proceeding with fencing configuration")
			return true, nil
		}

		klog.V(4).Info("Update-setup job still running")
		return false, nil
	}

	err := wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.UpdateSetupJobCompletedTimeout, true, updateSetupDone)
	if err != nil {
		klog.Errorf("Timed out waiting for update-setup job to complete: %v", err)
		return fmt.Errorf("timed out waiting for update-setup job to complete: %w", err)
	}
	return nil
}
