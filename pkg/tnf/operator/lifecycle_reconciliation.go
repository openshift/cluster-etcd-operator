package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

var (
	// updateSetupGeneration orders update-setup ConfigMaps when events arrive close together (OCPBUGS-84695).
	updateSetupGeneration int64 = 0

	// reconcilePacemakerConfigMutex serializes ReconcilePacemakerConfig to prevent time-of-check-time-of-use
	// races between drift detection and update-setup triggering. Multiple concurrent calls can detect
	// drift simultaneously, but only one should proceed with reconciliation at a time.
	reconcilePacemakerConfigMutex sync.Mutex

	// updateSetupFunc is a variable to allow mocking in tests
	updateSetupFunc = updateSetup
)

// ReconcilePacemakerConfig performs drift detection and reconciliation after external etcd transition completes.
// Compares K8s node state with pacemaker membership and triggers update-setup if drift is detected.
// This method is called:
//  1. Periodically from sync() (every 30s)
//  2. On node Add events
//  3. On node Update events (IP changes while Ready)
//  4. On node Delete events
//
// Returns nil if no action needed or reconciliation triggered successfully.
//
// Concurrency model: Multiple goroutines can enter this function concurrently and perform
// initial checks (informer sync, node readiness, drift detection). Once drift is detected,
// reconcilePacemakerConfigMutex serializes the check-and-trigger path to prevent time-of-check-time-of-use
// races where multiple goroutines would redundantly create ConfigMaps and trigger jobs. The second
// goroutine will either find no drift (first goroutine fixed it) or trigger a new reconciliation
// with a higher generation number. The generation counter ensures correctness (highest wins).
func (c *PacemakerLifecycleManager) ReconcilePacemakerConfig(ctx context.Context) error {
	// Check if node informer has synced
	if c.nodeInformer == nil || !c.nodeInformer.HasSynced() {
		klog.V(4).Infof("Skipping drift reconciliation - node informer not synced yet")
		return nil
	}

	// Get K8s control plane nodes
	k8sNodes, err := ceohelpers.ListNodesFromInformer(c.nodeInformer)
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	// Validate node count (pacemaker only supports 2 nodes)
	if len(k8sNodes) > 2 {
		klog.Warningf("Found %d control plane nodes - pacemaker behavior undefined for >2 nodes, no reconciliation action taken", len(k8sNodes))
		return nil
	}

	// Check all K8s nodes are Ready before attempting drift reconciliation
	// This ensures we don't try to reconcile while nodes are in flux (e.g., single-node case after delete)
	for _, node := range k8sNodes {
		if !tools.IsNodeReady(node) {
			klog.V(4).Infof("Node %s is not Ready - skipping drift reconciliation (will retry in next sync)", node.Name)
			return nil
		}
	}

	// Get pacemaker nodes from PacemakerCluster CR
	pacemakerNodes, err := c.getPacemakerNodes()
	if err != nil {
		// CR might not exist yet during initial setup, don't treat as error
		klog.V(4).Infof("Skipping reconciliation check - failed to get pacemaker nodes: %v", err)
		return nil
	}

	// Detect drift (compares node names and IPs)
	hasDrift := c.detectDrift(k8sNodes, pacemakerNodes)
	if !hasDrift {
		klog.V(4).Infof("No drift detected between K8s (%d nodes) and pacemaker (%d nodes)",
			len(k8sNodes), len(pacemakerNodes))
		return nil
	}

	klog.Infof("Detected drift between K8s (%d nodes) and pacemaker (%d nodes) - checking if reconciliation needed",
		len(k8sNodes), len(pacemakerNodes))

	// Serialize reconciliation trigger to prevent time-of-check-time-of-use race between drift detection
	// and update-setup start. Multiple concurrent callers can detect drift, but only one should check-and-trigger at a time.
	reconcilePacemakerConfigMutex.Lock()
	defer reconcilePacemakerConfigMutex.Unlock()

	// Check if update-setup is already running
	isRunning, err := c.isUpdateSetupRunning(ctx)
	if err != nil {
		return fmt.Errorf("failed to check if update-setup is running: %w", err)
	}

	if isRunning {
		klog.V(2).Infof("Update-setup already running, skipping reconciliation trigger")
		return nil
	}

	// Trigger update-setup reconciliation
	klog.Infof("Triggering update-setup reconciliation to sync pacemaker with K8s state")

	// Calculate intersection: nodes that exist in BOTH K8s and pacemaker
	intersection := c.getIntersection(k8sNodes, pacemakerNodes)
	if len(intersection) == 0 {
		return fmt.Errorf("no nodes in both K8s and pacemaker - manual intervention may be required")
	}

	klog.Infof("Found %d nodes in intersection (K8s ∩ pacemaker): %v", len(intersection), getNodeNames(intersection))

	// Call update-setup with intersection nodes (it will pick target and write ConfigMap)
	// Use updateSetupFunc to allow mocking in tests
	return updateSetupFunc(
		intersection,
		k8sNodes,
		ctx,
		c.controllerContext,
		c.operatorClient,
		c.kubeClient,
		c.kubeInformersForNamespaces,
	)
}

// detectDrift compares K8s nodes with pacemaker nodes and returns true if drift exists.
// Checks both node names and IPs (supports IPv4 and IPv6).
func (c *PacemakerLifecycleManager) detectDrift(k8sNodes []*corev1.Node, pacemakerNodes map[string]string) bool {
	// Check count mismatch
	if len(k8sNodes) != len(pacemakerNodes) {
		klog.V(2).Infof("Drift detected: node count mismatch (K8s: %d, Pacemaker: %d)",
			len(k8sNodes), len(pacemakerNodes))
		return true
	}

	// Check each K8s node exists in pacemaker with matching IP
	for _, k8sNode := range k8sNodes {
		k8sIP, err := tools.GetNodeIPForPacemaker(*k8sNode)
		if err != nil {
			klog.Warningf("Failed to get IP for K8s node %s: %v", k8sNode.Name, err)
			continue
		}

		pmIP, exists := pacemakerNodes[k8sNode.Name]
		if !exists {
			klog.V(2).Infof("Drift detected: node %s exists in K8s but not in pacemaker", k8sNode.Name)
			return true
		}

		if !ceohelpers.IPAddressesEqual(k8sIP, pmIP) {
			klog.V(2).Infof("Drift detected: node %s IP mismatch (K8s: %s, Pacemaker: %s)",
				k8sNode.Name, k8sIP, pmIP)
			return true
		}
	}

	// Check for nodes in pacemaker but not in K8s
	for pmNodeName := range pacemakerNodes {
		found := false
		for _, k8sNode := range k8sNodes {
			if k8sNode.Name == pmNodeName {
				found = true
				break
			}
		}
		if !found {
			klog.V(2).Infof("Drift detected: node %s exists in pacemaker but not in K8s", pmNodeName)
			return true
		}
	}

	return false
}

// getIntersection returns nodes that exist in BOTH K8s and pacemaker.
// Used to determine valid target nodes for update-setup operations.
func (c *PacemakerLifecycleManager) getIntersection(k8sNodes []*corev1.Node, pacemakerNodes map[string]string) []*corev1.Node {
	intersection := []*corev1.Node{}
	for _, k8sNode := range k8sNodes {
		if _, exists := pacemakerNodes[k8sNode.Name]; exists {
			intersection = append(intersection, k8sNode)
		}
	}
	return intersection
}

// isUpdateSetupRunning checks if any update-setup job is currently running.
func (c *PacemakerLifecycleManager) isUpdateSetupRunning(ctx context.Context) (bool, error) {
	// List all update-setup jobs by name label
	jobList, err := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, v1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=tnf-update-setup-job",
	})
	if err != nil {
		return false, fmt.Errorf("failed to list update-setup jobs: %w", err)
	}

	// Check if any job is still running (not Complete and not Failed)
	for _, job := range jobList.Items {
		if !isJobStopped(job) {
			klog.V(4).Infof("Update-setup job %s is still running", job.Name)
			return true, nil
		}
	}

	return false, nil
}

// getNextUpdateSetupGeneration increments and returns the next generation counter.
// Caller must hold reconcilePacemakerConfigMutex.
func getNextUpdateSetupGeneration() int64 {
	updateSetupGeneration++
	return updateSetupGeneration
}

// updateSetup writes a snapshot ConfigMap, runs auth on all nodes, update-setup on one target, then after-setup on all.
// validTargetNodes: nodes that can run the update-setup job (intersection of K8s and pacemaker)
// allK8sNodes: all K8s nodes for the ConfigMap snapshot
func updateSetup(
	validTargetNodes []*corev1.Node,
	allK8sNodes []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) error {

	generation := getNextUpdateSetupGeneration()

	// Pick target node from valid nodes (first in list)
	if len(validTargetNodes) == 0 {
		return fmt.Errorf("no valid target nodes for update-setup - manual intervention may be required")
	}
	targetNode := validTargetNodes[0]

	klog.Infof("Generation %d: Target=%s, ValidTargets=%v, AllNodes=%v",
		generation, targetNode.Name, getNodeNames(validTargetNodes), getNodeNames(allK8sNodes))

	// Get current pacemaker membership to determine reconciliation actions
	pacemakerNodes, err := getPacemakerMembership(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pacemaker membership: %w", err)
	}

	// Build K8s node map (name -> IP) for reconciliation
	k8sNodeMap := buildK8sNodeMap(allK8sNodes)

	// Determine what changes are needed (this is the single source of truth for drift decisions)
	nodesToRemove, nodesToAdd := determineReconciliationActions(k8sNodeMap, pacemakerNodes)

	klog.Infof("Generation %d: Reconciliation decisions - nodesToAdd=%v, nodesToRemove=%v",
		generation, nodesToAdd, nodesToRemove)

	// Snapshot all K8s nodes into ConfigMap for the runner
	nodeListData, err := encodeNodeList(allK8sNodes)
	if err != nil {
		return fmt.Errorf("failed to encode node list: %w", err)
	}

	cmName := fmt.Sprintf("tnf-update-setup-%d", generation)
	cmData := map[string]string{
		"nodes":         nodeListData,
		"generation":    fmt.Sprintf("%d", generation),
		"timestamp":     time.Now().Format(time.RFC3339),
		"nodesToAdd":    encodeStringList(nodesToAdd),
		"nodesToRemove": encodeStringList(nodesToRemove),
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Name:      cmName,
			Namespace: operatorclient.TargetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/component":      tools.TnfUpdateSetupComponentValue,
				"tnf.etcd.openshift.io/generation": fmt.Sprintf("%d", generation),
			},
		},
		Data: cmData,
	}

	_, err = kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Create(ctx, cm, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create ConfigMap %s: %w", cmName, err)
	}

	// Clean up ConfigMap after all jobs complete
	defer func() {
		if err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Delete(ctx, cmName, v1.DeleteOptions{}); err != nil {
			klog.Warningf("failed to delete ConfigMap %s: %v", cmName, err)
		}
	}()

	// Run auth jobs on all nodes to ensure pacemaker authentication is current
	if err := runJobsOnNodes(ctx, tools.JobTypeAuth, allK8sNodes, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces); err != nil {
		return err
	}

	// Run update-setup job on target node
	// This is a cluster-wide operation (not tied to node lifecycle), but scheduled on a pacemaker-active node
	timeout := getJobTimeout(tools.JobTypeUpdateSetup)
	if err := jobs.RestartJobOrRunController(ctx, tools.JobTypeUpdateSetup, nil, &targetNode.Name,
		controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces,
		jobs.DefaultConditions, timeout); err != nil {
		return fmt.Errorf("failed to start update-setup job: %w", err)
	}
	if err := jobs.WaitForCompletion(ctx, kubeClient, tools.JobTypeUpdateSetup.GetJobName(nil),
		operatorclient.TargetNamespace, timeout); err != nil {
		return fmt.Errorf("failed to wait for update-setup job: %w", err)
	}

	// Run after-setup jobs on all nodes for post-reconciliation tasks
	if err := runJobsOnNodes(ctx, tools.JobTypeAfterSetup, allK8sNodes, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces); err != nil {
		return err
	}

	return nil
}

// runJobsOnNodes runs a node-specific job type on the given nodes and waits for completion.
// This is used for auth and after-setup jobs that are tied to individual node lifecycles.
func runJobsOnNodes(
	ctx context.Context,
	jobType tools.JobType,
	nodes []*corev1.Node,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) error {
	timeout := getJobTimeout(jobType)

	for _, node := range nodes {
		nodeTarget := &jobs.NodeTarget{Name: node.Name, UID: string(node.UID)}
		if err := jobs.RestartJobOrRunController(ctx, jobType, nodeTarget, nil,
			controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces,
			jobs.DefaultConditions, timeout); err != nil {
			return fmt.Errorf("failed to start %s job on node %s: %w", jobType.GetSubCommand(), node.Name, err)
		}
	}

	for _, node := range nodes {
		if err := jobs.WaitForCompletion(ctx, kubeClient, jobType.GetJobName(&node.Name),
			operatorclient.TargetNamespace, timeout); err != nil {
			return fmt.Errorf("failed to wait for %s job on node %s: %w", jobType.GetSubCommand(), node.Name, err)
		}
	}

	return nil
}

// getPacemakerMembership returns current pacemaker membership as a map of node name to IP.
// Calls 'pcs cluster config show' on the local node via exec.Execute.
func getPacemakerMembership(ctx context.Context) (map[string]string, error) {
	command := "/usr/sbin/pcs cluster config show --output-format json"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("failed to get pacemaker cluster config: stdout=%s, stderr=%s, err=%w", stdOut, stdErr, err)
	}

	var pacemakerConfig pacemaker.ClusterConfig
	if err := json.Unmarshal([]byte(stdOut), &pacemakerConfig); err != nil {
		return nil, fmt.Errorf("failed to parse pacemaker cluster config JSON: %w", err)
	}

	// Build map of node name -> IP
	pacemakerNodes := make(map[string]string)
	for _, node := range pacemakerConfig.Nodes {
		if len(node.Addrs) == 0 {
			return nil, fmt.Errorf("node %q has no addresses in pacemaker config", node.Name)
		}
		// Use first address (ring0) - matches initial setup
		pacemakerNodes[node.Name] = node.Addrs[0].Addr
	}

	return pacemakerNodes, nil
}

// buildK8sNodeMap builds a map of node name to IP from K8s nodes.
func buildK8sNodeMap(nodes []*corev1.Node) map[string]string {
	m := make(map[string]string)
	for _, node := range nodes {
		ip, err := tools.GetNodeIPForPacemaker(*node)
		if err != nil {
			klog.Warningf("failed to get IP for node %s: %v - skipping", node.Name, err)
			continue
		}
		m[node.Name] = ip
	}
	return m
}

// determineReconciliationActions compares K8s and pacemaker membership to determine
// which nodes to add/remove. K8s is the source of truth.
// Compares both name AND IP to detect replacements (same name, different IP).
func determineReconciliationActions(k8sNodes, pacemakerNodes map[string]string) (nodesToRemove, nodesToAdd []string) {
	// Remove: pacemaker nodes not in k8s OR with mismatched IPs
	for nodeName, pacemakerIP := range pacemakerNodes {
		k8sIP, existsInK8s := k8sNodes[nodeName]
		if !existsInK8s {
			// Node deleted from k8s
			nodesToRemove = append(nodesToRemove, nodeName)
		} else if !ceohelpers.IPAddressesEqual(k8sIP, pacemakerIP) {
			// Node exists in both but IP changed - remove old, add new
			nodesToRemove = append(nodesToRemove, nodeName)
			nodesToAdd = append(nodesToAdd, nodeName)
		}
	}

	// Add: k8s nodes not in pacemaker at all
	for nodeName := range k8sNodes {
		if _, exists := pacemakerNodes[nodeName]; !exists {
			nodesToAdd = append(nodesToAdd, nodeName)
		}
	}

	return nodesToRemove, nodesToAdd
}

// getJobTimeout returns the appropriate timeout for a given job type.
func getJobTimeout(jobType tools.JobType) time.Duration {
	switch jobType {
	case tools.JobTypeAuth:
		return tools.AuthJobCompletedTimeout
	case tools.JobTypeUpdateSetup:
		return tools.SetupJobCompletedTimeout
	case tools.JobTypeAfterSetup:
		return tools.AfterSetupJobCompletedTimeout
	default:
		return tools.AllCompletedTimeout
	}
}

// encodeNodeList encodes a list of nodes to JSON.
func encodeNodeList(nodes []*corev1.Node) (string, error) {
	type nodeInfo struct {
		Name              string               `json:"name"`
		CreationTimestamp v1.Time              `json:"creationTimestamp"`
		Labels            map[string]string    `json:"labels"`
		Addresses         []corev1.NodeAddress `json:"addresses"`
	}

	infos := make([]nodeInfo, len(nodes))
	for i, node := range nodes {
		infos[i] = nodeInfo{
			Name:              node.Name,
			CreationTimestamp: node.CreationTimestamp,
			Labels:            node.Labels,
			Addresses:         node.Status.Addresses,
		}
	}

	data, err := json.Marshal(infos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// encodeStringList encodes a list of strings to JSON.
func encodeStringList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	data, err := json.Marshal(list)
	if err != nil {
		// Should never happen for []string
		klog.Errorf("failed to encode string list: %v", err)
		return "[]"
	}
	return string(data)
}
