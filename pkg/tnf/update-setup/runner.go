package updatesetup

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
)

func RunTnfUpdateSetup() error {

	klog.Info("Setting up clients etc. for TNF update-setup")

	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(clock.RealClock{}, clientConfig, operatorv1.GroupVersion.WithResource("etcds"), operatorv1.GroupVersion.WithKind("Etcd"), operator.ExtractStaticPodOperatorSpec, operator.ExtractStaticPodOperatorStatus)
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

	dynamicInformers.Start(ctx.Done())
	dynamicInformers.WaitForCacheSync(ctx.Done())

	// Get the current node name from environment
	currentNodeName := os.Getenv("MY_NODE_NAME")
	if currentNodeName == "" {
		return fmt.Errorf("MY_NODE_NAME environment variable not set")
	}

	klog.Infof("Running TNF update-setup")

	// check if cluster is running on this node
	command := "/usr/sbin/pcs cluster status"
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return fmt.Errorf("cluster not running on this node, will retry on other node: %w", err)
	}

	// Register pacemaker alert agents for fencing taint/untaint. This runs on
	// every invocation (i.e. on every CEO reconcile once TNF is set up,
	// including upgrades), independent of whether a node is being replaced,
	// because the alert scripts may only become available well after this job
	// first starts running.
	if err := pcs.ConfigureAlertsWithRetry(ctx); err != nil {
		return err
	}

	// Get current cluster config from Kubernetes
	cfg, err := config.GetClusterConfig(ctx, kubeClient)
	if err != nil {
		return err
	}

	// Determine which node we are and get the new IP
	var currentNodeIP, otherNodeName, otherNodeIP string
	if cfg.NodeName1 == currentNodeName {
		currentNodeIP = cfg.NodeIP1
		otherNodeName = cfg.NodeName2
		otherNodeIP = cfg.NodeIP2
	} else if cfg.NodeName2 == currentNodeName {
		currentNodeIP = cfg.NodeIP2
		otherNodeName = cfg.NodeName1
		otherNodeIP = cfg.NodeIP1
	} else {
		return fmt.Errorf("current node %s not found in cluster config (nodes: %s, %s)", currentNodeName, cfg.NodeName1, cfg.NodeName2)
	}

	// find offline node
	command = "/usr/sbin/pcs status nodes corosync | grep Offline | awk '{print $2}'"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		klog.Errorf("Failed to find offline node: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
		return err
	}
	offlineNodeName := strings.TrimSpace(stdOut)

	if offlineNodeName == "" {
		klog.Info("No offline node found, checking if cluster has correct number of nodes configured")

		// Check if Pacemaker cluster has exactly 2 nodes configured
		// This catches the case where a node was removed from Pacemaker but not re-added
		// (e.g., update-setup job failed after node removal)
		command = "/usr/sbin/pcs status xml"
		stdOut, stdErr, err = exec.Execute(ctx, command)
		if err != nil {
			klog.Errorf("Failed to query cluster status: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
			return fmt.Errorf("failed to check cluster node count: %w", err)
		}

		var result pacemaker.PacemakerResult
		if parseErr := xml.Unmarshal([]byte(stdOut), &result); parseErr != nil {
			klog.Errorf("Failed to parse pcs status xml: %v", parseErr)
			return fmt.Errorf("failed to parse cluster status: %w", parseErr)
		}

		// Count total nodes configured (online or offline)
		totalNodes := len(result.Nodes.Node)
		if totalNodes == 2 {
			klog.Info("Cluster has 2 nodes configured, nothing to do")
			return nil
		}

		// Cluster missing a node - determine which one and add it back
		klog.Warningf("Cluster has %d nodes configured (expected 2), determining missing node", totalNodes)

		// Build set of nodes in Pacemaker
		pacemakerNodes := make(map[string]bool)
		for _, node := range result.Nodes.Node {
			pacemakerNodes[node.Name] = true
		}

		// Determine which K8s node is missing from Pacemaker
		var missingNodeName string
		if !pacemakerNodes[cfg.NodeName1] {
			missingNodeName = cfg.NodeName1
		} else if !pacemakerNodes[cfg.NodeName2] {
			missingNodeName = cfg.NodeName2
		} else {
			// Should never happen - totalNodes != 2 but both K8s nodes are in Pacemaker
			return fmt.Errorf("cluster has %d nodes but both K8s nodes (%s, %s) are in Pacemaker - unexpected state", totalNodes, cfg.NodeName1, cfg.NodeName2)
		}

		klog.Infof("Node %q is missing from Pacemaker cluster, adding it back", missingNodeName)

		// Add missing node back to cluster (skip remove step - already removed)
		return addNodeBackToCluster(ctx, kubeClient, missingNodeName, currentNodeName, cfg)
	}

	klog.Infof("Current node: %q (IP: %s), Other node: %q (IP: %s), Offline node: %q", currentNodeName, currentNodeIP, otherNodeName, otherNodeIP, offlineNodeName)

	// don't start the cluster on the new node too early, it might result in etcd start failure because of missing manifests on the new node
	klog.Info("Waiting for etcd revision update before going on...")
	err = etcd.WaitForStableRevision(ctx, operatorClient)
	if err != nil {
		klog.Error(err, "Failed to wait for etcd container transition")
		return err
	}

	commands := []string{
		// Remove offline node from the cluster configuration
		fmt.Sprintf("/usr/sbin/pcs cluster node remove %s --force --skip-offline", offlineNodeName),
		// Add new node to the cluster configuration
		fmt.Sprintf("/usr/sbin/pcs cluster node add %s", otherNodeName),
	}
	err = runCommands(ctx, commands)
	if err != nil {
		return err
	}

	// Reconfigure cluster after node change (fencing, etcd, validation)
	return reconfigureClusterAfterNodeChange(ctx, kubeClient, currentNodeName, cfg)
}

// addNodeBackToCluster adds a missing node back to the Pacemaker cluster.
// This handles the recovery case where a node was removed from Pacemaker but not re-added
// (e.g., update-setup job failed after node removal).
func addNodeBackToCluster(ctx context.Context, kubeClient kubernetes.Interface, missingNodeName string, currentNodeName string, cfg config.ClusterConfig) error {
	klog.Infof("Adding node %q back to Pacemaker cluster", missingNodeName)

	// Validate node name before constructing command (defense-in-depth against bad data in PacemakerCluster CR)
	if errs := validation.IsDNS1123Label(missingNodeName); len(errs) > 0 {
		return fmt.Errorf("invalid node name %q: %v", missingNodeName, errs)
	}

	// Add missing node to cluster configuration
	// Use --force to override warning about existing cluster config files on the node
	// (the node was removed from cluster but retains config files)
	command := fmt.Sprintf("/usr/sbin/pcs cluster node add %s --force", missingNodeName)
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		klog.Errorf("Failed to add node to cluster: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
		return fmt.Errorf("failed to add missing node %s: %w", missingNodeName, err)
	}
	klog.Infof("Successfully executed: %s", command)

	// Reconfigure cluster after node change (fencing, etcd, validation)
	return reconfigureClusterAfterNodeChange(ctx, kubeClient, currentNodeName, cfg)
}

// reconfigureClusterAfterNodeChange handles the common post-node-change operations:
// configures fencing, updates etcd resource, removes unstarted members, starts cluster, validates.
// Called by both the offline-node path (after remove+add) and missing-node path (after add).
func reconfigureClusterAfterNodeChange(ctx context.Context, kubeClient kubernetes.Interface, currentNodeName string, cfg config.ClusterConfig) error {
	// Update fence devices (both nodes in correct order)
	// Node order matters: resources can't be restarted while fencing isn't configured on all nodes
	err := pcs.ConfigureFencing(ctx, kubeClient, []string{cfg.NodeName1, cfg.NodeName2})
	if err != nil {
		klog.Error(err, "Failed to configure fencing, skipping update of etcd! Restart update-setup job when fencing config is fixed!")
		return err
	}

	commands := []string{
		// Force new cluster on next etcd restart on current node
		fmt.Sprintf("crm_attribute --lifetime reboot --node %s --name \"force_new_cluster\" --update %s", currentNodeName, currentNodeName),
		// Update etcd resource with correct node IP map
		fmt.Sprintf("/usr/sbin/pcs resource update etcd node_ip_map=\"%s:%s;%s:%s\" --wait=300", cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2),
	}
	err = runCommands(ctx, commands)
	if err != nil {
		return err
	}

	// Remove unstarted etcd member if present
	command := "podman exec etcd /usr/bin/etcdctl member list | grep unstarted | awk -F, '{ print $1 }'"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		klog.Errorf("Failed to find unstarted etcd member: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
	} else {
		unstartedMemberID := strings.TrimSpace(stdOut)
		if unstartedMemberID != "" {
			command = fmt.Sprintf("podman exec etcd /usr/bin/etcdctl member remove %s", unstartedMemberID)
			stdOut, stdErr, err = exec.Execute(ctx, command)
			if err != nil {
				klog.Errorf("Failed to remove unstarted etcd member: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
				return err
			}
			klog.Infof("Removed unstarted etcd member: %s", unstartedMemberID)
		}
	}

	// Wait for cluster to settle
	time.Sleep(10 * time.Second)

	commands = []string{
		// Enable cluster on all nodes
		"/usr/sbin/pcs cluster enable --all",
		// Start cluster on all nodes
		"/usr/sbin/pcs cluster start --all",
	}
	err = runCommands(ctx, commands)
	if err != nil {
		return err
	}

	// Wait for cluster to fully start before validating
	klog.Info("Waiting for cluster to stabilize...")
	time.Sleep(10 * time.Second)

	// Validate final cluster state
	return validateClusterState(ctx)
}

// validateClusterState validates that the Pacemaker cluster has exactly 2 nodes online.
// Returns an error if validation fails.
func validateClusterState(ctx context.Context) error {
	klog.Info("Validating final cluster configuration...")
	command := "/usr/sbin/pcs status xml"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		klog.Errorf("Failed to query cluster status: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
		return fmt.Errorf("failed to validate cluster state: %w", err)
	}

	var result pacemaker.PacemakerResult
	if parseErr := xml.Unmarshal([]byte(stdOut), &result); parseErr != nil {
		klog.Errorf("Failed to parse pcs status xml: %v", parseErr)
		return fmt.Errorf("failed to parse cluster status: %w", parseErr)
	}

	// Count online nodes
	onlineNodes := []string{}
	for _, node := range result.Nodes.Node {
		if node.Online == "true" {
			onlineNodes = append(onlineNodes, node.Name)
		}
	}

	if len(onlineNodes) != 2 {
		return fmt.Errorf("invalid cluster state: expected 2 online nodes, found %d: %v (this will retry until auth runs on new node and cluster is complete)", len(onlineNodes), onlineNodes)
	}

	klog.Infof("Cluster validation successful: 2 nodes online: %v", onlineNodes)
	return nil
}

func runCommands(ctx context.Context, commands []string) error {
	for _, command := range commands {
		stdOut, stdErr, err := exec.Execute(ctx, command)
		if err != nil {
			klog.Errorf("Failed to run update-setup command: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
			return err
		}
		klog.Infof("Successfully executed: %s", command)
	}
	return nil
}
