package operator

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			klog.Warningf("Found %d control plane nodes - pacemaker only supports 2, skipping initial setup", len(k8sNodes))
			return nil
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

		// Check all control plane nodes are Ready
		for _, node := range k8sNodes {
			if !tools.IsNodeReady(node) {
				klog.V(4).Infof("Control plane node %s not Ready - waiting for all control plane nodes before starting job controllers", node.Name)
				return nil
			}
		}

		klog.V(4).Infof("Transition complete - ensuring job controllers running for %d ready control plane nodes", len(k8sNodes))
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
			jobs.RunTNFJobController(ctx, tools.JobTypeAuth, nodeTarget, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
			jobs.RunTNFJobController(ctx, tools.JobTypeAfterSetup, nodeTarget, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		}

		clusterWideTargetNodesFunc := func() ([]*corev1.Node, error) {
			return lifecycleManager.getActivePacemakerNodes()
		}

		// DO NOT start setup job controller - transition already complete, setup job is one-time only
		// Day 2 changes are handled by update-setup job via ReconcilePacemakerConfig

		fencingJobConfigFunc := createFencingJobConfigFunc(nodeList, kubeInformersForNamespaces)
		jobs.RunTNFJobController(ctx, tools.JobTypeFencing, nil, clusterWideTargetNodesFunc, fencingJobConfigFunc, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

		// Check if there's a stopped update-setup job that needs a controller
		// (operator may have restarted while update-setup was running/stopped)
		updateSetupJobName := tools.JobTypeUpdateSetup.GetJobName(nil)
		updateSetupJob, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, updateSetupJobName, metav1.GetOptions{})
		if err == nil && jobs.IsStopped(*updateSetupJob) && !jobs.IsControllerRunning(updateSetupJobName) {
			klog.Infof("Found stopped update-setup job without controller - starting controller to update conditions")
			jobs.RunTNFJobController(ctx, tools.JobTypeUpdateSetup, nil, clusterWideTargetNodesFunc, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
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
		jobs.RunTNFJobController(ctx, tools.JobTypeAuth, nodeTarget, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		jobs.RunTNFJobController(ctx, tools.JobTypeAfterSetup, nodeTarget, nil, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
	}

	// Create targetNodesFunc for multi-node retry across all control plane nodes
	// Used by setup and fencing jobs which can run on any node
	clusterWideTargetNodesFunc := func() ([]*corev1.Node, error) {
		return lifecycleManager.getActivePacemakerNodes()
	}

	// Cluster-wide jobs: setup and fencing can run on any node
	// Multi-node: retries=3 means try all nodes 3 times before degrading
	jobs.RunTNFJobController(ctx, tools.JobTypeSetup, nil, clusterWideTargetNodesFunc, nil, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.AllConditions)

	// Fencing job with drift detection: captures node UIDs + fencing secret UIDs
	fencingJobConfigFunc := createFencingJobConfigFunc(nodeList, kubeInformersForNamespaces)
	jobs.RunTNFJobController(ctx, tools.JobTypeFencing, nil, clusterWideTargetNodesFunc, fencingJobConfigFunc, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

	return nil
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

