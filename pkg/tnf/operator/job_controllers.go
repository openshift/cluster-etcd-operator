package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// startBootstrapJobControllersFunc is a variable to allow mocking in tests
	startBootstrapJobControllersFunc = startBootstrapJobControllers

	// startRuntimeJobControllersFunc is a variable to allow mocking in tests
	startRuntimeJobControllersFunc = startRuntimeJobControllers

	// retryBackoffConfig allows customizing retry behavior for tests
	retryBackoffConfig = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Steps:    9, // ~10 minutes total: 5s + 10s + 20s + 40s + 80s + 120s + 120s + 120s + 120s
		Cap:      2 * time.Minute,
	}
)

// startJobControllers starts TNF job controllers based on transition state.
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
func (c *pacemakerLifecycleManager) startJobControllers(ctx context.Context) error {
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, c.operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check external etcd transition status: %w", err)
	}

	if c.controlPlaneNodeInformer == nil || !c.controlPlaneNodeInformer.HasSynced() {
		klog.V(4).Infof("Skipping job controller startup - node informer not synced yet")
		return nil
	}

	// TNF and arbiters are mutually exclusive - no filtering needed
	controlPlaneNodes, err := tools.ListNodesFromInformer(c.controlPlaneNodeInformer)
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}
	if !transitionComplete {
		// Pacemaker only supports 2 control plane nodes
		if len(controlPlaneNodes) > 2 {
			return fmt.Errorf("TNF requires exactly 2 control plane nodes for initial setup, found %d - pacemaker does not support >2 nodes", len(controlPlaneNodes))
		}
		if len(controlPlaneNodes) < 2 {
			klog.V(4).Infof("Waiting for 2 control plane nodes for initial setup (current: %d)", len(controlPlaneNodes))
			return nil
		}

		for _, node := range controlPlaneNodes {
			if !tools.IsNodeReady(node) {
				klog.V(4).Infof("Control plane node %s not Ready - waiting for both nodes before initial setup", node.Name)
				return nil
			}
		}

		klog.V(2).Infof("Both control plane nodes ready - starting initial job controllers with retry")
		return c.retryInitialTransitionOrDegrade(ctx, controlPlaneNodes)
	} else {
		// Called on every sync to handle missed node events
		if len(controlPlaneNodes) == 0 {
			klog.V(4).Infof("No control plane nodes found, skipping job controller startup")
			return nil
		}

		// Note: Node readiness checked at job admission level (not controller startup).
		// Jobs report TNF<JobName>Degraded if affected nodes not ready or no schedulable nodes (after 10 min timeout).

		klog.V(4).Infof("Transition complete - ensuring job controllers running for %d control plane nodes", len(controlPlaneNodes))
		err = c.startRuntimeJobControllersWithLock(ctx, controlPlaneNodes)
		if err != nil {
			return err
		}

		return nil
	}
}

// retryInitialTransitionOrDegrade starts job controllers with exponential backoff.
// Sets TNFJobControllersDegraded based on success or failure.
// Only used for initial transition (before ExternalEtcdTransitionCompleted).
func (c *pacemakerLifecycleManager) retryInitialTransitionOrDegrade(ctx context.Context, nodes []*corev1.Node) error {
	var setupErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoffConfig, func(ctx context.Context) (bool, error) {
		setupErr = c.startBootstrapJobControllersWithLock(ctx, nodes)
		if setupErr != nil {
			klog.Warningf("failed to setup TNF job controllers, will retry: %v", setupErr)
			return false, nil
		}
		return true, nil
	})

	if err != nil || setupErr != nil {
		displayErr := setupErr
		if displayErr == nil {
			displayErr = err
		}
		klog.Errorf("failed to setup TNF job controllers after retries: %v", displayErr)

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

// startBootstrapJobControllersWithLock serializes bootstrap job controller startup to prevent concurrent:
// - etcd bootstrap / stable revision waits
// - duplicate job controller creation
// - races in wait logic
func (c *pacemakerLifecycleManager) startBootstrapJobControllersWithLock(ctx context.Context, nodes []*corev1.Node) error {
	c.startJobControllersMu.Lock()
	defer c.startJobControllersMu.Unlock()

	return startBootstrapJobControllersFunc(ctx, nodes, c.controllerContext, c.operatorClient, c.kubeClient, c.kubeInformersForNamespaces, c.etcdInformer, c)
}

// startRuntimeJobControllersWithLock serializes runtime job controller startup to prevent concurrent:
// - duplicate job controller creation
func (c *pacemakerLifecycleManager) startRuntimeJobControllersWithLock(ctx context.Context, nodes []*corev1.Node) error {
	c.startJobControllersMu.Lock()
	defer c.startJobControllersMu.Unlock()

	return startRuntimeJobControllersFunc(ctx, nodes, c.controllerContext, c.operatorClient, c.kubeClient, c.kubeInformersForNamespaces, c.etcdInformer, c)
}

// startCommonJobControllers creates the job controllers that run in both bootstrap and runtime modes.
// Creates auth jobs (per-node), setup job (cluster-wide), fencing job (cluster-wide), and after-setup jobs (per-node).
// Clears legacy condition names from upgrades.
func startCommonJobControllers(
	ctx context.Context,
	controlPlaneNodeList []*corev1.Node,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	lifecycleManager *pacemakerLifecycleManager,
) {
	// Node job controllers (per-node)
	for _, node := range controlPlaneNodeList {
		jobs.RunNodeJobController(ctx, tools.JobTypeAuth, node, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, lifecycleManager.controlPlaneNodeInformer, jobs.DefaultConditions)
		jobs.RunNodeJobController(ctx, tools.JobTypeAfterSetup, node, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, lifecycleManager.controlPlaneNodeInformer, jobs.DefaultConditions)
	}

	// schedulableNodesFunc: returns ready nodes where job can run (K8s ∩ Pacemaker intersection)
	// During bootstrap: PacemakerCluster CR doesn't exist yet, so getActivePacemakerNodes falls back to all ready nodes.
	// Returns error only when informer is unsynced or no ready nodes exist.
	schedulableNodesFunc := func() ([]*corev1.Node, error) {
		return lifecycleManager.getActivePacemakerNodes()
	}

	// affectedNodesFunc for setup: all control plane nodes (ready or not)
	// Job waits for these nodes to become ready before proceeding
	// Query dynamically from informer to avoid stale node list
	setupAffectedNodesFunc := func() ([]*corev1.Node, error) {
		return tools.ListNodesFromInformer(lifecycleManager.controlPlaneNodeInformer)
	}

	// affectedNodesFunc for fencing: all control plane nodes (ready or not) that have fencing secrets
	// Job waits for these nodes to become ready before proceeding
	// Nodes without secrets won't block the job; when secret is added, drift detection triggers restart
	// Query dynamically from informer to avoid stale node list
	fencingAffectedNodesFunc := func() ([]*corev1.Node, error) {
		nodes, err := tools.ListNodesFromInformer(lifecycleManager.controlPlaneNodeInformer)
		if err != nil {
			return nil, err
		}
		return getNodesWithFencingSecrets(nodes, kubeInformersForNamespaces)
	}

	// Setup job controller: maintains conditions for completed setup job
	// Even though setup is one-time only, the controller must run to keep conditions current
	// (fencing/auth/after-setup jobs wait for setup job completion status)
	jobs.RunClusterJobController(ctx, tools.JobTypeSetup, schedulableNodesFunc, setupAffectedNodesFunc, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.AllConditions)

	// Fencing job with drift detection: captures node UIDs + fencing secret UIDs
	fencingJobConfigFunc := createFencingJobConfigFunc(lifecycleManager, kubeInformersForNamespaces)
	jobs.RunClusterJobController(ctx, tools.JobTypeFencing, schedulableNodesFunc, fencingAffectedNodesFunc, fencingJobConfigFunc, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

	// Clear legacy condition names from upgrades (controllers recreate with new names)
	clearLegacyConditions(ctx, operatorClient)
}

// startRuntimeJobControllers creates TNF job controllers after transition is complete.
// Ensures controllers are running (idempotent restart safe).
// Also starts update-setup job (if 2 nodes), status collector CronJob, and health check controller.
func startRuntimeJobControllers(
	ctx context.Context,
	controlPlaneNodeList []*corev1.Node,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
	lifecycleManager *pacemakerLifecycleManager) error {

	klog.V(4).Infof("Transition complete - starting runtime job controllers")

	// Start common job controllers (auth, after-setup, setup, fencing)
	startCommonJobControllers(ctx, controlPlaneNodeList, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, lifecycleManager)

	// Update-setup job: ensures pacemaker cluster configuration is current
	// Runs post-transition only (not needed during bootstrap)
	// Only runs when exactly 2 control plane nodes exist (pacemaker limitation)
	if len(controlPlaneNodeList) == 2 {
		// schedulableNodesFunc: returns ready nodes where job can run (K8s ∩ Pacemaker intersection)
		schedulableNodesFunc := func() ([]*corev1.Node, error) {
			return lifecycleManager.getActivePacemakerNodes()
		}

		// affectedNodesFunc for update-setup: K8s ∩ Pacemaker active nodes (ready only)
		// Only blocks on nodes that are actually in the Pacemaker cluster configuration.
		// This prevents deadlock when a node is removed from Pacemaker but can't become Ready
		// without update-setup re-adding it (kubelet is a Pacemaker resource).
		updateSetupAffectedNodesFunc := func() ([]*corev1.Node, error) {
			return lifecycleManager.getActivePacemakerNodes()
		}

		jobs.RunClusterJobController(ctx, tools.JobTypeUpdateSetup, schedulableNodesFunc, updateSetupAffectedNodesFunc, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
	} else {
		klog.V(4).Infof("Skipping update-setup job controller: requires exactly 2 control plane nodes, have %d", len(controlPlaneNodeList))
	}

	// Start status collector (only after transition is complete, when Pacemaker exists)
	lifecycleManager.runPacemakerStatusCollectorCronJob(ctx)

	// Start health check controller (only after transition is complete, when Pacemaker exists)
	lifecycleManager.runPacemakerHealthCheckController(ctx)

	// Cert watcher DaemonSet: watches CA bundle files on disk and restarts
	// the local etcd when they change. Runs independently of the operator
	// and API server — prevents force_new_cluster during CA rotation.
	// Ungated by node count so it keeps running during single-node and
	// node-replacement windows (the OCPBUGS-84695 recovery scenario). Log on
	// error rather than aborting: the other runtime controllers must still run.
	if err := lifecycleManager.ensureCertWatcherDaemonSet(ctx); err != nil {
		klog.Errorf("failed to ensure cert-watcher DaemonSet: %v", err)
	}

	klog.V(4).Infof("Runtime controllers running")
	return nil
}

// startBootstrapJobControllers creates TNF job controllers during initial bootstrap.
// Waits for etcd bootstrap completion and stable revision before creating controllers.
// Creates auth jobs (per-node), setup job (cluster-wide), fencing job (cluster-wide), and after-setup jobs (per-node).
// Does not start status collector or health check controller (Pacemaker doesn't exist yet).
func startBootstrapJobControllers(
	ctx context.Context,
	controlPlaneNodeList []*corev1.Node,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
	lifecycleManager *pacemakerLifecycleManager) error {

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

	// Start common job controllers (auth, after-setup, setup, fencing)
	// The order of job creation does not matter, the jobs wait on each other as needed
	startCommonJobControllers(ctx, controlPlaneNodeList, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, lifecycleManager)

	return nil
}

// RemoveConditionFn returns a func to remove a condition entirely from operator status.
func RemoveConditionFn(conditionType string) v1helpers.UpdateStatusFunc {
	return func(status *operatorv1.OperatorStatus) error {
		v1helpers.RemoveOperatorCondition(&status.Conditions, conditionType)
		return nil
	}
}

// clearLegacyConditions removes old-format TNF condition names from operator status.
// Old format: tnf-{job}-job{Condition} (e.g., tnf-setup-jobDegraded, tnf-auth-job-master-0-637363beAvailable)
// New format: TNF{Job}{Condition} (e.g., TNFSetupJobDegraded, TNFAuthJobMaster0637363beAvailable)
// Safe to call after job controllers start - they recreate conditions with new names on first sync.
func clearLegacyConditions(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) {
	_, opStatus, _, err := operatorClient.GetStaticPodOperatorState()
	if err != nil || opStatus == nil {
		klog.V(2).Infof("Cannot get operator status for legacy condition cleanup: %v", err)
		return
	}

	var removeFuncs []v1helpers.UpdateStatusFunc
	for _, cond := range opStatus.Conditions {
		// Match old pattern: starts with "tnf-", contains "job", and ends with condition type
		// Cluster jobs: tnf-setup-jobAvailable, tnf-fencing-jobDegraded
		// Node jobs: tnf-auth-job-master-0-637363beProgressing, tnf-after-setup-job-master-1-64736551Degraded
		if strings.HasPrefix(cond.Type, "tnf-") &&
			strings.Contains(cond.Type, "job") &&
			(strings.HasSuffix(cond.Type, "Degraded") ||
				strings.HasSuffix(cond.Type, "Available") ||
				strings.HasSuffix(cond.Type, "Progressing")) {
			klog.V(2).Infof("Found legacy condition to remove: %s", cond.Type)
			removeFuncs = append(removeFuncs, RemoveConditionFn(cond.Type))
		}
	}

	if len(removeFuncs) > 0 {
		klog.Infof("Removing %d legacy TNF conditions from upgrade", len(removeFuncs))
		_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, removeFuncs...)
		if err != nil {
			klog.Warningf("Failed to remove legacy conditions: %v", err)
		}
	}
}

// Matches secrets by name: fencing-credentials-{nodeName}.
// Returns nodes (ready or not) that have secrets. Job will wait for these nodes to become ready.
// Nodes without secrets are excluded from the result (job won't be blocked waiting for them).
// When a fencing secret is added, drift detection (via ResourceVersion tracking) triggers a job restart.
func getNodesWithFencingSecrets(nodes []*corev1.Node, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) ([]*corev1.Node, error) {
	secretsLister := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister()

	// Build set of node names that have fencing secrets
	nodesWithSecrets := make(map[string]bool)
	for _, node := range nodes {
		// Check for fencing-credentials-{nodeName}
		secretName := fmt.Sprintf("fencing-credentials-%s", node.Name)
		_, err := secretsLister.Secrets(operatorclient.TargetNamespace).Get(secretName)
		if err == nil {
			nodesWithSecrets[node.Name] = true
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check for fencing secret %s: %w", secretName, err)
		}
		// NotFound is treated as "node doesn't have secret" - skip silently
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

// createFencingJobConfigFunc creates a JobConfigFunc for the fencing job.
// Returns a function that captures node UIDs and fencing secret ResourceVersions for drift detection.
// ResourceVersion changes on every secret update (including data changes), enabling drift detection
// without requiring separate event handlers.
func createFencingJobConfigFunc(lifecycleManager *pacemakerLifecycleManager, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) jobs.JobConfigFunc {
	return func() (string, error) {
		// Query nodes dynamically to avoid stale closure capture
		nodes, err := tools.ListNodesFromInformer(lifecycleManager.controlPlaneNodeInformer)
		if err != nil {
			return "", fmt.Errorf("failed to list nodes: %w", err)
		}

		// Collect node UIDs
		nodeUIDs := make([]string, len(nodes))
		for i, node := range nodes {
			nodeUIDs[i] = string(node.UID)
		}
		sort.Strings(nodeUIDs)

		// Collect fencing secret ResourceVersions from informer
		secretsLister := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister()
		allSecrets, err := secretsLister.List(labels.Everything())
		if err != nil {
			return "", fmt.Errorf("failed to list secrets: %w", err)
		}

		var secretVersions []string
		for _, secret := range allSecrets {
			if tools.IsFencingSecret(secret.Name) {
				secretVersions = append(secretVersions, fmt.Sprintf("%s=%s", secret.Name, secret.ResourceVersion))
			}
		}
		sort.Strings(secretVersions)

		config := map[string]interface{}{
			"nodeUIDs":       nodeUIDs,
			"secretVersions": secretVersions,
		}

		configJSON, err := json.Marshal(config)
		if err != nil {
			return "", fmt.Errorf("failed to marshal fencing config: %w", err)
		}

		return string(configJSON), nil
	}
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
