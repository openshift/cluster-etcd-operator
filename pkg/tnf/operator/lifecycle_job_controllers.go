package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// Operator condition types
	conditionTypeTNFJobControllersDegraded = "TNFJobControllersDegraded"
)

var (
	// startTnfJobcontrollersFunc is a variable to allow mocking in tests
	startTnfJobcontrollersFunc = startTnfJobcontrollers

	// retryBackoffConfig allows customizing retry behavior for tests
	retryBackoffConfig = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Steps:    9, // ~10 minutes total: 5s + 10s + 20s + 40s + 80s + 120s + 120s + 120s + 120s
		Cap:      2 * time.Minute,
	}
)

// StartJobControllers starts TNF job controllers based on transition state.
//
// Before ExternalEtcdTransitionCompleted:
//   - Requires exactly 2 ready control plane nodes
//   - Uses exponential backoff retry (5s to 2min, ~10 min total)
//   - Sets TNFJobControllersDegraded condition on failure
//   - Mutex protects concurrent retry attempts
//
// After ExternalEtcdTransitionCompleted:
//   - Accepts any number of ready control plane nodes (handles single-node case)
//   - Idempotent (safe to call repeatedly, handles operator restarts)
//   - No retry logic (controller framework retries sync() on error)
//   - No mutex needed (job-level locking provides adequate protection)
//
// The job controllers are idempotent and will not create duplicate jobs.
func (c *PacemakerLifecycleManager) StartJobControllers(ctx context.Context) error {
	// Check if external etcd transition is complete
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, c.operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check external etcd transition status: %w", err)
	}

	// Check if node informer has synced
	if c.nodeInformer == nil || !c.nodeInformer.HasSynced() {
		klog.V(4).Infof("Skipping job controller startup - node informer not synced yet")
		return nil
	}

	// Get K8s control plane nodes
	k8sNodes, err := ceohelpers.ListNodesFromInformer(c.nodeInformer)
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	// Before transition: require exactly 2 ready control plane nodes for initial setup
	// After transition: require all control plane nodes ready (handles single-node case)
	if !transitionComplete {
		// Pacemaker only supports 2 control plane nodes - initial setup requires exactly 2
		if len(k8sNodes) > 2 {
			return fmt.Errorf("TNF requires exactly 2 control plane nodes for initial setup, found %d - pacemaker does not support >2 nodes", len(k8sNodes))
		}
		if len(k8sNodes) < 2 {
			klog.V(4).Infof("Waiting for 2 control plane nodes for initial setup (current: %d)", len(k8sNodes))
			return nil
		}

		// Check both control plane nodes are Ready
		for _, node := range k8sNodes {
			if !tools.IsNodeReady(node) {
				klog.V(4).Infof("Control plane node %s not Ready - waiting for both nodes before initial setup", node.Name)
				return nil
			}
		}

		// Initial transition: use retry logic with exponential backoff and specific degraded condition
		klog.V(2).Infof("Both control plane nodes ready - starting initial job controllers with retry")
		return c.retryInitialTransitionOrDegrade(ctx, k8sNodes)
	} else {
		// Post-transition: ensure controllers running for all ready nodes
		// Called on every sync to handle missed node add/delete/update events
		// RunTNFJobController has internal duplicate prevention

		// Defensive check: ensure we have at least one control plane node
		if len(k8sNodes) == 0 {
			klog.V(4).Infof("No control plane nodes found, skipping job controller startup")
			return nil
		}

		// Note: Node readiness is now checked at job admission level (not controller startup level)
		// Jobs will be blocked with TNF<JobName>Blocked condition if affected nodes are not ready
		// This allows controllers to start even when some nodes are degraded

		klog.V(4).Infof("Transition complete - ensuring job controllers running for %d control plane nodes", len(k8sNodes))
		err = c.startJobControllersWithLock(ctx, k8sNodes)
		if err != nil {
			return err
		}

		return nil
	}
}

// retryInitialTransitionOrDegrade starts TNF job controllers with exponential backoff retry.
// Sets TNFJobControllersDegraded condition on failure or success.
// This is only used for initial transition (before ExternalEtcdTransitionCompleted).
func (c *PacemakerLifecycleManager) retryInitialTransitionOrDegrade(ctx context.Context, nodes []*corev1.Node) error {
	// Retry with exponential backoff to handle transient failures
	var setupErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoffConfig, func(ctx context.Context) (bool, error) {
		setupErr = c.startJobControllersWithLock(ctx, nodes)
		if setupErr != nil {
			klog.Warningf("failed to setup TNF job controllers, will retry: %v", setupErr)
			return false, nil
		}
		return true, nil
	})

	if err != nil || setupErr != nil {
		// Prefer setupErr but fall back to err if setupErr is nil (e.g., context cancelled)
		displayErr := setupErr
		if displayErr == nil {
			displayErr = err
		}
		klog.Errorf("failed to setup TNF job controllers after retries: %v", displayErr)

		// Degrade the operator to indicate TNF job controller setup failed
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    conditionTypeTNFJobControllersDegraded,
			Status:  operatorv1.ConditionTrue,
			Reason:  "SetupFailed",
			Message: fmt.Sprintf("Failed to setup TNF job controllers after retries: %v", displayErr),
		}))
		if updateErr != nil {
			klog.Errorf("failed to update operator status to degraded: %v", updateErr)
		}
		return displayErr
	}

	// Clear any previous degraded condition on success
	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    conditionTypeTNFJobControllersDegraded,
		Status:  operatorv1.ConditionFalse,
		Reason:  "AsExpected",
		Message: "TNF job controllers setup completed successfully",
	}))
	if updateErr != nil {
		klog.Errorf("failed to update operator status: %v", updateErr)
	}

	return nil
}

// startJobControllersWithLock is a wrapper that acquires the mutex before calling startTnfJobcontrollersFunc.
// This ensures only one job controller startup runs at a time, preventing:
// - Concurrent waits on etcd bootstrap completion
// - Concurrent waits on stable revision
// - Duplicate job controller creation attempts
// - Race conditions in the wait logic
//
// Used by both bootstrap path (via retryInitialTransitionOrDegrade) and post-transition path.
func (c *PacemakerLifecycleManager) startJobControllersWithLock(ctx context.Context, nodes []*corev1.Node) error {
	c.startJobControllersMu.Lock()
	defer c.startJobControllersMu.Unlock()

	return startTnfJobcontrollersFunc(ctx, nodes, c.controllerContext, c.operatorClient, c.kubeClient, c.kubeInformersForNamespaces, c.etcdInformer, c)
}

// startTnfJobcontrollers creates TNF job controllers for the given nodes.
// It waits for etcd bootstrap completion, ensures all nodes are at latest revision,
// then creates auth, setup, fencing, and after-setup job controllers.
// Waits for after-setup jobs to complete to avoid races with update jobs.
func startTnfJobcontrollers(
	ctx context.Context,
	nodeList []*corev1.Node,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
	lifecycleManager *PacemakerLifecycleManager) error {

	// Check if transition already complete (operator restart scenario)
	// If so, skip bootstrap flow and just ensure controllers are running
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
	if err != nil {
		klog.Warningf("Failed to check transition status: %v - proceeding with bootstrap flow", err)
	}

	if transitionComplete {
		klog.Infof("Transition already complete - skipping bootstrap flow, ensuring controllers are running")

		// Just start the controllers without going through bootstrap/setup again
		// This prevents recreating setup job and racing with reconciliation
		for _, node := range nodeList {
			nodeTarget := &jobs.NodeTarget{Name: node.Name, UID: string(node.UID)}
			// Single-node jobs: schedulableNodesFunc=nil, affectedNodesFunc=nil (nodeTarget handles both)
			jobs.RunTNFJobController(ctx, tools.JobTypeAuth, nodeTarget, nil, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
			jobs.RunTNFJobController(ctx, tools.JobTypeAfterSetup, nodeTarget, nil, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		}

		// schedulableNodesFunc: nodes running pacemaker (eligible to run jobs)
		schedulableNodesFunc := func() ([]*corev1.Node, error) {
			return lifecycleManager.getActivePacemakerNodes()
		}

		// affectedNodesFunc for fencing: only nodes with fencing secrets (subset of nodeList)
		// Nodes without secrets won't block the job; when secret is added, event handler triggers restart
		fencingAffectedNodesFunc := func() ([]*corev1.Node, error) {
			return getNodesWithFencingSecrets(nodeList, kubeInformersForNamespaces)
		}

		// affectedNodesFunc for update-setup: nodes in latest update-setup ConfigMap snapshot
		// During replacement (A+B running, B→C): ConfigMap has [A,C], affected=[A,C] (not all control plane nodes)
		updateSetupAffectedNodesFunc := func() ([]*corev1.Node, error) {
			return getNodesFromLatestUpdateSetupConfigMap(lifecycleManager.nodeInformer, kubeInformersForNamespaces)
		}

		// DO NOT start setup job controller - transition already complete, setup job is one-time only
		// Day 2 changes are handled by update-setup job via ReconcilePacemakerConfig

		fencingJobConfigFunc := createFencingJobConfigFunc(nodeList, kubeInformersForNamespaces)
		jobs.RunTNFJobController(ctx, tools.JobTypeFencing, nil, schedulableNodesFunc, fencingAffectedNodesFunc, fencingJobConfigFunc, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

		// Check if there's a stopped update-setup job that needs a controller
		// (operator may have restarted while update-setup was running/stopped)
		updateSetupJobName := tools.JobTypeUpdateSetup.GetJobName(nil)
		updateSetupJob, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, updateSetupJobName, metav1.GetOptions{})
		if err == nil && jobs.IsStopped(*updateSetupJob) && !jobs.IsControllerRunning(updateSetupJobName) {
			klog.Infof("Found stopped update-setup job without controller - starting controller to update conditions")
			jobs.RunTNFJobController(ctx, tools.JobTypeUpdateSetup, nil, schedulableNodesFunc, updateSetupAffectedNodesFunc, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		}

		klog.V(4).Infof("Controllers running (post-transition)")
		return nil
	}

	klog.Infof("Running TNF setup procedure. Waiting for etcd bootstrap to complete")

	// Wait for the etcd informer to sync before checking bootstrap status
	// This ensures operatorClient.GetStaticPodOperatorState() has data to work with
	klog.Infof("waiting for etcd informer to sync...")
	if !cache.WaitForCacheSync(ctx.Done(), etcdInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync etcd informer")
	}
	klog.Infof("etcd informer synced")

	if err := waitForEtcdBootstrapCompleted(ctx, operatorClient); err != nil {
		return fmt.Errorf("failed to wait for etcd bootstrap: %w", err)
	}

	// Wait for all nodes to have their installers complete (creates /var/lib/etcd)
	klog.Infof("bootstrap completed, waiting for all nodes to reach latest revision")
	if err := etcd.WaitForStableRevision(ctx, operatorClient); err != nil {
		return fmt.Errorf("failed to wait for all nodes at latest revision: %w", err)
	}

	klog.Infof("all nodes at latest revision, creating TNF job controllers")

	// the order of job creation does not matter, the jobs wait on each other as needed
	for _, node := range nodeList {
		// Node-specific jobs: auth and after-setup are tied to individual nodes
		// Single-node: retries=3 sets backoffLimit (Kubernetes retries on same node)
		nodeTarget := &jobs.NodeTarget{Name: node.Name, UID: string(node.UID)}
		// Single-node jobs: schedulableNodesFunc=nil, affectedNodesFunc=nil (nodeTarget handles both)
		jobs.RunTNFJobController(ctx, tools.JobTypeAuth, nodeTarget, nil, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		jobs.RunTNFJobController(ctx, tools.JobTypeAfterSetup, nodeTarget, nil, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
	}

	// schedulableNodesFunc: nodes that can run jobs (pacemaker nodes during bootstrap = nodeList)
	schedulableNodesFunc := func() ([]*corev1.Node, error) {
		return lifecycleManager.getActivePacemakerNodes()
	}

	// affectedNodesFunc for setup during bootstrap: all nodes being set up
	setupAffectedNodesFunc := func() ([]*corev1.Node, error) {
		return nodeList, nil
	}

	// affectedNodesFunc for fencing: only nodes with fencing secrets (subset of nodeList)
	// Nodes without secrets won't block the job; when secret is added, event handler triggers restart
	fencingAffectedNodesFunc := func() ([]*corev1.Node, error) {
		return getNodesWithFencingSecrets(nodeList, kubeInformersForNamespaces)
	}

	// Cluster-wide jobs: setup and fencing can run on any node
	// Multi-node: retries=3 means try all nodes 3 times before degrading
	jobs.RunTNFJobController(ctx, tools.JobTypeSetup, nil, schedulableNodesFunc, setupAffectedNodesFunc, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.AllConditions)

	// Fencing job with drift detection: captures node UIDs + fencing secret UIDs
	fencingJobConfigFunc := createFencingJobConfigFunc(nodeList, kubeInformersForNamespaces)
	jobs.RunTNFJobController(ctx, tools.JobTypeFencing, nil, schedulableNodesFunc, fencingAffectedNodesFunc, fencingJobConfigFunc, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

	return nil
}

// getNodesWithFencingSecrets filters the given nodes to only those that have fencing secrets.
// Matches secrets by name: fencing-credentials-{nodeName}.
// Nodes without secrets are excluded from the result (job won't be blocked waiting for them).
// When a fencing secret is added, the event handler in starter.go triggers a job restart.
func getNodesWithFencingSecrets(nodes []*corev1.Node, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) ([]*corev1.Node, error) {
	secretsLister := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister()

	// Build set of node names that have fencing secrets
	nodesWithSecrets := make(map[string]bool)
	for _, node := range nodes {
		// Check for fencing-credentials-{nodeName}
		secretName := tools.GetFencingSecretName(node.Name)
		_, err := secretsLister.Secrets(operatorclient.TargetNamespace).Get(secretName)
		if err == nil {
			nodesWithSecrets[node.Name] = true
		}
		// Note: We don't check for MAC-hashed secrets here (would require expensive matching).
		// Those nodes will be configured when the fencing job runs and succeeds.
	}

	// Filter to only nodes with secrets
	var result []*corev1.Node
	for _, node := range nodes {
		if nodesWithSecrets[node.Name] {
			result = append(result, node)
		}
	}

	return result, nil
}

// getNodesFromLatestUpdateSetupConfigMap returns Node objects listed in the latest update-setup ConfigMap.
// Finds the ConfigMap with highest generation number, parses its "nodes" JSON, and matches to Node objects.
// This ensures update-setup job only waits for nodes it's actually supposed to configure.
func getNodesFromLatestUpdateSetupConfigMap(nodeInformer cache.SharedIndexInformer, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) ([]*corev1.Node, error) {
	// List all update-setup ConfigMaps
	configMapsLister := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister()
	configMaps, err := configMapsLister.List(labels.SelectorFromSet(labels.Set{
		"app.kubernetes.io/component": tools.TnfUpdateSetupComponentValue,
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to list update-setup ConfigMaps: %w", err)
	}

	if len(configMaps) == 0 {
		// No ConfigMap exists yet - return empty list (job not ready to run)
		klog.V(4).Infof("No update-setup ConfigMaps found - cannot determine affected nodes")
		return []*corev1.Node{}, nil
	}

	// Find ConfigMap with latest generation
	var latestCM *corev1.ConfigMap
	var latestGeneration int64 = 0
	for _, cm := range configMaps {
		genStr := cm.Data["generation"]
		gen, err := strconv.ParseInt(genStr, 10, 64)
		if err != nil {
			klog.Warningf("ConfigMap %s has invalid generation %q: %v", cm.Name, genStr, err)
			continue
		}
		if gen > latestGeneration {
			latestGeneration = gen
			latestCM = cm
		}
	}

	if latestCM == nil {
		klog.Warningf("No valid update-setup ConfigMaps found with parseable generation")
		return []*corev1.Node{}, nil
	}

	// Parse nodes JSON from ConfigMap
	nodesJSON := latestCM.Data["nodes"]
	if nodesJSON == "" {
		klog.Warningf("ConfigMap %s has empty nodes field", latestCM.Name)
		return []*corev1.Node{}, nil
	}

	type nodeInfo struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
		UID  string `json:"uid"`
	}
	var nodeInfos []nodeInfo
	if err := json.Unmarshal([]byte(nodesJSON), &nodeInfos); err != nil {
		return nil, fmt.Errorf("failed to unmarshal nodes from ConfigMap %s: %w", latestCM.Name, err)
	}

	// Get all control plane nodes from informer
	allNodes, err := ceohelpers.ListNodesFromInformer(nodeInformer)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes from informer: %w", err)
	}

	// Match ConfigMap nodes to Node objects by name+UID
	nodeMap := make(map[string]*corev1.Node)
	for _, node := range allNodes {
		key := node.Name + ":" + string(node.UID)
		nodeMap[key] = node
	}

	var result []*corev1.Node
	var missingNodes []string
	for _, info := range nodeInfos {
		key := info.Name + ":" + info.UID
		if node, exists := nodeMap[key]; exists {
			result = append(result, node)
		} else {
			// Node in ConfigMap but not found in cluster - ConfigMap is stale
			missingNodes = append(missingNodes, fmt.Sprintf("%s (UID %s)", info.Name, info.UID))
		}
	}

	if len(missingNodes) > 0 {
		// SAFETY: Fail rather than admitting job with subset of intended nodes
		// Reconciliation engine will detect drift and create new job with current nodes
		return nil, fmt.Errorf("ConfigMap generation %d references nodes not found in cluster: %v - job is stale, reconciliation will replace it", latestGeneration, missingNodes)
	}

	klog.V(4).Infof("Update-setup ConfigMap generation %d affects nodes: %v", latestGeneration, tools.GetNodeNames(result))
	return result, nil
}

// waitForEtcdBootstrapCompleted waits for etcd bootstrap to complete.
// If etcd is not yet running in cluster, it waits for bootstrap teardown.
func waitForEtcdBootstrapCompleted(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	isEtcdRunningInCluster, err := ceohelpers.IsEtcdRunningInCluster(ctx, operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check if bootstrap is completed: %w", err)
	}
	if !isEtcdRunningInCluster {
		klog.Infof("waiting for bootstrap to complete with etcd running in cluster")
		clientConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		err = bootstrapteardown.WaitForEtcdBootstrap(ctx, clientConfig)
		if err != nil {
			return fmt.Errorf("failed to wait for bootstrap to complete: %w", err)
		}
	}
	return nil
}

