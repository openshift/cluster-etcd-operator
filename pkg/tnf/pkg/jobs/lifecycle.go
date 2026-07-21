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

// SchedulableNodesFunc returns nodes where a job CAN BE SCHEDULED (run location).
// Called by job controller before each attempt to get fresh node state.
// The job will round-robin through these nodes on retry.
type SchedulableNodesFunc func() ([]*corev1.Node, error)

// AffectedNodesFunc returns nodes that will be CONFIGURED by a job (operation target).
// Used to check readiness before admitting job - all affected nodes must be Ready.
// For update-setup: nodes in the ConfigMap that will be reconfigured.
// For fencing: nodes whose fencing will be configured.
// Optional - if nil, no affected node readiness check is performed.
type AffectedNodesFunc func() ([]*corev1.Node, error)

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
	retryState = make(map[string]*jobRetryState)
	// retryStateMutex protects access to retryState map
	retryStateMutex sync.Mutex

	// jobBlockedSince tracks when jobs first became blocked due to affected nodes not being ready
	// Map key is job name, value is timestamp when first blocked
	// Used to set TNF<JobName>Blocked condition after 10 minutes
	jobBlockedSince = make(map[string]time.Time)
	// jobBlockedMutex protects access to jobBlockedSince map
	jobBlockedMutex sync.Mutex
)

// jobRetryState tracks retry progress for multi-node jobs
type jobRetryState struct {
	mu               sync.Mutex // Protects fields below
	AttemptNumber    int        // Current attempt (1-N)
	NodeIndex        int        // Index of node to try in current attempt
	TargetNodes      []string   // Cached node names from last targetNodesFunc call
	MaxRetryAttempts int        // Maximum attempts before degrading
	LastFailTime     time.Time  // When last failure occurred
	LastJobConfig    string     // Serialized config when job was created (for drift detection)
}

const (
	// blockedConditionTimeout is how long to wait before setting Blocked condition when nodes are not ready
	blockedConditionTimeout = 10 * time.Minute
)

// manageBlockedCondition manages the Blocked condition for a job based on node readiness.
// Sets TNF<JobName>Blocked after blockedConditionTimeout if nodes remain not ready.
// Clears the condition when all nodes become ready.
// Returns true if nodes are ready, false if blocked.
func manageBlockedCondition(ctx context.Context, jobName string, notReadyNodes []string, operatorClient v1helpers.StaticPodOperatorClient) bool {
	conditionName := tools.ToPascalCase(jobName) + "Blocked"

	if len(notReadyNodes) > 0 {
		// Nodes not ready - track blocked time
		jobBlockedMutex.Lock()
		blockedSince, wasBlocked := jobBlockedSince[jobName]
		if !wasBlocked {
			// First time blocked - record timestamp
			jobBlockedSince[jobName] = time.Now()
			blockedSince = jobBlockedSince[jobName]
			klog.V(4).Infof("Job %s blocked: nodes not ready: %v (will set condition after %v)", jobName, notReadyNodes, blockedConditionTimeout)
		}
		jobBlockedMutex.Unlock()

		// Set condition if blocked long enough
		if time.Since(blockedSince) > blockedConditionTimeout {
			_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    conditionName,
				Status:  operatorv1.ConditionTrue,
				Reason:  "NodesNotReady",
				Message: fmt.Sprintf("Job blocked for %v: nodes not ready: %v", time.Since(blockedSince).Round(time.Second), notReadyNodes),
			}))
			if updateErr != nil {
				klog.Errorf("Failed to set %s condition: %v", conditionName, updateErr)
			}
		}

		return false
	}

	// All nodes ready - clear blocked tracking and condition
	jobBlockedMutex.Lock()
	_, wasBlocked := jobBlockedSince[jobName]
	if wasBlocked {
		delete(jobBlockedSince, jobName)
		klog.V(4).Infof("Job %s unblocked: all affected nodes are ready", jobName)
	}
	jobBlockedMutex.Unlock()

	// Clear condition if it was set
	if wasBlocked {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    conditionName,
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "All affected nodes are ready",
		}))
		if updateErr != nil {
			klog.Errorf("Failed to clear %s condition: %v", conditionName, updateErr)
		}
	}

	return true
}

// checkNodesReadinessAndSetCondition checks if all nodes are Ready and manages the TNF<JobName>Blocked condition.
// Returns (ready=false, nil) if any nodes are not ready (blocks job admission without triggering Degraded).
// Returns (ready=false, err) if there's an error checking nodes.
// Returns (ready=true, nil) if all nodes are ready.
// After 10 minutes of being blocked, sets the TNF<JobName>Blocked condition.
// Clears the condition and blocked tracking when all nodes become ready.
func checkNodesReadinessAndSetCondition(ctx context.Context, nodeNames []string, jobName string, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient) (bool, error) {
	// Collect all not-ready nodes
	var notReadyNodes []string

	for _, nodeName := range nodeNames {
		node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, v1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}

		if !tools.IsNodeReady(node) {
			notReadyNodes = append(notReadyNodes, nodeName)
		}
	}

	// Manage blocked condition based on node readiness
	ready := manageBlockedCondition(ctx, jobName, notReadyNodes, operatorClient)
	if !ready {
		return false, nil // Nodes not ready - return false without error to avoid triggering Degraded
	}

	return true, nil
}

// syncMultiNodeJobState manages the retry state for a multi-node job.
// This should be called before the job hook to ensure state is current.
// It handles:
// - Checking if target nodes changed (resets state)
// - Checking if job config changed (resets state)
// - Detecting failed jobs and incrementing to next node
// - Deleting failed jobs so they can be recreated on next node
// - Setting degraded condition when max retries exhausted
func syncMultiNodeJobState(ctx context.Context, jobName string, schedulableNodesFunc SchedulableNodesFunc, affectedNodesFunc AffectedNodesFunc, jobConfigFunc JobConfigFunc, maxRetryAttempts int, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient) error {
	// Check affected nodes readiness before admitting/retrying job
	if affectedNodesFunc != nil {
		affectedNodes, err := affectedNodesFunc()
		if err != nil {
			return fmt.Errorf("failed to get affected nodes: %w", err)
		}

		// Check readiness and manage blocked condition
		affectedNodeNames := tools.GetNodeNames(affectedNodes)
		ready, err := checkNodesReadinessAndSetCondition(ctx, affectedNodeNames, jobName, kubeClient, operatorClient)
		if err != nil {
			return err
		}
		if !ready {
			// Block job admission - nodes not ready yet
			return nil
		}
	}

	// Lock the global state map
	retryStateMutex.Lock()
	state, exists := retryState[jobName]
	if !exists {
		// Initialize state for new job
		schedulableNodes, err := schedulableNodesFunc()
		if err != nil {
			retryStateMutex.Unlock()
			return fmt.Errorf("failed to get schedulable nodes: %w", err)
		}
		if len(schedulableNodes) == 0 {
			retryStateMutex.Unlock()
			return fmt.Errorf("no schedulable nodes available for job")
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

		state = &jobRetryState{
			AttemptNumber:    1,
			NodeIndex:        0,
			TargetNodes:      tools.GetNodeNames(schedulableNodes),
			MaxRetryAttempts: maxRetryAttempts,
			LastJobConfig:    initialConfig,
		}
		retryState[jobName] = state
		retryStateMutex.Unlock()
		klog.Infof("Starting job %s - attempt %d/%d, will try schedulable nodes: %v",
			jobName, state.AttemptNumber, state.MaxRetryAttempts, state.TargetNodes)
		return nil
	}
	retryStateMutex.Unlock()

	// Lock this job's state for the rest of the sync
	state.mu.Lock()
	defer state.mu.Unlock()

	// Check if schedulable nodes have changed
	schedulableNodes, err := schedulableNodesFunc()
	if err != nil {
		return fmt.Errorf("failed to get schedulable nodes: %w", err)
	}
	if len(schedulableNodes) == 0 {
		return fmt.Errorf("no schedulable nodes available for job")
	}

	currentSchedulableNodes := tools.GetNodeNames(schedulableNodes)
	nodesChanged := !slicesEqual(state.TargetNodes, currentSchedulableNodes)

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

		state.AttemptNumber = 1
		state.NodeIndex = 0
		state.TargetNodes = currentSchedulableNodes
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
			Type:    tools.ToPascalCase(jobName) + operatorv1.OperatorStatusTypeDegraded,
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
		if state.NodeIndex >= len(schedulableNodes) {
			if state.AttemptNumber >= state.MaxRetryAttempts {
				// Exceeded max attempts - set degraded condition and reset to attempt 1
				klog.Warningf("Job %s exhausted all %d attempts (tried %d nodes each), marking degraded",
					jobName, state.MaxRetryAttempts, len(schedulableNodes))

				// Set degraded condition to indicate job has failed after all retries
				_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
					Type:    tools.ToPascalCase(jobName) + operatorv1.OperatorStatusTypeDegraded,
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
func configureMultiNodeJob(ctx context.Context, job *batchv1.Job, schedulableNodesFunc SchedulableNodesFunc, affectedNodesFunc AffectedNodesFunc, jobConfigFunc JobConfigFunc, maxRetryAttempts int, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient) error {
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

	// Get current schedulable nodes
	schedulableNodes, err := schedulableNodesFunc()
	if err != nil {
		return fmt.Errorf("failed to get schedulable nodes: %w", err)
	}
	if len(schedulableNodes) == 0 {
		return fmt.Errorf("no schedulable nodes available for job")
	}

	// Lock state for reading
	state.mu.Lock()
	nodeIndex := state.NodeIndex
	attemptNumber := state.AttemptNumber
	state.mu.Unlock()

	// Validate node index
	if nodeIndex >= len(schedulableNodes) {
		return fmt.Errorf("invalid node index %d (only %d nodes available)", nodeIndex, len(schedulableNodes))
	}

	selectedNode := schedulableNodes[nodeIndex]
	klog.V(4).Infof("Job %s attempt %d/%d: scheduling on node %s (index %d/%d)",
		jobName, attemptNumber, maxRetryAttempts, selectedNode.Name,
		nodeIndex+1, len(schedulableNodes))

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

// RunNodeJobController starts a job controller for a node-specific job (auth, after-setup).
// The job is tied to a specific node: job name includes node suffix, job is labeled with node UID for cleanup,
// and pod is scheduled on this node.
//
// Parameters:
//   - jobType: Type of job to run (auth, after-setup)
//   - nodeTarget: Identifies the target node for scheduling and cleanup
//   - retries: Number of Kubernetes retries on same node (sets backoffLimit). Pass 0 for no retries.
func RunNodeJobController(ctx context.Context, jobType tools.JobType, nodeTarget NodeTarget, retries int, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	jobNodeName := &nodeTarget.Name
	jobName := jobType.GetJobName(jobNodeName)

	// Check if controller already running
	runningControllersMutex.Lock()
	if runningControllers[jobName] {
		runningControllersMutex.Unlock()
		klog.Infof("Node job controller for %q on node %q is already running, skipping duplicate start", jobType.GetSubCommand(), nodeTarget.Name)
		return
	}
	runningControllers[jobName] = true
	runningControllersMutex.Unlock()

	klog.Infof("starting node job controller for %q on node %q", jobType.GetSubCommand(), nodeTarget.Name)

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
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				job.SetName(jobName)
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()

				// Check node readiness before creating job
				ready, err := checkNodesReadinessAndSetCondition(ctx, []string{nodeTarget.Name}, job.Name, kubeClient, operatorClient)
				if err != nil {
					return err
				}
				if !ready {
					klog.V(4).Infof("Skipping job %s creation: node %s not ready", job.Name, nodeTarget.Name)
					return nil
				}

				// Configure node job: schedule on specific node, label with UID, set backoffLimit
				job.Spec.Template.Spec.NodeName = nodeTarget.Name
				job.Labels["node"] = nodeTarget.UID
				job.Spec.BackoffLimit = ptr.To(int32(retries))

				// Set image and command
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()
				return nil
			}}...,
	)

	go func() {
		defer func() {
			runningControllersMutex.Lock()
			delete(runningControllers, jobName)
			runningControllersMutex.Unlock()
			klog.Infof("Node job controller for %q on node %q stopped", jobType.GetSubCommand(), nodeTarget.Name)
		}()
		tnfJobController.Run(ctx, 1)
	}()
}

// RunClusterJobController starts a job controller for a cluster-wide job (setup, update-setup, fencing).
// The job uses round-robin retry logic across schedulable nodes until one succeeds.
//
// Parameters:
//   - jobType: Type of job to run (setup, update-setup, fencing)
//   - schedulableNodesFunc: Function that returns nodes where job CAN BE SCHEDULED (required)
//   - affectedNodesFunc: Optional function that returns nodes that will be CONFIGURED by job.
//     All affected nodes must be Ready before job can be admitted. Pass nil to skip readiness check.
//   - jobConfigFunc: Optional function that returns job config as JSON for drift detection.
//     If config changes, job is deleted and recreated. Pass nil to skip config drift detection.
//   - retries: Number of full round-robin attempts before marking degraded (maxRetryAttempts). Pass 0 for no retries.
func RunClusterJobController(ctx context.Context, jobType tools.JobType, schedulableNodesFunc SchedulableNodesFunc, affectedNodesFunc AffectedNodesFunc, jobConfigFunc JobConfigFunc, retries int, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	jobName := jobType.GetJobName(nil)

	// Check if controller already running
	runningControllersMutex.Lock()
	if runningControllers[jobName] {
		runningControllersMutex.Unlock()
		klog.Infof("Cluster job controller for %q is already running, skipping duplicate start", jobType.GetSubCommand())
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
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				job.SetName(jobName)
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()

				// Sync multi-node job state (handles transitions based on job status)
				if err := syncMultiNodeJobState(ctx, job.Name, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, kubeClient, operatorClient); err != nil {
					return err
				}

				// Check if retry state was created (won't exist if affected nodes not ready)
				retryStateMutex.Lock()
				_, stateExists := retryState[job.Name]
				retryStateMutex.Unlock()

				if !stateExists {
					klog.V(4).Infof("Skipping job %s creation: retry state not initialized", job.Name)
					return nil
				}

				// Configure cluster job: round-robin scheduling, no k8s retries (backoffLimit=0)
				job.Spec.BackoffLimit = ptr.To(int32(0))
				if err := configureMultiNodeJob(ctx, job, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, kubeClient, operatorClient); err != nil {
					return err
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
			delete(runningControllers, jobName)
			runningControllersMutex.Unlock()
			klog.Infof("Cluster job controller for %q stopped", jobType.GetSubCommand())
		}()
		tnfJobController.Run(ctx, 1)
	}()
}

// RestartNodeJobOrRunController ensures a node job controller is running, restarting the job if it already exists.
// Use for node-specific jobs like auth, after-setup, update-setup.
//
// Parameters:
//   - jobType: Type of job to run (auth, after-setup, update-setup)
//   - nodeTarget: Identifies the target node for scheduling and cleanup
//   - retries: Number of Kubernetes retries on same node (sets backoffLimit). Pass 0 for no retries.
//   - existingJobCompletionTimeout: How long to wait for existing job to stop before deleting it
func RestartNodeJobOrRunController(
	ctx context.Context,
	jobType tools.JobType,
	nodeTarget NodeTarget,
	retries int,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	conditions []string,
	existingJobCompletionTimeout time.Duration) error {

	jobName := jobType.GetJobName(&nodeTarget.Name)

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

	// Always run the controller (CEO might have been restarted)
	RunNodeJobController(ctx, jobType, nodeTarget, retries, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, conditions)

	if !jobExists {
		// No existing job - reset retry state to start fresh
		resetJobRetryState(jobName)
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

	return nil
}

// RestartClusterJobOrRunController ensures a cluster job controller is running, restarting the job if it already exists.
// Use for cluster-wide jobs like setup, update-setup, fencing.
//
// Parameters:
//   - jobType: Type of job to run (setup, update-setup, fencing)
//   - schedulableNodesFunc: Function that returns nodes where job CAN BE SCHEDULED (required)
//   - affectedNodesFunc: Optional function that returns nodes that will be CONFIGURED by job
//   - jobConfigFunc: Optional function that returns job config as JSON for drift detection
//   - retries: Number of full round-robin attempts before marking degraded. Pass 0 for no retries.
//   - existingJobCompletionTimeout: How long to wait for existing job to stop before deleting it
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

	// Always run the controller (CEO might have been restarted)
	RunClusterJobController(ctx, jobType, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, conditions)

	if !jobExists {
		// No existing job - reset retry state to start fresh
		resetJobRetryState(jobName)
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

	return nil
}
