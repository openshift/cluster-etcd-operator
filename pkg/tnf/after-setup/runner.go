package after_setup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	operatorv1 "github.com/openshift/api/operator/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/kubelet"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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

	// Get current node name from environment
	currentNodeName := os.Getenv("MY_NODE_NAME")
	if currentNodeName == "" {
		return fmt.Errorf("MY_NODE_NAME environment variable not set")
	}
	klog.Infof("After-setup for node: %s", currentNodeName)

	// Check if external etcd transition is complete
	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(clock.RealClock{}, clientConfig, operatorv1.GroupVersion.WithResource("etcds"), operatorv1.GroupVersion.WithKind("Etcd"), operator.ExtractStaticPodOperatorSpec, operator.ExtractStaticPodOperatorStatus)
	if err != nil {
		return err
	}
	dynamicInformers.Start(ctx.Done())
	dynamicInformers.WaitForCacheSync(ctx.Done())

	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check external etcd transition status: %w", err)
	}

	if !transitionComplete {
		// Pre-transition (bootstrap): wait for transition flag to be set
		// The setup job sets this flag when Pacemaker configuration is complete
		klog.Info("Pre-transition: waiting for transition to complete")
		err = waitForTransitionComplete(ctx, operatorClient)
		if err != nil {
			return err
		}
	} else {
		// Post-transition (Day 2): wait for update-setup job that includes this node
		klog.Infof("Post-transition: waiting for update-setup job containing node %s to complete", currentNodeName)
		err = waitForUpdateSetupJobCompletion(ctx, kubeClient, currentNodeName)
		if err != nil {
			return err
		}
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

// waitForTransitionComplete waits for the ExternalEtcdTransitionCompleted flag to be set (bootstrap phase)
// The setup job sets this flag when Pacemaker configuration is complete
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
		return fmt.Errorf("timed out waiting for transition to complete: %w", err)
	}
	return nil
}

// nodeInfo mirrors the structure used in lifecycle_reconciliation.go for ConfigMap node lists
type nodeInfo struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// waitForUpdateSetupJobCompletion waits for an update-setup job that includes the current node (Day 2 phase)
func waitForUpdateSetupJobCompletion(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	klog.Infof("Waiting for update-setup job containing node %s to complete", nodeName)

	updateSetupDone := func(context.Context) (done bool, err error) {
		// Get all update-setup ConfigMaps
		configMaps, err := kubeClient.CoreV1().ConfigMaps("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeUpdateSetup.GetNameLabelValue()),
		})
		if err != nil {
			klog.Warningf("Failed to list update-setup ConfigMaps: %v", err)
			return false, nil
		}

		// Find the latest generation ConfigMap that contains this node
		var latestGeneration int64 = -1
		var targetConfigMap *corev1.ConfigMap
		for i := range configMaps.Items {
			cm := &configMaps.Items[i]
			genStr := cm.Data["generation"]
			gen, err := strconv.ParseInt(genStr, 10, 64)
			if err != nil {
				klog.Warningf("Failed to parse generation from ConfigMap %s: %v", cm.Name, err)
				continue
			}

			// Decode node list from ConfigMap
			nodesJSON := cm.Data["nodes"]
			var nodes []nodeInfo
			if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
				klog.Warningf("Failed to unmarshal nodes from ConfigMap %s: %v", cm.Name, err)
				continue
			}

			// Check if this node is in the ConfigMap
			nodeFound := false
			for _, node := range nodes {
				if node.Name == nodeName {
					nodeFound = true
					break
				}
			}

			// Track latest generation that contains this node
			if nodeFound && gen > latestGeneration {
				latestGeneration = gen
				targetConfigMap = cm
			}
		}

		if targetConfigMap == nil {
			klog.V(4).Infof("No update-setup ConfigMap found containing node %s", nodeName)
			return false, nil
		}

		klog.V(4).Infof("Found update-setup ConfigMap generation %d containing node %s", latestGeneration, nodeName)

		// Check if update-setup job for this generation is complete
		updateSetupJobs, err := kubeClient.BatchV1().Jobs("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeUpdateSetup.GetNameLabelValue()),
		})
		if err != nil {
			klog.Warningf("Failed to list update-setup jobs: %v", err)
			return false, nil
		}

		for _, job := range updateSetupJobs.Items {
			if jobs.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) {
				klog.Infof("Update-setup job %s completed successfully (contains node %s)", job.Name, nodeName)
				return true, nil
			}
		}

		klog.V(4).Infof("Update-setup job for generation %d not yet complete", latestGeneration)
		return false, nil
	}

	err := wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.UpdateSetupJobCompletedTimeout, true, updateSetupDone)
	if err != nil {
		return fmt.Errorf("timed out waiting for update-setup job containing node %s to complete: %w", nodeName, err)
	}
	return nil
}
