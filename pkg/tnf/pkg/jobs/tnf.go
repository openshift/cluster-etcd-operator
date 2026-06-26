package jobs

import (
	"context"
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
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// NodeTarget identifies a specific node for job scheduling and lifecycle management.
// When set, the job is tied to this node's identity (named with node suffix, labeled with UID for cleanup).
type NodeTarget struct {
	Name string // Node name for scheduling and job naming
	UID  string // Node UID for job labeling (enables cleanup on node deletion/replacement)
}

// TargetNodesFunc calculates the list of valid target nodes for a job.
// Called by job controller before each attempt to get fresh node state.
type TargetNodesFunc func() ([]*corev1.Node, error)

// JobConfigFunc returns a serialized JSON string representing the job's configuration.
// Used to detect config drift - if the returned value changes, the job needs to be restarted.
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
)

// JobRetryState tracks retry progress for multi-node jobs
type JobRetryState struct {
	mu               sync.Mutex // Protects fields below
	AttemptNumber    int        // Current attempt (1-N)
	NodeIndex        int        // Index of node to try in current attempt
	TargetNodes      []string   // Cached node names from last targetNodesFunc call
	MaxRetryAttempts int        // Maximum attempts before degrading
	LastFailTime     time.Time  // When last failure occurred
	LastJobConfig    string     // Serialized config when job was created (for drift detection)
}

// syncMultiNodeJobState manages the retry state for a multi-node job.
// This should be called before the job hook to ensure state is current.
// It handles:
// - Checking if target nodes changed (resets state)
// - Checking if job config changed (resets state)
// - Detecting failed jobs and incrementing to next node
// - Deleting failed jobs so they can be recreated on next node
// - Setting degraded condition when max retries exhausted
func syncMultiNodeJobState(ctx context.Context, jobName string, targetNodesFunc TargetNodesFunc, jobConfigFunc JobConfigFunc, maxRetryAttempts int, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient) error {
	// Lock the global state map
	retryStateMutex.Lock()
	state, exists := retryState[jobName]
	if !exists {
		// Initialize state for new job
		targetNodes, err := targetNodesFunc()
		if err != nil {
			retryStateMutex.Unlock()
			return fmt.Errorf("failed to get target nodes: %w", err)
		}
		if len(targetNodes) == 0 {
			retryStateMutex.Unlock()
			return fmt.Errorf("no target nodes available for job")
		}

		// Get initial job config
		var initialConfig string
		if jobConfigFunc != nil {
			initialConfig, err = jobConfigFunc()
			if err != nil {
				retryStateMutex.Unlock()
				return fmt.Errorf("failed to get initial job config: %w", err)
			}
		}

		state = &JobRetryState{
			AttemptNumber:    1,
			NodeIndex:        0,
			TargetNodes:      tools.GetNodeNames(targetNodes),
			MaxRetryAttempts: maxRetryAttempts,
			LastJobConfig:    initialConfig,
		}
		retryState[jobName] = state
		retryStateMutex.Unlock()
		klog.Infof("Starting job %s - attempt %d/%d, will try nodes: %v",
			jobName, state.AttemptNumber, state.MaxRetryAttempts, state.TargetNodes)
		return nil
	}
	retryStateMutex.Unlock()

	// Lock this job's state for the rest of the sync
	state.mu.Lock()
	defer state.mu.Unlock()

	// Check if target nodes have changed
	targetNodes, err := targetNodesFunc()
	if err != nil {
		return fmt.Errorf("failed to get target nodes: %w", err)
	}
	if len(targetNodes) == 0 {
		return fmt.Errorf("no target nodes available for job")
	}

	currentTargetNodes := tools.GetNodeNames(targetNodes)
	nodesChanged := !slicesEqual(state.TargetNodes, currentTargetNodes)

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
			klog.Infof("Job %s target nodes changed from %v to %v - resetting retry state and deleting job",
				jobName, state.TargetNodes, currentTargetNodes)
		}
		if configChanged {
			klog.Infof("Job %s config changed - resetting retry state and deleting job. Old: %s, New: %s",
				jobName, state.LastJobConfig, currentConfig)
		}

		state.AttemptNumber = 1
		state.NodeIndex = 0
		state.TargetNodes = currentTargetNodes
		state.LastJobConfig = currentConfig

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
		// Success - clear state and degraded condition
		klog.Infof("Job %s completed successfully", jobName)
		resetJobRetryState(jobName)

		// Clear degraded condition if it was set
		_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    jobName + operatorv1.OperatorStatusTypeDegraded,
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: fmt.Sprintf("Job %s completed successfully", jobName),
		}))
		if err != nil {
			klog.Errorf("Failed to clear degraded condition for %s: %v", jobName, err)
		}

		return nil
	}

	if IsFailed(*existingJob) || IsStopped(*existingJob) {
		// Failed - move to next node
		currentNodeIndex := state.NodeIndex
		klog.Infof("Job %s failed on node index %d - moving to next node", jobName, currentNodeIndex)

		// Increment to next node
		state.NodeIndex++

		// Check if we've exhausted all nodes in this attempt
		if state.NodeIndex >= len(targetNodes) {
			if state.AttemptNumber >= state.MaxRetryAttempts {
				// Exceeded max attempts - set degraded condition and reset to attempt 1
				klog.Warningf("Job %s exhausted all %d attempts (tried %d nodes each), marking degraded",
					jobName, state.MaxRetryAttempts, len(targetNodes))

				// Set degraded condition to indicate job has failed after all retries
				_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
					Type:    jobName + operatorv1.OperatorStatusTypeDegraded,
					Status:  operatorv1.ConditionTrue,
					Reason:  "MaxRetriesExceeded",
					Message: fmt.Sprintf("Job failed after %d attempts across all nodes", state.MaxRetryAttempts),
				}))
				if err != nil {
					klog.Errorf("Failed to set degraded condition for %s: %v", jobName, err)
				}

				// Reset to attempt 1 and continue trying (degraded condition remains set until success)
				state.AttemptNumber = 1
				state.NodeIndex = 0
			} else {
				// Start new attempt
				state.AttemptNumber++
				state.NodeIndex = 0
				klog.Infof("Job %s exhausted all nodes in attempt %d, starting attempt %d/%d",
					jobName, state.AttemptNumber-1, state.AttemptNumber, state.MaxRetryAttempts)
			}
		}

		// Delete the failed job so it can be recreated on next node
		klog.Infof("Deleting failed job %s to retry on node index %d", jobName, state.NodeIndex)
		if err := DeleteAndWait(ctx, kubeClient, jobName, operatorclient.TargetNamespace); err != nil {
			return fmt.Errorf("failed to delete failed job: %w", err)
		}
	}

	// Job is running - nothing to do
	return nil
}

// configureMultiNodeJob configures a job based on current retry state.
// This is a pure function that just reads state and configures the job.
// State management is done by syncMultiNodeJobState.
func configureMultiNodeJob(ctx context.Context, job *batchv1.Job, targetNodesFunc TargetNodesFunc, jobConfigFunc JobConfigFunc, maxRetryAttempts int, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient) error {
	jobName := job.Name

	// Get current state (should have been initialized by syncMultiNodeJobState)
	retryStateMutex.Lock()
	state, exists := retryState[jobName]
	retryStateMutex.Unlock()

	if !exists {
		// State should have been created by syncMultiNodeJobState, but handle gracefully
		if err := syncMultiNodeJobState(ctx, jobName, targetNodesFunc, jobConfigFunc, maxRetryAttempts, kubeClient, operatorClient); err != nil {
			return err
		}
		retryStateMutex.Lock()
		state = retryState[jobName]
		retryStateMutex.Unlock()
	}

	// Get current target nodes
	targetNodes, err := targetNodesFunc()
	if err != nil {
		return fmt.Errorf("failed to get target nodes: %w", err)
	}
	if len(targetNodes) == 0 {
		return fmt.Errorf("no target nodes available for job")
	}

	// Lock state for reading
	state.mu.Lock()
	nodeIndex := state.NodeIndex
	attemptNumber := state.AttemptNumber
	state.mu.Unlock()

	// Validate node index
	if nodeIndex >= len(targetNodes) {
		return fmt.Errorf("invalid node index %d (only %d nodes available)", nodeIndex, len(targetNodes))
	}

	selectedNode := targetNodes[nodeIndex]
	klog.V(4).Infof("Job %s attempt %d/%d: scheduling on node %s (index %d/%d)",
		jobName, attemptNumber, maxRetryAttempts, selectedNode.Name,
		nodeIndex+1, len(targetNodes))

	// Configure job to run on selected node
	job.Spec.Template.Spec.NodeName = selectedNode.Name
	job.Labels["tnf.etcd.openshift.io/attempt"] = fmt.Sprintf("%d", attemptNumber)
	job.Labels["tnf.etcd.openshift.io/node-index"] = fmt.Sprintf("%d", nodeIndex)

	return nil
}

// slicesEqual checks if two string slices have the same elements in the same order
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resetJobRetryState clears the retry state for a job (called on success or when starting fresh)
func resetJobRetryState(jobName string) {
	retryStateMutex.Lock()
	defer retryStateMutex.Unlock()
	delete(retryState, jobName)
	klog.V(2).Infof("Reset retry state for job %s", jobName)
}

// IsControllerRunning checks if a controller is already running for the given job name
func IsControllerRunning(jobName string) bool {
	runningControllersMutex.Lock()
	defer runningControllersMutex.Unlock()
	return runningControllers[jobName]
}

// RunTNFJobController starts a job controller for the specified job type.
//
// Parameters:
//   - nodeTarget: If non-nil, ties the job to this specific node (job name includes node suffix,
//     job is labeled with node UID for cleanup, and pod is scheduled on this node).
//     Use for node-specific jobs like auth and after-setup.
//   - targetNodesFunc: Optional function that returns list of target nodes to try (for multi-node retry logic).
//     Job controller will try nodes serially until one succeeds, calling this function before each attempt.
//     Only used when nodeTarget is nil. Use for cluster-wide jobs like update-setup.
//   - jobConfigFunc: Optional function that returns job configuration as JSON string (for config drift detection).
//     If config changes, job is deleted and recreated with new config.
//   - retries: Number of retries before marking degraded. Meaning depends on job type:
//   - Single-node (nodeTarget): sets backoffLimit (Kubernetes retries on same node)
//   - Multi-node (targetNodesFunc): sets maxRetryAttempts (loop across different nodes, backoffLimit=0)
//   - Pass 0 for no retries
func RunTNFJobController(ctx context.Context, jobType tools.JobType, nodeTarget *NodeTarget, targetNodesFunc TargetNodesFunc, jobConfigFunc JobConfigFunc, retries int, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	nodeNameForLogs := "any"
	var jobNodeName *string
	if nodeTarget != nil {
		nodeNameForLogs = nodeTarget.Name
		jobNodeName = &nodeTarget.Name
	}

	// Check if a controller for this jobType and node is already running
	controllerKey := jobType.GetJobName(jobNodeName)
	runningControllersMutex.Lock()
	if runningControllers[controllerKey] {
		runningControllersMutex.Unlock()
		klog.V(4).Infof("Two Node Fencing job controller for command %q on node %q is already running, skipping duplicate start", jobType.GetSubCommand(), nodeNameForLogs)
		return
	}
	// Mark this controller as running
	runningControllers[controllerKey] = true
	runningControllersMutex.Unlock()

	klog.Infof("Starting Two Node Fencing job controller for command %q on node %q", jobType.GetSubCommand(), nodeNameForLogs)
	tnfJobController := NewJobController(
		jobType.GetJobName(jobNodeName),
		bindata.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		conditions,
		[]factory.Informer{},
		[]JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				// Set job name first so it's available for logging in configuration functions
				job.SetName(jobType.GetJobName(jobNodeName))
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()

				// Configure job based on node target and retry strategy
				if nodeTarget != nil {
					// Single-node job: schedule on node, label with UID, set backoffLimit for retries
					job.Spec.Template.Spec.NodeName = nodeTarget.Name
					job.Labels["node"] = nodeTarget.UID
					job.Spec.BackoffLimit = ptr.To(int32(retries))
				} else if targetNodesFunc != nil {
					// Multi-node job: sync state first (handles transitions), then configure
					// syncMultiNodeJobState manages state transitions based on job status
					if err := syncMultiNodeJobState(ctx, job.Name, targetNodesFunc, jobConfigFunc, retries, kubeClient, operatorClient); err != nil {
						return err
					}
					// Now configure job based on current state (pure function)
					job.Spec.BackoffLimit = ptr.To(int32(0))
					if err := configureMultiNodeJob(ctx, job, targetNodesFunc, jobConfigFunc, retries, kubeClient, operatorClient); err != nil {
						return err
					}
				} else {
					// No specific targeting - use default backoffLimit from yaml
					if retries > 0 {
						job.Spec.BackoffLimit = ptr.To(int32(retries))
					}
				}

				// Set image and command
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()
				return nil
			}}...,
	)
	go func() {
		defer func() {
			runningControllersMutex.Lock()
			delete(runningControllers, controllerKey)
			runningControllersMutex.Unlock()
			klog.Infof("Two Node Fencing job controller for command %q on node %q stopped", jobType.GetSubCommand(), nodeNameForLogs)
		}()
		tnfJobController.Run(ctx, 1)
	}()
}

// RestartJobOrRunController ensures a job controller is running, restarting the job if it already exists.
//
// Parameters:
//   - nodeTarget: If non-nil, ties the job to this specific node (job name includes node suffix,
//     job is labeled with node UID for cleanup, and pod is scheduled on this node).
//     Use for node-specific jobs like auth and after-setup.
//   - targetNodesFunc: Optional function that returns list of target nodes to try (for multi-node retry logic).
//     Job controller will try nodes serially until one succeeds, calling this function before each attempt.
//     Only used when nodeTarget is nil. Use for cluster-wide jobs like update-setup.
//   - jobConfigFunc: Optional function that returns job configuration as JSON string (for config drift detection).
//     If config changes, job is deleted and recreated with new config.
//   - retries: Number of retries before marking degraded. Meaning depends on job type:
//   - Single-node (nodeTarget): sets backoffLimit (Kubernetes retries on same node)
//   - Multi-node (targetNodesFunc): sets maxRetryAttempts (loop across different nodes, backoffLimit=0)
//   - Pass 0 for no retries
func RestartJobOrRunController(
	ctx context.Context,
	jobType tools.JobType,
	nodeTarget *NodeTarget,
	targetNodesFunc TargetNodesFunc,
	jobConfigFunc JobConfigFunc,
	retries int,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	conditions []string,
	existingJobCompletionTimeout time.Duration) error {

	// Determine job name based on node target
	var jobNodeName *string
	if nodeTarget != nil {
		jobNodeName = &nodeTarget.Name
	}
	jobName := jobType.GetJobName(jobNodeName)

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

	// always try to run the controller, CEO might have been restarted
	RunTNFJobController(ctx, jobType, nodeTarget, targetNodesFunc, jobConfigFunc, retries, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, conditions)

	if !jobExists {
		// No existing job - reset retry state to start fresh
		resetJobRetryState(jobName)
		return nil
	}

	// Job exists, wait for completion
	klog.Infof("Job %s already exists, waiting for being stopped", jobName)
	if err := WaitForStopped(ctx, kubeClient, jobName, operatorclient.TargetNamespace, existingJobCompletionTimeout); err != nil {
		return fmt.Errorf("failed to wait for update-setup job %s to complete: %w", jobName, err)
	}

	// Delete the job so the controller can recreate it
	klog.Infof("Deleting existing job %s", jobName)
	if err := DeleteAndWait(ctx, kubeClient, jobName, operatorclient.TargetNamespace); err != nil {
		return fmt.Errorf("failed to delete existing update-setup job %s: %w", jobName, err)
	}

	// Reset retry state when starting fresh after deleting old job
	resetJobRetryState(jobName)

	return nil
}
