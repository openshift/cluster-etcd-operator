package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// SchedulableNodesFunc returns ready nodes where job can run (round-robin target).
type SchedulableNodesFunc func() ([]*corev1.Node, error)

// AffectedNodesFunc returns nodes that must be Ready before job admission (ready check target).
type AffectedNodesFunc func() ([]*corev1.Node, error)

// JobConfigFunc returns deterministic JSON for drift detection. Changes trigger job restart.
type JobConfigFunc func() (string, error)

var (
	// runningControllers tracks which controllers are already running to prevent duplicates
	runningControllers = make(map[string]bool)
	// runningControllersMutex protects the runningControllers map
	runningControllersMutex sync.Mutex

	// restartJobLocks tracks in-flight RestartJobOrRunController calls to prevent parallel execution
	restartJobLocks = make(map[string]*sync.Mutex)
	// restartJobLocksMutex protects the restartJobLocks map
	restartJobLocksMutex sync.Mutex

	// retryState tracks multi-node retry state for jobs using TargetNodesFunc
	// Map key is job name, value tracks current attempt and node index
	retryState = make(map[string]*JobRetryState)
	// retryStateMutex protects access to retryState map
	retryStateMutex sync.Mutex

	// jobBlockedSince tracks when jobs first became blocked due to affected nodes not being ready.
	// Map key is job name, value is timestamp when first blocked.
	// Used to return error after 10 minutes, triggering Degraded condition via WithSyncDegradedOnError.
	jobBlockedSince = make(map[string]time.Time)
	// jobBlockedMutex protects access to jobBlockedSince map
	jobBlockedMutex sync.Mutex

	// jobNoSchedulableNodesSince tracks when jobs first had no schedulable nodes.
	// Map key is job name, value is timestamp when first detected.
	// Used to return error after 10 minutes, triggering Degraded condition via WithSyncDegradedOnError.
	jobNoSchedulableNodesSince = make(map[string]time.Time)
	// jobNoSchedulableNodesMutex protects access to jobNoSchedulableNodesSince map
	jobNoSchedulableNodesMutex sync.Mutex
)

// JobRetryState tracks retry progress for multi-node jobs
type JobRetryState struct {
	Mu               sync.Mutex // Protects fields below
	AttemptNumber    int        // Current attempt (1-N)
	NodeIndex        int        // Index of node to try in current attempt
	TargetNodes      []string   // Cached node names from last targetNodesFunc call
	MaxRetryAttempts int        // Maximum attempts before degrading
	LastFailTime     time.Time  // When last failure occurred
	LastJobConfig    string     // Serialized config when job was created (for drift detection)
}

const (
	// blockedConditionTimeout is how long to wait before returning error when jobs are blocked.
	// Error triggers Degraded condition via WithSyncDegradedOnError.
	blockedConditionTimeout = 10 * time.Minute
)

// manageTimedBlockedCondition manages the Degraded condition for blocked jobs.
// Returns error after timeout to trigger WithSyncDegradedOnError. When unblocked, returns success
// and lets natural syncManaged flow determine degraded state (job complete/failed/running).
// Returns (ready bool, error):
//   - (true, nil): unblocked, ready to proceed
//   - (false, nil): blocked but < timeout, wait without reporting
//   - (false, error): blocked >= timeout, error triggers Degraded
func manageTimedBlockedCondition(
	jobName string,
	isBlocked bool,
	blockedSinceMap map[string]time.Time,
	mapMutex *sync.Mutex,
	errorMessage string,
) (bool, error) {
	if isBlocked {
		// Track blocked time
		mapMutex.Lock()
		blockedSince, wasBlocked := blockedSinceMap[jobName]
		if !wasBlocked {
			// First time blocked - record timestamp
			blockedSinceMap[jobName] = time.Now()
			blockedSince = blockedSinceMap[jobName]
		}
		mapMutex.Unlock()

		// Return error if blocked long enough (WithSyncDegradedOnError will handle it)
		if time.Since(blockedSince) > blockedConditionTimeout {
			return false, fmt.Errorf("%s", errorMessage)
		}

		return false, nil // Blocked but not long enough yet
	}

	// Clear blocked tracking (condition cleared naturally via syncManaged flow)
	mapMutex.Lock()
	delete(blockedSinceMap, jobName)
	mapMutex.Unlock()

	return true, nil
}

// manageBlockedCondition tracks blocked state for jobs waiting on nodes to become ready.
// Returns (true, nil) if nodes are ready, (false, nil) if blocked but not timed out,
// or (false, error) after 10 min timeout (triggers WithSyncDegradedOnError).
func manageBlockedCondition(jobName string, notReadyNodes []string) (bool, error) {
	return manageTimedBlockedCondition(
		jobName,
		len(notReadyNodes) > 0,
		jobBlockedSince,
		&jobBlockedMutex,
		fmt.Sprintf("Affected nodes not ready: %v", notReadyNodes),
	)
}

// manageNoSchedulableNodesBlockedCondition tracks blocked state when no schedulable nodes are available.
// Returns (true, nil) if schedulable nodes exist, (false, nil) if blocked but not timed out,
// or (false, error) after 10 min timeout (triggers WithSyncDegradedOnError).
func manageNoSchedulableNodesBlockedCondition(jobName string, hasSchedulableNodes bool) (bool, error) {
	return manageTimedBlockedCondition(
		jobName,
		!hasSchedulableNodes,
		jobNoSchedulableNodesSince,
		&jobNoSchedulableNodesMutex,
		"No schedulable nodes available",
	)
}

// checkNodesReadinessAndSetCondition checks if all nodes are ready and tracks blocked state with timeout.
// Returns (true, nil) if all nodes ready, (false, nil) if some not ready but not timed out,
// or (false, error) if nodes not ready > 10 min (triggers WithSyncDegradedOnError).
func checkNodesReadinessAndSetCondition(nodes []*corev1.Node, jobName string) (bool, error) {
	// Collect all not-ready nodes
	var notReadyNodes []string

	for _, node := range nodes {
		if !tools.IsNodeReady(node) {
			notReadyNodes = append(notReadyNodes, node.Name)
		}
	}

	// Track blocked state with timeout, propagate error if blocked >= timeout
	ready, err := manageBlockedCondition(jobName, notReadyNodes)
	if err != nil {
		return false, err // Propagate error to trigger WithSyncDegradedOnError
	}
	if !ready {
		return false, nil // Blocked but not long enough yet
	}

	return true, nil
}

// syncMultiNodeJobState manages retry state for multi-node job (node changes, config drift, failures).
// Detects drift and failures, updates retry state accordingly. Job deletion/recreation is handled
// by ApplyJob via drift detection (NodeName changed), except for real config/node drift where
// jobs are explicitly deleted before resetting state.
func syncMultiNodeJobState(ctx context.Context, jobName string, schedulableNodesFunc SchedulableNodesFunc, affectedNodesFunc AffectedNodesFunc, jobConfigFunc JobConfigFunc, maxRetryAttempts int, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient) error {
	// Check affected nodes readiness before admitting/retrying job
	if affectedNodesFunc != nil {
		affectedNodes, err := affectedNodesFunc()
		if err != nil {
			return fmt.Errorf("failed to get affected nodes: %w", err)
		}

		// Check readiness and manage blocked condition
		ready, err := checkNodesReadinessAndSetCondition(affectedNodes, jobName)
		if err != nil {
			return err
		}
		if !ready {
			// Block job admission - nodes not ready yet
			return nil
		}
	}

	// Lock the global state map
	// Check if state already exists before computing (fast path)
	retryStateMutex.Lock()
	state, exists := retryState[jobName]
	retryStateMutex.Unlock()

	if !exists {
		// Compute schedulable nodes and config outside lock to avoid blocking
		schedulableNodes, err := schedulableNodesFunc()
		if err != nil {
			return fmt.Errorf("failed to get schedulable nodes: %w", err)
		}
		if len(schedulableNodes) == 0 {
			_, err := manageNoSchedulableNodesBlockedCondition(jobName, false)
			return err // Propagate error to trigger WithSyncDegradedOnError if blocked >= timeout
		}

		// Get initial job config
		var initialConfig string
		if jobConfigFunc != nil {
			initialConfig, err = jobConfigFunc()
			if err != nil {
				return fmt.Errorf("failed to get initial job config: %w", err)
			}
		}

		// Now acquire lock and re-check state
		retryStateMutex.Lock()
		state, exists = retryState[jobName]
		if !exists {
			// Another controller didn't create it while we were computing, safe to initialize
			state = &JobRetryState{
				AttemptNumber:    1,
				NodeIndex:        0,
				TargetNodes:      tools.GetNodeNames(schedulableNodes),
				MaxRetryAttempts: maxRetryAttempts,
				LastJobConfig:    initialConfig,
			}
			retryState[jobName] = state
		}
		retryStateMutex.Unlock()

		// Clear blocked condition now that schedulable nodes are available
		if _, err := manageNoSchedulableNodesBlockedCondition(jobName, true); err != nil {
			klog.Errorf("Failed to clear blocked condition for %s: %v", jobName, err)
		}

		klog.V(4).Infof("Starting job %s - attempt %d/%d, will try schedulable nodes: %v",
			jobName, state.AttemptNumber, state.MaxRetryAttempts, state.TargetNodes)
		return nil
	}

	// Lock this job's state for the rest of the sync
	state.Mu.Lock()
	defer state.Mu.Unlock()

	// Check if schedulable nodes have changed
	schedulableNodes, err := schedulableNodesFunc()
	if err != nil {
		return fmt.Errorf("failed to get schedulable nodes: %w", err)
	}
	if len(schedulableNodes) == 0 {
		_, err = manageNoSchedulableNodesBlockedCondition(jobName, false)
		return err // Propagate error to trigger WithSyncDegradedOnError if blocked >= timeout
	}

	// Clear blocked condition if schedulable nodes are now available
	if _, err := manageNoSchedulableNodesBlockedCondition(jobName, true); err != nil {
		klog.Errorf("Failed to clear blocked condition for %s: %v", jobName, err)
	}

	// Nodes are already sorted by schedulableNodesFunc (critical for round-robin NodeIndex)
	currentSchedulableNodes := tools.GetNodeNames(schedulableNodes)
	nodesChanged := !tools.StringSlicesEqual(state.TargetNodes, currentSchedulableNodes)

	// Check if job config has changed
	var configChanged bool
	var currentConfig string
	if jobConfigFunc != nil {
		currentConfig, err = jobConfigFunc()
		if err != nil {
			return fmt.Errorf("failed to get current job config: %w", err)
		}
		configChanged = state.LastJobConfig != currentConfig
	}

	// If nodes or config changed, restart job
	if nodesChanged || configChanged {
		if nodesChanged {
			klog.Infof("Job %s schedulable nodes changed from %v to %v - resetting retry state and deleting job",
				jobName, state.TargetNodes, currentSchedulableNodes)
		}
		if configChanged {
			klog.Infof("Job %s config changed - resetting retry state and deleting job. Old: %s, New: %s",
				jobName, state.LastJobConfig, currentConfig)
		}

		// Delete existing job if it exists (config drift or wrong target node)
		_, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, v1.GetOptions{})
		if err == nil {
			// Job exists - delete it so we can recreate with new config
			if err := DeleteAndWait(ctx, kubeClient, jobName, operatorclient.TargetNamespace); err != nil {
				return fmt.Errorf("failed to delete job %s after config/nodes changed: %w", jobName, err)
			}
			klog.Infof("Deleted job %s after config/nodes changed", jobName)
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing job %s: %w", jobName, err)
		}

		// Update state only after successful deletion
		state.AttemptNumber = 1
		state.NodeIndex = 0
		state.TargetNodes = currentSchedulableNodes
		state.LastJobConfig = currentConfig

		return nil
	}

	// Get existing job (if any)
	existingJob, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, v1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No job exists - nothing to sync (will be created by JobController)
			return nil
		}
		return fmt.Errorf("failed to get job %s: %w", jobName, err)
	}

	// Job exists - check if it's done
	if IsComplete(*existingJob) {
		// Success - condition cleared naturally via syncManaged flow
		klog.V(4).Infof("Job %s completed successfully", jobName)
		return nil
	}

	if IsFailed(*existingJob) || IsStopped(*existingJob) {
		// Failed - calculate next retry position
		currentNodeIndex := state.NodeIndex
		currentAttemptNumber := state.AttemptNumber
		klog.V(4).Infof("Job %s failed on node index %d (attempt %d) - moving to next node",
			jobName, currentNodeIndex, currentAttemptNumber)

		// Calculate next position
		nextNodeIndex := currentNodeIndex + 1
		nextAttemptNumber := currentAttemptNumber

		// Check if we've exhausted all nodes in this attempt
		exhaustedNodes := nextNodeIndex >= len(schedulableNodes)
		maxRetriesExceeded := false
		if exhaustedNodes {
			nextNodeIndex = 0
			exhaustedAttempts := currentAttemptNumber >= state.MaxRetryAttempts
			if exhaustedAttempts {
				// Exceeded max attempts - reset to attempt 1 and continue trying
				klog.Warningf("Job %s exhausted all %d attempts (tried %d nodes each), marking degraded",
					jobName, state.MaxRetryAttempts, len(schedulableNodes))
				nextAttemptNumber = 1
				maxRetriesExceeded = true
			} else {
				// Start new attempt
				nextAttemptNumber++
				klog.V(4).Infof("Job %s exhausted all nodes in attempt %d, starting attempt %d/%d",
					jobName, currentAttemptNumber, nextAttemptNumber, state.MaxRetryAttempts)
			}
		}

		// Update retry state - ApplyJob will detect drift (NodeName, node-index, or attempt labels changed) and recreate job
		klog.V(4).Infof("Job %s failed - updating retry state to node index %d", jobName, nextNodeIndex)
		state.NodeIndex = nextNodeIndex
		state.AttemptNumber = nextAttemptNumber

		// If we just exceeded max retries, return error to set degraded condition
		if maxRetriesExceeded {
			return errors.New(DegradedMessageMaxRetries)
		}
	}

	// Job is running - nothing to do
	return nil
}

// configureMultiNodeJob configures job based on current retry state (pure function - reads state only).
func configureMultiNodeJob(job *batchv1.Job, maxRetryAttempts int) error {
	jobName := job.Name

	// Get current state (must have been initialized by syncMultiNodeJobState)
	retryStateMutex.Lock()
	state, exists := retryState[jobName]
	retryStateMutex.Unlock()

	if !exists {
		// State should always exist when this function is called
		// If it doesn't, it's a programming error
		return fmt.Errorf("retry state for job %s does not exist (should have been created by syncMultiNodeJobState)", jobName)
	}

	// Lock state for reading
	state.Mu.Lock()
	nodeIndex := state.NodeIndex
	attemptNumber := state.AttemptNumber
	targetNodes := state.TargetNodes
	state.Mu.Unlock()

	// Validate node index against cached target nodes
	if nodeIndex >= len(targetNodes) {
		return fmt.Errorf("invalid node index %d (only %d nodes in cached state)", nodeIndex, len(targetNodes))
	}

	selectedNodeName := targetNodes[nodeIndex]
	klog.V(4).Infof("Job %s attempt %d/%d: scheduling on node %s (index %d/%d)",
		jobName, attemptNumber, maxRetryAttempts, selectedNodeName,
		nodeIndex+1, len(targetNodes))

	// Configure job to run on selected node
	job.Spec.Template.Spec.NodeName = selectedNodeName
	job.Labels[LabelAttempt] = fmt.Sprintf("%d", attemptNumber)
	job.Labels[LabelNodeIndex] = fmt.Sprintf("%d", nodeIndex)
	job.Labels[LabelJobType] = "cluster"

	return nil
}

// resetJobRetryState clears the retry state for a job (called on success or when starting fresh)
func resetJobRetryState(jobName string) {
	retryStateMutex.Lock()
	defer retryStateMutex.Unlock()
	delete(retryState, jobName)
	klog.V(2).Infof("Reset retry state for job %s", jobName)
}

// RunNodeJobController starts job controller for node-specific job (auth, after-setup).
func RunNodeJobController(ctx context.Context, jobType tools.JobType, node *corev1.Node, retries int, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, controlPlaneNodeInformer cache.SharedIndexInformer, conditions []string) {
	jobNodeName := &node.Name
	jobName := jobType.GetJobName(jobNodeName)

	// Check if controller already running
	runningControllersMutex.Lock()
	if runningControllers[jobName] {
		runningControllersMutex.Unlock()
		klog.V(4).Infof("Node job controller for %q on node %q is already running, skipping duplicate start", jobType.GetSubCommand(), node.Name)
		return
	}
	runningControllers[jobName] = true
	runningControllersMutex.Unlock()

	klog.Infof("starting node job controller for %q on node %q", jobType.GetSubCommand(), node.Name)

	// Create node lister for fetching fresh node data in hook
	nodeLister := corev1listers.NewNodeLister(controlPlaneNodeInformer.GetIndexer())

	tnfJobController := NewJobController(
		jobName,
		bindata.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		conditions,
		[]factory.Informer{},
		[]JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) (bool, error) {
				job.SetName(jobName)
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()

				// Fetch fresh node from informer to handle node replacement (same name, different UID)
				freshNode, err := nodeLister.Get(node.Name)
				if err != nil {
					if apierrors.IsNotFound(err) {
						// Node was deleted - skip job application and let controller wait for context cancellation
						klog.V(4).Infof("Node %s no longer exists, skipping job %s application (controller will stop when context is canceled)", node.Name, job.Name)
						return false, nil
					}
					return false, fmt.Errorf("failed to get node %s from informer: %w", node.Name, err)
				}

				// Check node readiness before configuring job
				ready, err := checkNodesReadinessAndSetCondition([]*corev1.Node{freshNode}, job.Name)
				if err != nil {
					return false, err
				}
				if !ready {
					klog.V(4).Infof("Skipping job %s creation: node %s not ready", job.Name, freshNode.Name)
					return false, nil
				}

				// Configure node job: schedule on specific node, label with UID, set backoffLimit
				// Use freshNode.UID to detect drift when node is replaced (same name, different UID)
				job.Spec.Template.Spec.NodeName = freshNode.Name
				job.Labels["node"] = string(freshNode.UID)
				job.Spec.BackoffLimit = ptr.To(int32(retries))

				// Set image and command
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()

				return true, nil
			}}...,
	)

	go func() {
		defer func() {
			runningControllersMutex.Lock()
			delete(runningControllers, jobName)
			runningControllersMutex.Unlock()
			klog.Infof("Node job controller for %q on node %q stopped", jobType.GetSubCommand(), node.Name)
		}()
		tnfJobController.Run(ctx, 1)
	}()
}

// RunClusterJobController starts job controller for cluster-wide job with round-robin retry logic.
func RunClusterJobController(ctx context.Context, jobType tools.JobType, schedulableNodesFunc SchedulableNodesFunc, affectedNodesFunc AffectedNodesFunc, jobConfigFunc JobConfigFunc, retries int, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	jobName := jobType.GetJobName(nil)

	// Check if controller already running
	runningControllersMutex.Lock()
	if runningControllers[jobName] {
		runningControllersMutex.Unlock()
		klog.V(4).Infof("Cluster job controller for %q is already running, skipping duplicate start", jobType.GetSubCommand())
		return
	}
	runningControllers[jobName] = true
	runningControllersMutex.Unlock()

	klog.Infof("starting cluster job controller for %q", jobType.GetSubCommand())

	tnfJobController := NewJobController(
		jobName,
		bindata.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		conditions,
		[]factory.Informer{},
		[]JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) (bool, error) {
				job.SetName(jobName)
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()

				// Sync multi-node job state (handles transitions based on job status)
				if err := syncMultiNodeJobState(ctx, job.Name, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, kubeClient, operatorClient); err != nil {
					return false, err
				}

				// Check if retry state was created (won't exist if affected nodes not ready)
				retryStateMutex.Lock()
				_, stateExists := retryState[job.Name]
				retryStateMutex.Unlock()

				if !stateExists {
					klog.V(4).Infof("Skipping job %s creation: retry state not initialized", job.Name)
					return false, nil
				}

				// Configure cluster job: round-robin scheduling, no k8s retries (backoffLimit=0)
				job.Spec.BackoffLimit = ptr.To(int32(0))
				if err := configureMultiNodeJob(job, retries); err != nil {
					return false, err
				}

				// Set image and command
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()

				return true, nil
			}}...,
	)

	go func() {
		defer func() {
			runningControllersMutex.Lock()
			delete(runningControllers, jobName)
			runningControllersMutex.Unlock()
			klog.Infof("Cluster job controller for %q stopped", jobType.GetSubCommand())
		}()
		tnfJobController.Run(ctx, 1)
	}()
}

// RestartClusterJobOrRunController ensures cluster job controller is running, restarting job if it exists.
func RestartClusterJobOrRunController(
	ctx context.Context,
	jobType tools.JobType,
	schedulableNodesFunc SchedulableNodesFunc,
	affectedNodesFunc AffectedNodesFunc,
	jobConfigFunc JobConfigFunc,
	retries int,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	conditions []string,
	existingJobCompletionTimeout time.Duration) error {

	jobName := jobType.GetJobName(nil)

	// Acquire a lock for this specific job to prevent parallel execution
	restartJobLocksMutex.Lock()
	jobLock, exists := restartJobLocks[jobName]
	if !exists {
		jobLock = &sync.Mutex{}
		restartJobLocks[jobName] = jobLock
	}
	restartJobLocksMutex.Unlock()

	jobLock.Lock()
	defer jobLock.Unlock()

	// Check if job already exists
	jobExists := true
	_, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, v1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing job %s: %w", jobName, err)
		}
		jobExists = false
	}

	if !jobExists {
		// No existing job - reset retry state to start fresh, then run controller
		resetJobRetryState(jobName)
		RunClusterJobController(ctx, jobType, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, conditions)
		return nil
	}

	// Job exists, wait for it to stop
	klog.Infof("Job %s already exists, waiting for it to stop", jobName)
	if err := WaitForStopped(ctx, kubeClient, jobName, operatorclient.TargetNamespace, existingJobCompletionTimeout); err != nil {
		return fmt.Errorf("failed to wait for job %s to stop: %w", jobName, err)
	}

	// Delete the job so the controller can recreate it
	klog.Infof("Deleting existing job %s", jobName)
	if err := DeleteAndWait(ctx, kubeClient, jobName, operatorclient.TargetNamespace); err != nil {
		return fmt.Errorf("failed to delete existing job %s: %w", jobName, err)
	}

	// Reset retry state when starting fresh after deleting old job
	resetJobRetryState(jobName)

	// Run controller after cleanup completes (CEO might have been restarted)
	RunClusterJobController(ctx, jobType, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, conditions)

	return nil
}
