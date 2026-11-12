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
  - Exponential backoff retry (5s → 2min cap), with about ten minutes of retry delays; prerequisite waits have separate timeouts
  - Blocks until etcd bootstrap completes and all nodes at stable revision
  - Starts auth, setup, fencing, and after-setup controllers; Jobs run asynchronously
  - Sets `TNFJobControllersDegraded` if controller startup exhausts its retries
- **Exit:** The setup runner marks transition complete after static-container removal reaches a stable revision, before the Job completes

### Runtime Mode (Post-Transition)

Operations after initial setup is complete (operator restart, upgrade, node replacement):

- **Entry Point:** Operator sync loop detects transition already complete
- **Requirements:** A nonempty registered control-plane node set; individual Jobs check execution readiness
- **Behavior:**
  - Idempotent restart support - safe to call repeatedly
  - Setup creation is refused after transition, including after Job deletion or image/spec drift; deferred creation preserves completion status
  - Ensures auth/after-setup controllers running for each node
  - Ensures fencing controller running
- **Use Cases:** Operator restart, upgrade, node replacement, degraded node scenarios

Bootstrap controller startup waits for installation prerequisites. After startup, both modes run Jobs asynchronously.

## Condition Model

The shared job framework supports three condition types, enabled per controller.
Setup enables all three; other framework jobs enable Progressing and Degraded.
Conditions are set on the etcd Operator CR and propagate to the ClusterOperator CR:

**Available Condition**:
- Type format: `<JobName>Available` (e.g., `TNFSetupJobAvailable`)
- Indicates the job has **successfully completed**
- Set when job reaches Complete status
- Set false when an incomplete or failed Job is observed. Job deletion alone does not clear it; skipped application or deferred creation preserves existing status.
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
- Reason: `SyncError`, set by the controller factory when sync returns an error
- **Readiness or no-target blocks**:
  - Set after the blocking condition persists for ten minutes
  - Cleared when the blocking condition resolves, unless retry exhaustion must be preserved
  - Message contains details: "Affected nodes not ready: [master-0]" or "No schedulable nodes available"
- **Execution and controller errors**: failed Jobs, API errors, and configuration-drift deletion errors can report `SyncError` immediately; the readiness timeout does not apply to them
- **Retry exhaustion**:
  - Returns `SyncError` with message "Job failed after maximum retry attempts" immediately when a cycle is exhausted (e.g., two nodes × three attempts)
  - State resets to attempt 1 and retry cycles continue; this shared framework has no exhaustion cooldown
  - The exhaustion message is preserved through retries and deferrals until the Job succeeds

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
RunClusterJobController(ctx, jobType, schedulableNodesFunc, createAdmissionHook, jobConfigFunc, retries, ...)
```

### RestartClusterJobOrRunController

Retained helper for restarting cluster Jobs; the lifecycle manager no longer calls it.
It waits for old execution/deletion, resets retry state, and ensures the controller is running.

## Job Types and Characteristics

**Node-Specific (single-node pattern):**
- **auth:** Sets hacluster password (required before Pacemaker operations)
- **after-setup:** Disables kubelet systemd service (Pacemaker owns it now). After transition, it requires its live node UID and the newest matching generation's completion, bound to that record's UID after Job cleanup.

**Cluster-Wide (multi-node pattern):**
- **setup:** Creates Pacemaker cluster and marks transition complete. Creation admission refuses new Jobs afterward; executor changes preserve running/completed setup. Resetting transition and deleting the Job permits a deliberate rerun. The runner also checks transition.
- **fencing:** Configures STONITH for BMC power control
- **update-setup:** Reconciles Kubernetes membership through recorded attempts outside this shared framework. See [reconciliation.md](reconciliation.md) for its execution, validation, and retry rules.

### Alert-Agent Registration

The setup and update-setup jobs register `tnf-taint-alert` for fencing events and `tnf-untaint-alert`
for node events. Their scripts are delivered asynchronously by MCO under
`/var/lib/pacemaker/alerts/`; registration requires the expected local script to be executable.

- **Initial setup:** Calls `pcs.ConfigureAlerts()` after configuring fencing. If a script is missing
  or not executable, it logs a warning and continues setup, allowing later registration once MCO
  delivers the scripts. Other configuration errors fail the job and use the normal job retry path.
- **Update-setup:** Application and repair call `pcs.ConfigureAlertsWithRetry()` after cluster
  startup and before validation. Matching read-only discovery skips registration. Existing alert IDs
  are skipped; missing scripts are polled every 15 seconds for up to 60 minutes, bounded by the Job's
  15-minute deadline. Other errors return immediately and use the recorded-attempt retry path.

Configuration creates missing registrations with a `30s` execution timeout and the expected event
filter. Existing registrations reached during configuration are updated in place, avoiding a
delete/recreate gap. Update-setup's initial existence check does not validate paths or filters;
the status collector's [periodic reconciliation](lifecycle-manager.md#alert-agent-reconciliation)
repairs those fields and handles registrations deferred during setup when `TNFObservability` is
enabled. The setup and update-setup registration paths predate this feature and remain available
when the gate is disabled. The new alert-agent status reporting, health aggregation, and periodic
repair all require `TNFObservability`. Neither path restores script files; that remains MCO's
responsibility.

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
 │     └─ Create job spec:
 │        - Fetch fresh node from informer (handles node replacement)
 │        - Pin to target node (NodeName fixed assignment)
 │        - Label with node UID (job.Labels["node"] = string(node.UID))
 │        - Set backoffLimit=retries (Kubernetes handles retries)
 │
 ├─ ApplyJob reconciles existing Jobs and waits for earlier Pods
 ├─ Before creation, admission checks the current target's readiness
 │  - Not ready < 10min → Defer creation, preserving status
 │  - Not ready ≥ 10min → Return error (Degraded)
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
- `schedulableNodesFunc`: Returns nodes where job can run (intersection of K8s Ready nodes and Pacemaker members). Changes refresh placement without invalidating running/completed Jobs. See [Lifecycle Manager](lifecycle-manager.md#node-selection-logic) for implementation details.
- `createAdmissionHook`: Called before every create, including replacement/retry. Returns `(false, nil)` to defer without consuming retries or changing status. Setup checks transition/all-node readiness; fencing checks nodes with fencing Secrets.
- `jobConfigFunc`: Returns config string for drift detection (generation, secret ResourceVersions). Changes trigger retry reset.

**Execution Flow:**

```text
RunClusterJobController(jobType, schedulableNodesFunc, createAdmissionHook, jobConfigFunc, retries, ...)
 │
 ├─ Create JobController with getJob hook
 │  │
 │  └─ getJob hook (runs on every sync):
 │     │
 │     ├─ Call syncMultiNodeJobState (manages retry state):
 │     │  │
 │     │  ├─ Check schedulableNodesFunc() → any nodes available?
 │     │  │  ├─ NO → Return error after 10min (triggers Degraded via WithSyncDegradedOnError)
 │     │  │  └─ YES → Continue (Degraded clears naturally when job succeeds/completes)
 │     │  │
 │     │  ├─ Get or initialize retry state (AttemptNumber, NodeIndex, config)
 │     │  │
 │     │  ├─ Check schedulableNodesFunc() → nodes changed?
 │     │  │  └─ YES → Refresh executors and retry cursor; keep running/completed Job
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
 │     ├─ Check if retry state exists (skipped if no execution target)
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
 ├─ ApplyJob reconciles existing Jobs, then waits for earlier Pods before creation
 ├─ Before every create: createAdmissionHook → allow, defer, or error
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

**Drift Detection:**
- Changed `schedulableNodesFunc` output refreshes executors and rebases the retry cursor, retaining its node when still eligible; it does not delete running/completed work or reset the retry budget.
- Changed `jobConfigFunc` output deletes stale work and resets retries. Setup has no configuration callback; fencing includes node/Secret identities.

**ApplyJob Drift Handling:**
`ApplyJob` detects drift between existing and required job specs and handles recreation:
- **Non-failed cluster jobs (completed or running):** Drift in retry fields (NodeName, node-index, attempt labels) is ignored to prevent recreation on operator restart when retry state is reinitialized
- **Non-failed cluster jobs with real drift:** Image/command/config changes trigger recreation even for non-failed jobs
- **Failed cluster jobs:** Any drift (including NodeName from retry) triggers delete/recreate for round-robin retry

**Config String Components:**
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
- **[pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)** - Lifecycle controller sync loop and informer work queue
- **[pkg/tnf/operator/helpers.go](../../pkg/tnf/operator/helpers.go)** - getActivePacemakerNodes(), intersection logic
- **[pkg/tnf/operator/status_collector.go](../../pkg/tnf/operator/status_collector.go)** - Status collector CronJob
- **[pkg/tnf/operator/job_controllers.go](../../pkg/tnf/operator/job_controllers.go)** - Job controller startup logic

**Job Implementations:**
- **[pkg/tnf/setup/runner.go](../../pkg/tnf/setup/runner.go)** - Setup job implementation (creates Pacemaker cluster)
- **[pkg/tnf/update-setup/runner.go](../../pkg/tnf/update-setup/runner.go)** - Update-setup entry point; [recorded.go](../../pkg/tnf/update-setup/recorded.go) implements recorded reconciliation
