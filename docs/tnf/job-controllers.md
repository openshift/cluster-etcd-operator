# TNF Job Controllers

## Overview

Job controllers manage the lifecycle of Kubernetes Jobs that configure and maintain the Pacemaker cluster. The framework provides both single-node and multi-node job execution patterns with automatic retry logic and status tracking.

## Operational Modes

The job controller system operates in two distinct modes based on whether the external etcd transition is complete:

### Bootstrap Mode (Pre-Transition)

Initial cluster setup during first-time installation:

- **Entry Point:** Operator sync loop during startup
- **Requirements:** 
  - Exactly 2 ready control plane nodes (Pacemaker limitation - errors if > 2, waits if < 2)
  - Both nodes must report Ready status
  - etcd bootstrap must complete
  - All nodes must reach latest static pod revision
- **Behavior:**
  - Exponential backoff retry (5s → 2min cap) with 10-minute timeout
  - Blocks until etcd bootstrap completes and all nodes at stable revision
  - Creates auth jobs (per-node), setup job (cluster-wide), fencing job (cluster-wide)
  - Waits for setup job to complete before marking transition complete
  - Sets `TNFJobControllersDegraded` condition if setup fails after 10-minute retry
- **Exit:** Marks external etcd transition complete when setup job succeeds

### Runtime Mode (Post-Transition)

Operations after initial setup is complete (operator restart, upgrade, node replacement):

- **Entry Point:** Operator sync loop detects transition already complete
- **Requirements:** At least 1 ready control plane node
- **Behavior:**
  - Idempotent restart support - safe to call repeatedly
  - Setup job is one-time execution (not recreated), but controller runs to maintain conditions
  - Ensures auth/after-setup controllers running for each node
  - Ensures fencing controller running
- **Use Cases:** Operator restart, upgrade, node replacement, degraded node scenarios

**Key Difference:** Bootstrap mode blocks until setup completes (initial cluster creation), while runtime mode is non-blocking and assumes cluster already exists.

## Condition Model

Job controllers use a three-condition model to track lifecycle and health status. All conditions are set on the etcd Operator CR and propagate to the ClusterOperator CR:

**Available Condition**:
- Type format: `<JobName>Available` (e.g., `TNFSetupJobAvailable`)
- Indicates the job has **successfully completed**
- Set when job reaches Complete status
- Cleared when job is restarted or deleted
- Reason: `JobComplete`

**Progressing Condition**:
- Type format: `<JobName>Progressing` (e.g., `TNFSetupJobProgressing`)
- Indicates the job is **actively running**
- Set when job is created and running
- Cleared when job completes or fails
- Reason: `JobRunning`

**Degraded Condition**:
- Type format: `<JobName>Degraded` (e.g., `TNFSetupJobDegraded`)
- Indicates a job has **failed OR is blocked** from running
- Reasons:
  - `SyncError`: Job blocked (nodes not ready or no schedulable nodes) for >10 minutes
  - `MaxRetriesExceeded`: Job exhausted all retry attempts in current cycle (e.g., 6 tries across 2 nodes × 3 attempts)
- **Blocked jobs** (SyncError):
  - Set after 10-minute timeout when blocking condition persists
  - Automatically cleared when condition resolves (nodes become ready, schedulable nodes available)
  - Message contains details: "Affected nodes not ready: [master-0]" or "No schedulable nodes available"
- **Failed jobs** (MaxRetriesExceeded):
  - Set immediately when max retry attempts exhausted in a cycle
  - **Retry cycles continue** - after setting Degraded, state resets to attempt 1 and retries resume
  - Remains set until the job succeeds

All conditions are set on the **etcd Operator CR** and propagate to the **ClusterOperator CR** via the cluster status controller. This allows per-job granularity on the Operator CR while the ClusterOperator shows overall health state to cluster administrators.

## Job Controller Framework

### RunNodeJobController

Entry point for starting node-specific job controllers. Provides duplicate prevention (safe to call repeatedly) and integrates with the operator's controller framework.

**Characteristics:**
- Checks single node readiness before creating job
- Uses Kubernetes backoffLimit for retries on same node
- Examples: auth, after-setup

**Signature:**
```go
RunNodeJobController(ctx, jobType, node, retries, ...)
```

### RunClusterJobController

Entry point for starting cluster-wide job controllers with multi-node retry logic.

**Characteristics:**
- Round-robin retry across schedulable nodes
- Config drift detection triggers job restart
- Uses backoffLimit=0 with manual retry state management
- Examples: setup, fencing

**Signature:**
```go
RunClusterJobController(ctx, jobType, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, ...)
```

### RestartClusterJobOrRunController

Handles job restart logic with blocking cleanup:

- Deletes existing job and waits for deletion (blocking)
- Starts new job controller after cleanup completes
- Used when job config changes or manual restart needed
- Only used for cluster-wide jobs (node jobs use drift detection instead)

## Job Types and Characteristics

**Node-Specific (single-node pattern):**
- **auth:** Sets hacluster password (required before Pacemaker operations)
- **after-setup:** Disables kubelet systemd service (Pacemaker owns it now)

**Cluster-Wide (multi-node pattern):**
- **setup:** Creates Pacemaker cluster, marks transition complete. Returns success immediately if transition already complete (e.g., job recreated due to drift detection)
- **fencing:** Configures STONITH for BMC power control
- **update-setup:** Handles node replacements (removes offline node, adds new node, updates IPs). Validates: (1) Pacemaker cluster running on scheduled node, (2) exactly 2 nodes online after operations complete. Returns error to trigger retry if validation fails (e.g., auth hasn't run on new node yet)

## Single-Node Job Pattern

Single-node jobs run on a specific target node with Kubernetes-native retry (backoffLimit). Used for node-specific operations like setting auth credentials.

**Execution Flow:**

```text
RunNodeJobController(jobType, node, retries, ...)
 │
 ├─ Create JobController with getJob hook
 │  │
 │  └─ getJob hook (runs on every sync):
 │     │
 │     ├─ Check node readiness via checkNodesReadinessAndSetCondition
 │     │  ├─ Not ready < 10min → Return (skip job, retry on next sync)
 │     │  ├─ Not ready ≥ 10min → Return error (triggers Degraded via WithSyncDegradedOnError)
 │     │  └─ Ready → Continue (Degraded clears naturally when job succeeds/completes)
 │     │
 │     └─ Create job spec:
 │        - Fetch fresh node from informer (handles node replacement)
 │        - Pin to target node (NodeName fixed assignment)
 │        - Label with node UID (job.Labels["node"] = string(node.UID))
 │        - Set backoffLimit=retries (Kubernetes handles retries)
 │
 └─ JobController syncs every minute
    - Complete → Clear conditions, done
    - Failed → Framework sets Degraded condition
```

**Key Characteristics:**
- **No round-robin:** Job always runs on the same target node
- **Node replacement detection:** Detects when node is replaced (same name, new UID) via `node` label, triggers job recreation
- **Graceful cleanup:** When node is deleted, controller skips job application and waits for context cancellation
- **Kubernetes retries:** Uses Job's built-in backoffLimit mechanism

**Node Replacement Handling:**

Node-specific jobs handle node replacement by fetching fresh node data from the node informer on every sync and labeling the job with the node's UID:

```go
// Fetch fresh node from informer to handle node replacement
freshNode, err := nodeLister.Get(node.Name)
if err != nil {
    if apierrors.IsNotFound(err) {
        // Node deleted - skip job application, wait for context cancellation
        return false, nil
    }
    return false, err
}

// Label job with node UID for drift detection
job.Labels["node"] = string(freshNode.UID)
```

This ensures:
- **Node replacement detected:** When a node is replaced (same name, different UID), the `node` label drift is detected by `ApplyJob`, triggering job deletion and recreation
- **Deleted nodes handled gracefully:** When a node is deleted entirely, the controller skips job application instead of erroring repeatedly
- **No stale data:** Fetching from the informer each sync prevents closure capture of stale node objects

## Multi-Node Job Pattern

Multi-node jobs implement round-robin retry across schedulable nodes with drift detection. Used for cluster-wide operations like setup and fencing.

**Function Roles:**
- `schedulableNodesFunc`: Returns nodes where job can run (intersection of K8s Ready nodes and Pacemaker members). Changes trigger retry reset. See [Lifecycle Manager](lifecycle-manager.md#node-selection-logic) for implementation details.
- `affectedNodesFunc`: Returns nodes that must be ready before job runs (can include nodes being added). Job blocked if any not ready.
- `jobConfigFunc`: Returns config string for drift detection (generation, secret ResourceVersions). Changes trigger retry reset.

**Execution Flow:**

```text
RunClusterJobController(jobType, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, ...)
 │
 ├─ Create JobController with getJob hook
 │  │
 │  └─ getJob hook (runs on every sync):
 │     │
 │     ├─ Call syncMultiNodeJobState (manages retry state):
 │     │  │
 │     │  ├─ Check affectedNodesFunc() → any nodes not ready?
 │     │  │  ├─ YES → Return error after 10min (triggers Degraded via WithSyncDegradedOnError)
 │     │  │  └─ NO → Continue (Degraded clears naturally when job succeeds/completes)
 │     │  │
 │     │  ├─ Check schedulableNodesFunc() → any nodes available?
 │     │  │  ├─ NO → Return error after 10min (triggers Degraded via WithSyncDegradedOnError)
 │     │  │  └─ YES → Continue (Degraded clears naturally when job succeeds/completes)
 │     │  │
 │     │  ├─ Get or initialize retry state (AttemptNumber, NodeIndex, config)
 │     │  │
 │     │  ├─ Check schedulableNodesFunc() → nodes changed?
 │     │  │  └─ YES → Reset state (attempt=1, index=0), delete job, return
 │     │  │
 │     │  ├─ Check jobConfigFunc() → config changed?
 │     │  │  └─ YES → Reset state (attempt=1, index=0), delete job, return
 │     │  │
 │     │  ├─ Get current job from cluster
 │     │  │  │
 │     │  │  ├─ Complete? → Return success (Degraded clears naturally via syncManaged flow)
 │     │  │  │
 │     │  │  └─ Failed?
 │     │  │     ├─ Move to next node (NodeIndex++)
 │     │  │     ├─ All nodes tried? → Increment attempt (AttemptNumber++)
 │     │  │     ├─ Max attempts exhausted? → Return error (triggers Degraded via WithSyncDegradedOnError), reset to attempt 1
 │     │  │     └─ Update retry state (ApplyJob will detect retry field drift and delete/recreate)
 │     │  │
 │     │  └─ ApplyJob detects drift (NodeName, node-index, or attempt labels changed) → Deletes and recreates job
 │     │
 │     ├─ Check if retry state exists (skipped if nodes not ready)
 │     │  └─ NO → Return nil (skip job creation, retry on next sync)
 │     │
 │     └─ Call configureMultiNodeJob (configures job with retry state):
 │        │
 │        ├─ Read retry state (NodeIndex, AttemptNumber)
 │        ├─ Get schedulable nodes
 │        ├─ Select node: schedulableNodes[NodeIndex]
 │        └─ Create job spec:
 │           - Schedule on selected node (nodeSelector)
 │           - Set backoffLimit=0 (manual retry only)
 │
 └─ JobController syncs every minute
    - Complete → Clear conditions, done
    - Failed → syncMultiNodeJobState advances to next node
```

**Round-Robin Retry:**
- **Attempt 1:** Try node 0 → fails → try node 1 → fails → continue
- **Attempt 2:** Try node 0 → fails → try node 1 → fails → continue
- **Attempt 3:** Try node 0 → fails → try node 1 → fails → **Degraded**
- **Total:** 6 tries (2 nodes × 3 attempts) with maxRetryAttempts=3
- **After Degraded:** Reset to attempt 1, retry continues (allows recovery)

**Job-Specific Validation:**
Cluster jobs may perform node-specific validation before and after executing their operations. For example, update-setup validates:
1. **Pre-execution:** Pacemaker cluster is running on the scheduled node (`pcs cluster status`). If not, returns error to trigger round-robin retry to find the node where cluster is running.
2. **Post-execution:** Exactly 2 nodes are online in the cluster (`pcs status xml`). If not, returns error to trigger retry, ensuring the job doesn't succeed until the new node is fully online (i.e., auth job has run and node joined cluster).

This allows cluster-wide jobs to find the correct execution context and validate complete state without requiring explicit knowledge at scheduling time.

**Drift Detection:**
When infrastructure changes (nodes added/removed, config updated), retry state resets:
- `schedulableNodesFunc` returns different nodes → Reset (Pacemaker membership changed)
- `jobConfigFunc` returns different config → Reset (infrastructure changed, e.g., secret rotation)
- Old job deleted, new job starts on first node with attempt 1

**ApplyJob Drift Handling:**
`ApplyJob` detects drift between existing and required job specs and handles recreation:
- **Non-failed cluster jobs (completed or running):** Drift in retry fields (NodeName, node-index, attempt labels) is ignored to prevent recreation on operator restart when retry state is reinitialized
- **Non-failed cluster jobs with real drift:** Image/command/config changes trigger recreation even for non-failed jobs
- **Failed cluster jobs:** Any drift (including NodeName from retry) triggers delete/recreate for round-robin retry

**Config String Components:**
- Generation counters (detect config changes)
- Node UIDs (detect node replacements)
- Secret ResourceVersions (detect credential rotation and data updates)

**Key Characteristics:**
- **Round-robin:** Tries all schedulable nodes before giving up
- **Drift detection:** Automatically restarts job when config changes
- **Manual retry:** Uses backoffLimit=0, framework manages retry state
- **Admission control:** Blocks job creation until all affected nodes ready

**Why Round-Robin Instead of Sticky-on-Success:**
Cluster jobs use round-robin retry **only on failure**. Once a job succeeds, it's done—there's no retry needed. This differs from the status collector CronJob which runs continuously and uses sticky-on-success (if a node succeeded last time, it's likely to succeed again). For one-time success jobs, there is no "next time" after success, so retry logic only applies to failures.

## Related Files

**Job Controller Framework:**
- **[pkg/tnf/pkg/jobs/lifecycle.go](../../pkg/tnf/pkg/jobs/lifecycle.go)** - Core job controller framework (RunNodeJobController, RunClusterJobController, retry logic)
- **[pkg/tnf/pkg/jobs/jobcontroller.go](../../pkg/tnf/pkg/jobs/jobcontroller.go)** - JobController type and sync implementation
- **[pkg/tnf/pkg/jobs/utils.go](../../pkg/tnf/pkg/jobs/utils.go)** - Job status helpers and utility functions
- **[pkg/tnf/pkg/tools/jobs.go](../../pkg/tnf/pkg/tools/jobs.go)** - JobType definitions, timeout constants, ToPascalCase

**Lifecycle Manager:**
- **[lifecycle-manager.md](lifecycle-manager.md)** - PacemakerLifecycleManager, node selection, status collector
- **[pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)** - Lifecycle controller sync loop, node event handlers
- **[pkg/tnf/operator/helpers.go](../../pkg/tnf/operator/helpers.go)** - getActivePacemakerNodes(), intersection logic
- **[pkg/tnf/operator/status_collector.go](../../pkg/tnf/operator/status_collector.go)** - Status collector CronJob
- **[pkg/tnf/operator/job_controllers.go](../../pkg/tnf/operator/job_controllers.go)** - Job controller startup logic

**Job Implementations:**
- **[pkg/tnf/setup/runner.go](../../pkg/tnf/setup/runner.go)** - Setup job implementation (creates Pacemaker cluster)
- **[pkg/tnf/update-setup/runner.go](../../pkg/tnf/update-setup/runner.go)** - Update-setup job implementation (node replacement orchestration). Validates Pacemaker cluster running on scheduled node before proceeding, and validates exactly 2 nodes online after completion; returns error to trigger retry otherwise
