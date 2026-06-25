package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// pacemakerCRStalenessThreshold is how long before a PacemakerCluster CR status is considered stale.
	// Status collector runs every minute, so 5 minutes means we've missed ~5 consecutive updates.
	pacemakerCRStalenessThreshold = 5 * time.Minute

	// maxUpdateSetupConfigMaps is the maximum number of update-setup ConfigMaps to keep for debugging.
	// Older ConfigMaps are cleaned up to prevent unbounded growth.
	maxUpdateSetupConfigMaps = 5
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
// Also handles orphaned jobs by starting JobController to reconcile operator conditions.
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

	// Check transition status to determine readiness requirements
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, c.operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check external etcd transition status: %w", err)
	}

	// Before transition: require all nodes Ready (stable bootstrap environment)
	// After transition: allow NotReady nodes (this is exactly when we need drift reconciliation for node replacement)
	if !transitionComplete {
		for _, node := range k8sNodes {
			if !tools.IsNodeReady(node) {
				klog.V(4).Infof("Node %s is not Ready - skipping drift reconciliation during bootstrap (will retry in next sync)", node.Name)
				return nil
			}
		}
	}

	// Get pacemaker nodes from PacemakerCluster CR
	pacemakerNodes, pacemakerCR, err := c.getPacemakerNodesWithCR()
	pacemakerNodesAvailable := (err == nil)
	if !pacemakerNodesAvailable {
		klog.V(4).Infof("PacemakerCluster CR not available: %v - will attempt discovery reconciliation", err)
	} else {
		// Check if CR status is stale (not updated recently)
		if isPacemakerCRStale(pacemakerCR) {
			klog.Warningf("PacemakerCluster CR status is stale (last updated %v) - treating as unavailable for discovery reconciliation", pacemakerCR.Status.LastUpdated)
			pacemakerNodesAvailable = false
			pacemakerNodes = nil
		}
	}

	// Check for stopped update-setup job from previous run (handles perma-degraded/perma-progressing)
	// This needs to run on every sync to catch jobs that stopped while operator was down
	stoppedJob, err := c.getStoppedUpdateSetupJob(ctx)
	if err != nil {
		klog.Warningf("Failed to check for stopped update-setup job: %v", err)
		// Don't return error - continue with drift detection
	}

	// Detect drift (compares node names and IPs)
	var hasDrift bool
	if pacemakerNodesAvailable {
		hasDrift = c.detectDrift(k8sNodes, pacemakerNodes)
	} else {
		// CR doesn't exist - treat as drift (unknown pacemaker state = needs reconciliation)
		hasDrift = true
		klog.Infof("PacemakerCluster CR not available - treating as drift for discovery reconciliation")
	}

	// Determine if we need to take action
	needsReconciliation := false
	var reason string

	// Reason 1: Stopped unsuccessful job exists
	if stoppedJob != nil && !jobs.IsComplete(*stoppedJob) {
		needsReconciliation = true
		reason = fmt.Sprintf("stopped unsuccessful job %s exists", stoppedJob.Name)
	}

	// Reason 2: Drift detected
	if hasDrift {
		needsReconciliation = true
		if reason == "" {
			// Format node names for better readability
			k8sNodeNames := getNodeNames(k8sNodes)
			pmNodeNames := make([]string, 0, len(pacemakerNodes))
			for nodeName := range pacemakerNodes {
				pmNodeNames = append(pmNodeNames, nodeName)
			}
			reason = fmt.Sprintf("drift detected between K8s %v and pacemaker %v", k8sNodeNames, pmNodeNames)
		} else {
			reason = fmt.Sprintf("%s AND drift detected", reason)
		}
	}

	// If no action needed, return early
	if !needsReconciliation {
		klog.V(4).Infof("No reconciliation needed - no drift and no stopped jobs")
		return nil
	}

	klog.Infof("Reconciliation needed: %s", reason)

	// Serialize reconciliation trigger to prevent time-of-check-time-of-use race between drift detection
	// and update-setup start. Multiple concurrent callers can detect drift, but only one should check-and-trigger at a time.
	reconcilePacemakerConfigMutex.Lock()
	defer reconcilePacemakerConfigMutex.Unlock()

	// Re-fetch CR state after acquiring lock to use current state (CR may have been updated by status collector)
	pacemakerNodes, pacemakerCR, err = c.getPacemakerNodesWithCR()
	pacemakerNodesAvailable = (err == nil)
	if !pacemakerNodesAvailable {
		klog.V(4).Infof("PacemakerCluster CR not available after lock: %v - will attempt discovery reconciliation", err)
	} else {
		if isPacemakerCRStale(pacemakerCR) {
			klog.Warningf("PacemakerCluster CR status is stale after lock (last updated %v) - treating as unavailable for discovery reconciliation", pacemakerCR.Status.LastUpdated)
			pacemakerNodesAvailable = false
			pacemakerNodes = nil
		}
	}

	// Re-check drift and stopped job after acquiring lock (another goroutine may have changed state)
	stoppedJob, err = c.getStoppedUpdateSetupJob(ctx)
	if err != nil {
		klog.Warningf("Failed to re-check for stopped update-setup job: %v", err)
	}
	hasDrift = c.detectDrift(k8sNodes, pacemakerNodes)

	// Decision tree:
	// 1. If drift exists → ensure ConfigMap exists and matches desired state, start controller if needed
	// 2. If stopped unsuccessful job but NO drift → delete job (cluster is correct, stale failure)
	// 3. Otherwise → nothing to do

	if hasDrift {
		// Continue to ConfigMap creation and controller start below...
	} else if stoppedJob != nil && !jobs.IsComplete(*stoppedJob) {
		// No drift, but stopped unsuccessful job exists - cluster is correct, just delete the stale job
		klog.Infof("No drift detected, but stopped job %s exists - deleting stale job to clear conditions", stoppedJob.Name)
		if err := jobs.DeleteAndWait(ctx, c.kubeClient, stoppedJob.Name, operatorclient.TargetNamespace); err != nil {
			return fmt.Errorf("failed to delete stopped job: %w", err)
		}
		klog.Infof("Successfully deleted stopped job %s - JobController will clear conditions", stoppedJob.Name)
		return nil
	} else {
		klog.V(4).Infof("No action needed after acquiring lock - another goroutine may have handled it")
		return nil
	}

	// Calculate valid target nodes for update-setup job
	var validTargetNodes []*corev1.Node
	if pacemakerNodesAvailable {
		// CR exists - use intersection: nodes that exist in BOTH K8s and pacemaker
		intersection := c.getIntersection(k8sNodes, pacemakerNodes)
		if len(intersection) == 0 {
			return fmt.Errorf("no nodes in both K8s and pacemaker - manual intervention may be required")
		}
		validTargetNodes = intersection
		klog.Infof("Found %d nodes in intersection (K8s ∩ pacemaker): %v", len(validTargetNodes), getNodeNames(validTargetNodes))
	} else {
		// CR unavailable/stale - try nodes sequentially to discover which has pacemaker running
		// Use stopped job to determine which node we already tried (to avoid retrying same node)
		validTargetNodes = c.selectDiscoveryTargetNodes(k8sNodes, stoppedJob)
		if len(validTargetNodes) == 0 {
			return fmt.Errorf("no valid nodes for discovery - all nodes have been tried and failed")
		}
		klog.Infof("Discovery mode (no actionable CR): will try nodes %v", getNodeNames(validTargetNodes))
	}

	// Create function that calculates valid target nodes
	// Job controller will call this before each attempt (to get fresh node state)
	validNodeFunc := func() ([]*corev1.Node, error) {
		return c.getActivePacemakerNodes()
	}

	// Call update-setup with valid target nodes (creates/updates ConfigMap and ensures controller is running)
	// Use updateSetupFunc to allow mocking in tests
	return updateSetupFunc(
		validTargetNodes,
		validNodeFunc,
		k8sNodes,
		pacemakerNodes,
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
			return true
		}

		if !ceohelpers.IPAddressesEqual(k8sIP, pmIP) {
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
			return true
		}
	}

	return false
}

// getActivePacemakerNodes returns the active pacemaker nodes for targeting jobs and CronJobs.
// Uses intersection logic: nodes that exist in BOTH K8s (ready) and pacemaker.
// If PacemakerCluster CR doesn't exist or is stale, returns all ready control plane nodes.
// This is the shared logic used by both update-setup job and status collector CronJob.
func (c *PacemakerLifecycleManager) getActivePacemakerNodes() ([]*corev1.Node, error) {
	// Check if node informer has synced
	if c.nodeInformer == nil || !c.nodeInformer.HasSynced() {
		return nil, fmt.Errorf("node informer not synced yet")
	}

	// Get K8s control plane nodes
	k8sNodes, err := ceohelpers.ListNodesFromInformer(c.nodeInformer)
	if err != nil {
		return nil, fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	// Filter to Ready nodes only
	readyNodes := []*corev1.Node{}
	for _, node := range k8sNodes {
		if tools.IsNodeReady(node) {
			readyNodes = append(readyNodes, node)
		}
	}

	if len(readyNodes) == 0 {
		return nil, fmt.Errorf("no ready control plane nodes found")
	}

	// Try to get pacemaker nodes from CR
	pacemakerNodes, pacemakerCR, err := c.getPacemakerNodesWithCR()
	pacemakerNodesAvailable := (err == nil && !isPacemakerCRStale(pacemakerCR))

	if pacemakerNodesAvailable {
		// CR exists and is fresh - use intersection logic (K8s ∩ pacemaker)
		intersection := c.getIntersection(readyNodes, pacemakerNodes)
		if len(intersection) > 0 {
			klog.V(2).Infof("Valid targets (intersection, ready only): %v", getNodeNames(intersection))
			return intersection, nil
		}
		// No intersection - fall through to using all ready nodes
		klog.Warningf("No nodes in intersection (K8s ∩ pacemaker) - using all ready nodes")
	} else {
		if err != nil {
			klog.V(4).Infof("PacemakerCluster CR not available: %v - using all ready nodes", err)
		} else {
			klog.Warningf("PacemakerCluster CR status is stale (last updated %v) - using all ready nodes", pacemakerCR.Status.LastUpdated)
		}
	}

	// CR doesn't exist, is stale, or no intersection - use all ready nodes
	klog.V(2).Infof("Valid targets (all ready nodes): %v", getNodeNames(readyNodes))
	return readyNodes, nil
}

// getIntersection returns nodes that exist in BOTH K8s and pacemaker.
// Used to determine valid target nodes for update-setup operations.
// Returns nodes sorted by name for deterministic ordering.
func (c *PacemakerLifecycleManager) getIntersection(k8sNodes []*corev1.Node, pacemakerNodes map[string]string) []*corev1.Node {
	intersection := []*corev1.Node{}
	for _, k8sNode := range k8sNodes {
		if _, exists := pacemakerNodes[k8sNode.Name]; exists {
			intersection = append(intersection, k8sNode)
		}
	}
	// Sort by name for deterministic target selection
	sort.Slice(intersection, func(i, j int) bool {
		return intersection[i].Name < intersection[j].Name
	})
	return intersection
}

// isPacemakerCRStale checks if the PacemakerCluster CR status is stale (hasn't been updated recently).
// A stale CR indicates the status collector isn't running or pacemaker isn't responding.
func isPacemakerCRStale(cr *pacmkrv1.PacemakerCluster) bool {
	if cr == nil {
		return true
	}

	timeSinceUpdate := time.Since(cr.Status.LastUpdated.Time)
	return timeSinceUpdate > pacemakerCRStalenessThreshold
}

// selectDiscoveryTargetNodes selects which nodes to try when PacemakerCluster CR is unavailable or stale.
// Used during initial cluster setup OR when status collector isn't updating the CR (pacemaker issue).
// Strategy: Try nodes sequentially, excluding nodes where previous attempts failed.
// - If no stopped job exists, return all nodes sorted by name
// - If a stopped unsuccessful job exists, exclude that node and return remaining sorted nodes
// - Returns empty list if all nodes have been tried and failed
func (c *PacemakerLifecycleManager) selectDiscoveryTargetNodes(k8sNodes []*corev1.Node, stoppedJob *batchv1.Job) []*corev1.Node {
	if len(k8sNodes) == 0 {
		return []*corev1.Node{}
	}

	// Sort nodes by name for deterministic ordering
	sortedNodes := make([]*corev1.Node, len(k8sNodes))
	copy(sortedNodes, k8sNodes)
	sort.Slice(sortedNodes, func(i, j int) bool {
		return sortedNodes[i].Name < sortedNodes[j].Name
	})

	// If no stopped job, return all nodes
	if stoppedJob == nil {
		klog.V(4).Infof("No previous job attempt - will try all nodes starting with: %s", sortedNodes[0].Name)
		return sortedNodes
	}

	// If stopped job succeeded, we're done (shouldn't reach here, but handle gracefully)
	if jobs.IsComplete(*stoppedJob) {
		klog.V(4).Infof("Previous job succeeded - will try all nodes starting with: %s", sortedNodes[0].Name)
		return sortedNodes
	}

	// Stopped job failed - find which node it ran on and exclude it
	failedNodeName := stoppedJob.Spec.Template.Spec.NodeName
	if failedNodeName == "" {
		klog.Warningf("Stopped job %s has no NodeName - cannot determine which node failed, will try all nodes", stoppedJob.Name)
		return sortedNodes
	}

	klog.Infof("Previous job failed on node %s - excluding from valid targets", failedNodeName)

	// Build list of nodes excluding the failed one
	remainingNodes := []*corev1.Node{}
	for _, node := range sortedNodes {
		if node.Name != failedNodeName {
			remainingNodes = append(remainingNodes, node)
		}
	}

	if len(remainingNodes) == 0 {
		klog.Warningf("All %d nodes have been tried - no remaining nodes to attempt", len(k8sNodes))
		return []*corev1.Node{}
	}

	klog.Infof("Will try %d remaining node(s) starting with: %s", len(remainingNodes), remainingNodes[0].Name)
	return remainingNodes
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
		if !jobs.IsStopped(job) {
			klog.V(4).Infof("Update-setup job %s is still running", job.Name)
			return true, nil
		}
	}

	return false, nil
}

// getStoppedUpdateSetupJob returns the stopped update-setup job if one exists, nil otherwise.
// A stopped job is one that has completed (successfully or failed).
func (c *PacemakerLifecycleManager) getStoppedUpdateSetupJob(ctx context.Context) (*batchv1.Job, error) {
	jobName := tools.JobTypeUpdateSetup.GetJobName(nil)
	job, err := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, v1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get update-setup job: %w", err)
	}

	if jobs.IsStopped(*job) {
		return job, nil
	}

	return nil, nil
}

// getNextUpdateSetupGeneration increments and returns the next generation counter.
// Caller must hold reconcilePacemakerConfigMutex.
func getNextUpdateSetupGeneration() int64 {
	updateSetupGeneration++
	return updateSetupGeneration
}

func getCurrentUpdateSetupGeneration() int64 {
	return updateSetupGeneration
}

// initUpdateSetupGeneration scans existing update-setup ConfigMaps and initializes the generation
// counter to max(existing)+1 to prevent reusing stale ConfigMaps after operator restart.
// Must be called once during lifecycle manager initialization before any reconciliation loops run.
func (c *PacemakerLifecycleManager) initUpdateSetupGeneration(ctx context.Context) error {
	cmList, err := c.kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(ctx, v1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=" + tools.TnfUpdateSetupComponentValue,
	})
	if err != nil {
		return fmt.Errorf("failed to list existing update-setup ConfigMaps: %w", err)
	}

	var maxGen int64 = 0
	for _, cm := range cmList.Items {
		genStr := cm.Data["generation"]
		if genStr == "" {
			continue
		}
		gen, err := strconv.ParseInt(genStr, 10, 64)
		if err != nil {
			klog.Warningf("Found update-setup ConfigMap %s with invalid generation %q: %v", cm.Name, genStr, err)
			continue
		}
		if gen > maxGen {
			maxGen = gen
		}
	}

	updateSetupGeneration = maxGen
	if maxGen > 0 {
		klog.Infof("Initialized update-setup generation counter to %d (found %d existing ConfigMaps)", maxGen, len(cmList.Items))
	} else {
		klog.V(2).Infof("No existing update-setup ConfigMaps found, starting generation counter at 0")
	}
	return nil
}

// updateSetup writes a snapshot ConfigMap, runs auth on all nodes, tries update-setup on valid targets, then after-setup on all.
// validTargetNodes: nodes that can run the update-setup job (initial list for logging)
// validNodeFunc: function that calculates fresh list of valid nodes on each retry attempt
// allK8sNodes: all K8s nodes for the ConfigMap snapshot
// pacemakerNodes: current pacemaker membership (name -> IP) from PacemakerCluster CR
func updateSetup(
	validTargetNodes []*corev1.Node,
	validNodeFunc jobs.TargetNodesFunc,
	allK8sNodes []*corev1.Node,
	pacemakerNodes map[string]string,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) error {

	if len(validTargetNodes) == 0 {
		return fmt.Errorf("no valid target nodes for update-setup - manual intervention may be required")
	}

	// Encode desired state for comparison
	nodeListData, err := encodeNodeList(allK8sNodes)
	if err != nil {
		return fmt.Errorf("failed to encode node list: %w", err)
	}

	// Check if current generation matches desired state - if so, reuse it instead of creating new one
	currentGeneration := getCurrentUpdateSetupGeneration()
	var generation int64
	var shouldCreateNewConfigMap bool

	if currentGeneration > 0 {
		currentCMName := fmt.Sprintf("tnf-update-setup-%d", currentGeneration)
		currentCM, err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(ctx, currentCMName, v1.GetOptions{})
		if err == nil {
			// Compare desired state with current ConfigMap (only check "nodes" - the desired end state)
			if currentCM.Data["nodes"] == nodeListData {
				klog.Infof("Desired state matches current generation %d - reusing existing ConfigMap", currentGeneration)
				generation = currentGeneration
				shouldCreateNewConfigMap = false
			} else {
				klog.Infof("Desired state differs from generation %d - creating new generation", currentGeneration)
				generation = getNextUpdateSetupGeneration()
				shouldCreateNewConfigMap = true
			}
		} else {
			// Current ConfigMap doesn't exist (deleted or error) - create new
			klog.V(2).Infof("Current generation %d ConfigMap not found: %v - creating new generation", currentGeneration, err)
			generation = getNextUpdateSetupGeneration()
			shouldCreateNewConfigMap = true
		}
	} else {
		// First generation
		generation = getNextUpdateSetupGeneration()
		shouldCreateNewConfigMap = true
	}

	klog.Infof("Generation %d: Will try nodes %v (desired state: %v)",
		generation, getNodeNames(validTargetNodes), getNodeNames(allK8sNodes))

	cmName := fmt.Sprintf("tnf-update-setup-%d", generation)

	// Only create ConfigMap if desired state differs from current generation
	if shouldCreateNewConfigMap {
		cmData := map[string]string{
			"nodes":      nodeListData, // Desired state: which K8s nodes should be in pacemaker
			"generation": fmt.Sprintf("%d", generation),
			"timestamp":  time.Now().Format(time.RFC3339),
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
		klog.Infof("Created new ConfigMap %s for generation %d", cmName, generation)

		// Clean up old ConfigMaps (keep 5 most recent)
		if err := cleanupOldUpdateSetupConfigMaps(ctx, kubeClient, generation); err != nil {
			klog.Warningf("Failed to cleanup old update-setup ConfigMaps: %v", err)
		}
	}

	// When reusing ConfigMap (desired state unchanged), check if job already exists and is working.
	// Skip restarting if job is still running OR completed successfully - pacemaker just needs time to converge.
	// This prevents redundant job creation when:
	// 1. First job completes and updates pacemaker
	// 2. Status collector updates CR with partial pacemaker state
	// 3. Reconciliation triggered again, detects drift (pacemaker not fully converged yet)
	// 4. Desired state matches current generation → would restart job unnecessarily
	if !shouldCreateNewConfigMap {
		jobName := tools.JobTypeUpdateSetup.GetJobName(nil)
		existingJob, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, v1.GetOptions{})
		if err == nil {
			// Job exists - check state
			if !jobs.IsStopped(*existingJob) {
				// Job still running - skip restart to avoid interrupting in-progress work
				klog.Infof("Reusing generation %d - job still running, skipping restart (waiting for job completion)", generation)
				return nil
			} else if jobs.IsComplete(*existingJob) {
				// Job completed successfully - skip restart, pacemaker convergence in progress
				klog.Infof("Reusing generation %d - job already completed successfully, skipping restart (waiting for pacemaker convergence)", generation)
				return nil
			}
			// Job stopped but not complete (failed) - fall through to restart
			klog.Infof("Reusing generation %d - existing job failed, restarting", generation)
		} else if !apierrors.IsNotFound(err) {
			// Error checking job (not NotFound) - log warning and fall through to restart
			klog.Warningf("Failed to check existing job for generation %d: %v - will restart", generation, err)
		} else {
			// Job doesn't exist - fall through to start
			klog.Infof("Reusing generation %d - no existing job found, starting", generation)
		}
	}

	// Restart update-setup job with new information (deletes existing job immediately, starts controller)
	// Controller will manage job lifecycle via sync loop and retry across valid nodes
	// Note: Auth and after-setup jobs are managed by separate background controllers (lifecycle_job_controllers.go)
	klog.Infof("Starting update-setup job controller for generation %d with %d initial valid target(s): %v", generation, len(validTargetNodes), getNodeNames(validTargetNodes))

	// Create jobConfigFunc that captures config for drift detection
	jobConfigFunc := func() (string, error) {
		nodes, err := validNodeFunc()
		if err != nil {
			return "", err
		}
		nodeUIDs := make([]string, len(nodes))
		for i, node := range nodes {
			nodeUIDs[i] = string(node.UID)
		}
		sort.Strings(nodeUIDs) // Sort for stable comparison

		config := map[string]interface{}{
			"nodeUIDs":            nodeUIDs,
			"configMapGeneration": generation,
		}
		configJSON, err := json.Marshal(config)
		if err != nil {
			return "", fmt.Errorf("failed to marshal job config: %w", err)
		}
		return string(configJSON), nil
	}

	const retries = 3 // Multi-node: try all valid nodes 3 times before degrading (backoffLimit=0)
	timeout := getJobTimeout(tools.JobTypeUpdateSetup)
	if err := jobs.RestartJobOrRunController(ctx, tools.JobTypeUpdateSetup, nil, validNodeFunc, jobConfigFunc, retries,
		controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces,
		jobs.DefaultConditions, timeout); err != nil {
		return fmt.Errorf("failed to restart update-setup job controller: %w", err)
	}

	return nil
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
// Only includes name and IP to avoid unnecessary ConfigMap recreation when labels/addresses change.
func encodeNodeList(nodes []*corev1.Node) (string, error) {
	type nodeInfo struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
	}

	infos := make([]nodeInfo, 0, len(nodes))
	for _, node := range nodes {
		ip, err := tools.GetNodeIPForPacemaker(*node)
		if err != nil {
			klog.Warningf("Failed to get IP for node %s: %v - skipping from ConfigMap", node.Name, err)
			continue
		}
		infos = append(infos, nodeInfo{
			Name: node.Name,
			IP:   ip,
		})
	}

	// Sort by name to ensure stable ConfigMap comparison (avoid order-sensitive duplicates)
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Name < infos[j].Name
	})

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

// cleanupOldUpdateSetupConfigMaps deletes old update-setup ConfigMaps, keeping the 5 most recent for debugging.
// This prevents ConfigMap accumulation while preserving recent history for comparison and troubleshooting.
func cleanupOldUpdateSetupConfigMaps(ctx context.Context, kubeClient kubernetes.Interface, currentGeneration int64) error {
	cmList, err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(ctx, v1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=" + tools.TnfUpdateSetupComponentValue,
	})
	if err != nil {
		return fmt.Errorf("failed to list update-setup ConfigMaps: %w", err)
	}

	if len(cmList.Items) <= maxUpdateSetupConfigMaps {
		klog.V(4).Infof("Found %d update-setup ConfigMaps (limit: %d) - no cleanup needed", len(cmList.Items), maxUpdateSetupConfigMaps)
		return nil
	}

	// Parse generations and sort by generation number (descending)
	type cmWithGen struct {
		cm  corev1.ConfigMap
		gen int64
	}
	var cms []cmWithGen
	for _, cm := range cmList.Items {
		genStr := cm.Data["generation"]
		if genStr == "" {
			klog.Warningf("ConfigMap %s has no generation field, skipping cleanup", cm.Name)
			continue
		}
		gen, err := strconv.ParseInt(genStr, 10, 64)
		if err != nil {
			klog.Warningf("ConfigMap %s has invalid generation %q: %v, skipping cleanup", cm.Name, genStr, err)
			continue
		}
		cms = append(cms, cmWithGen{cm: cm, gen: gen})
	}

	// Sort by generation (newest first)
	sort.Slice(cms, func(i, j int) bool {
		return cms[i].gen > cms[j].gen
	})

	// Delete ConfigMaps beyond the limit
	deletedCount := 0
	for i := maxUpdateSetupConfigMaps; i < len(cms); i++ {
		cm := cms[i].cm
		klog.V(2).Infof("Deleting old update-setup ConfigMap %s (generation %d, keeping %d most recent)", cm.Name, cms[i].gen, maxUpdateSetupConfigMaps)
		if err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Delete(ctx, cm.Name, v1.DeleteOptions{}); err != nil {
			if !apierrors.IsNotFound(err) {
				klog.Warningf("Failed to delete old ConfigMap %s: %v", cm.Name, err)
			}
		} else {
			deletedCount++
		}
	}

	if deletedCount > 0 {
		klog.Infof("Cleaned up %d old update-setup ConfigMaps (keeping %d most recent)", deletedCount, maxUpdateSetupConfigMaps)
	} else {
		klog.V(4).Infof("No old update-setup ConfigMaps to clean up")
	}

	return nil
}
