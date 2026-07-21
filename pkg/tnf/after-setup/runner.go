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
		// Pre-transition (bootstrap): wait for setup job
		klog.Info("Pre-transition: waiting for setup job to complete")
		err = waitForSetupJobCompletion(ctx, kubeClient)
		if err != nil {
			return err
		}
	} else {
		// Post-transition (Day 2): wait for update-setup job that includes this node
		// Get current node object to build nodeInfo (name, IP, UID)
		currentNode, err := kubeClient.CoreV1().Nodes().Get(ctx, currentNodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get current node %s: %w", currentNodeName, err)
		}

		currentNodeIP, err := tools.GetNodeIPForPacemaker(*currentNode)
		if err != nil {
			return fmt.Errorf("failed to get IP for node %s: %w", currentNodeName, err)
		}

		node := nodeInfo{
			Name: currentNodeName,
			IP:   currentNodeIP,
			UID:  string(currentNode.UID),
		}

		klog.Infof("Post-transition: waiting for update-setup job containing node %s (UID: %s) to complete", node.Name, node.UID)
		err = waitForUpdateSetupJobCompletion(ctx, kubeClient, node)
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

// waitForSetupJobCompletion waits for the setup job to complete (bootstrap phase)
func waitForSetupJobCompletion(ctx context.Context, kubeClient kubernetes.Interface) error {
	klog.Info("Waiting for setup job to complete")
	setupDone := func(context.Context) (done bool, err error) {
		setupJobs, err := kubeClient.BatchV1().Jobs("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeSetup.GetNameLabelValue()),
		})
		if err != nil {
			klog.Warningf("Failed to list setup jobs: %v", err)
			return false, nil
		}
		if setupJobs.Items == nil || len(setupJobs.Items) != 1 {
			klog.V(4).Infof("Expected 1 setup job, got %d", len(setupJobs.Items))
			return false, nil
		}
		job := setupJobs.Items[0]
		if !jobs.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) {
			klog.V(4).Infof("Setup job %s not yet complete", job.Name)
			return false, nil
		}
		klog.Info("Setup job completed successfully")
		return true, nil
	}
	err := wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.SetupJobCompletedTimeout, true, setupDone)
	if err != nil {
		return fmt.Errorf("timed out waiting for setup job to complete: %w", err)
	}
	return nil
}

// nodeInfo mirrors the structure used in lifecycle_reconciliation.go for ConfigMap node lists
type nodeInfo struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	UID  string `json:"uid"`
}

// waitForUpdateSetupJobCompletion waits for an update-setup job that includes the current node (Day 2 phase)
// Checks both node name AND UID to protect against node replacement scenarios (same name, different UID).
func waitForUpdateSetupJobCompletion(ctx context.Context, kubeClient kubernetes.Interface, node nodeInfo) error {
	klog.Infof("Waiting for update-setup job containing node %s (UID: %s) to complete", node.Name, node.UID)

	updateSetupDone := func(context.Context) (done bool, err error) {
		// Get all update-setup ConfigMaps
		configMaps, err := kubeClient.CoreV1().ConfigMaps("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeUpdateSetup.GetNameLabelValue()),
		})
		if err != nil {
			klog.Warningf("Failed to list update-setup ConfigMaps: %v", err)
			return false, nil
		}

		// Find the latest generation ConfigMap that contains this node (by name AND UID)
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

			// Check if this node is in the ConfigMap (match both name AND UID)
			nodeFound := false
			for _, n := range nodes {
				if n.Name == node.Name && n.UID == node.UID {
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
			klog.V(4).Infof("No update-setup ConfigMap found containing node %s (UID: %s)", node.Name, node.UID)
			return false, nil
		}

		klog.V(4).Infof("Found update-setup ConfigMap generation %d containing node %s (UID: %s)", latestGeneration, node.Name, node.UID)

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
				klog.Infof("Update-setup job %s completed successfully (contains node %s)", job.Name, node.Name)
				return true, nil
			}
		}

		klog.V(4).Infof("Update-setup job for generation %d not yet complete", latestGeneration)
		return false, nil
	}

	err := wait.PollUntilContextTimeout(ctx, tools.JobPollInterval, tools.UpdateSetupJobCompletedTimeout, true, updateSetupDone)
	if err != nil {
		return fmt.Errorf("timed out waiting for update-setup job containing node %s (UID: %s) to complete: %w", node.Name, node.UID, err)
	}
	return nil
}
