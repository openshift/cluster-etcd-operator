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
  - Exponential backoff retry (5s → 2min cap), with roughly ten minutes of retry delays; prerequisite waits have their own timeouts
  - Blocks until etcd bootstrap completes and all nodes at stable revision
  - Starts auth, setup, fencing, and after-setup controllers; Job execution proceeds asynchronously
  - Sets `TNFJobControllersDegraded` if controller startup exhausts its backoff
- **Exit:** The setup runner marks the transition complete after static-container removal reaches a stable revision, before the Job reports completion

### Runtime Mode (Post-Transition)

Operations after initial setup is complete (operator restart, upgrade, node replacement):

- **Entry Point:** Operator sync loop detects transition already complete
- **Requirements:** A nonempty registered control-plane node set to start controllers; each Job checks readiness/eligible execution targets at admission.
- **Behavior:**
  - Idempotent restart support - safe to call repeatedly
  - Setup configuration drift may delete its Job; creation admission denies replacement after transition and preserves existing completion status
  - Ensures auth/after-setup controllers running for each node
  - Ensures fencing controller running
- **Use Cases:** Operator restart, upgrade, node replacement, degraded node scenarios

Bootstrap controller startup waits for installation prerequisites. After startup,
both modes run Jobs asynchronously; the durable transition selects the runtime
dependencies and generation reconciliation.

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

Recorded update-setup uses its own persisted retry state: it reports reason
`MaxRetriesExceeded` after six attempts and allows a renewed budget after ten
minutes. It does not use the shared framework's in-memory retry cycle.

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

This retained cluster-job helper provides blocking restart behavior; the current
lifecycle manager does not call it:

- Waits for an existing Job to stop, then deletes it and waits for deletion
- Resets retry state and ensures its controller is running after cleanup
- Starts the controller directly when no Job exists

## Job Types and Characteristics

**Node-Specific (single-node pattern):**
- **auth:** Sets hacluster password (required before Pacemaker operations)
- **after-setup:** Disables kubelet systemd service (Pacemaker owns it now)

**Cluster-Wide (multi-node pattern):**
- **setup:** Creates the initial Pacemaker cluster. Its creation admission hook refuses new Jobs after the external etcd transition completes; its runner retains the defensive transition check.
- **fencing:** Configures STONITH for BMC power control
- **update-setup:** Uses immutable recorded generations and serialized attempts managed by the lifecycle controller. Desired membership includes registered NotReady nodes. See [recorded generations](lifecycle-manager.md#generation-records-and-jobs).

## Single-Node Job Pattern

Single-node jobs run on a specific target node with Kubernetes-native retry (backoffLimit). Used for node-specific operations like setting auth credentials.

**Execution Flow:**

1. On each sync, fetch the current node identity and build the Job specification.
2. Reconcile existing Jobs, including deletion for genuine UID/spec drift.
3. Before creation, wait for nonterminal Pods of the previous Job name.
4. Invoke creation admission: the node must still exist and be Ready. The
   existing readiness timeout reports blocked nodes after ten minutes.
5. Create only when admitted. Kubernetes handles Pod retries through backoffLimit.

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

- `schedulableNodesFunc` returns eligible execution targets. Placement changes
  refresh the retry cursor; a job includes executor identities in its explicit
  configuration when their changes must invalidate execution.
- `createAdmissionHook` returns `(bool, error)` immediately before each API
  create. `false, nil` defers creation without reporting failure. A nil hook
  permits creation. Existing Jobs are still reconciled while admission is denied.
- `jobConfigFunc` returns the deterministic configuration fingerprint. Changes
  delete stale work and reset retries before processing its failure. Setup
  includes eligible executor identities; fencing includes node/Secret identities.

**Execution Flow:**

```text
sync -> compute placement/configuration and reconcile retry state
     -> changed configuration? delete stale Job and reset retry state
     -> build proposed Job and reconcile any existing Job
     -> creation needed?
          -> wait for surviving Pods of the previous execution
          -> call job-specific creation admission hook
               -> false: preserve status and wait for a later sync
               -> error: report reconciliation failure
               -> true: create the Job
```

Admission is checked for initial creation, drift replacement, and failed-attempt
retries. A denied creation does not reset/consume the retry budget. Creation
admission is separate from the older `JobHookFunc`, whose mutation/skip hooks run
on every sync before application. The readiness helper `AdmitReadyNodes` can be
called from any job's admission function; the framework no longer accepts a
separate affected-nodes callback.

**Round-Robin Retry:**
- **Attempt 1:** Try node 0 → fails → try node 1 → fails → continue
- **Attempt 2:** Try node 0 → fails → try node 1 → fails → continue
- **Attempt 3:** Try node 0 → fails → try node 1 → fails → **Degraded**
- **Total:** 6 tries (2 nodes × 3 attempts) with maxRetryAttempts=3
- **After Degraded:** Reset to attempt 1, retry continues (allows recovery)

## Job configuration and drift

The framework fetches current node identity rather than relying on a captured
Node. Node-specific Job labels include that UID, and after-setup also receives
`TNF_NODE_UID` for its runner-side dependency check. A replacement with the same
name triggers Job drift. A deleted node requests a skip rather than repeated
Job recreation.

Cluster jobs compare schedulable-node and configuration inputs, including fencing
node/Secret identity. Secret changes can restart fencing independently of
membership repair. Retry labels distinguish scheduling attempts from actual
configuration drift; existing completed-Job and degradation behavior remains in
the framework.

Executor-list changes update the cached candidates and rebase the retry cursor,
retaining the selected node when it remains eligible. They do not delete the
current Job or restart its retry budget. Running and completed cluster Jobs are
also preserved by ApplyJob's existing retry-field comparison.

When an attempt fails, it uses the latest candidate set. If its executor left
that set, the next attempt starts with the first remaining eligible node. This
also applies when candidates changed while the attempt was still running.
Placement refresh alone does not consume a retry round.

Configuration changes retain the reset/recreation path. Fencing includes
registered node UIDs and Secret versions. Setup explicitly includes eligible
executor identities so stale placement can invalidate an unfinished attempt.
Its creation hook checks the durable transition condition before readiness.
Deletion of completed setup is permitted, but no replacement is created while
that condition is true. Creation deferral preserves existing Available and
Progressing status; resetting the condition and deleting setup permits rerun.

Recorded update-setup persists its executor configuration and retry boundary in
mutable metadata on the immutable membership record. For unfinished generations,
executor changes cancel stale attempts immediately and give the replacement a fresh budget, without
changing the membership generation or partial-repair checkpoint. The replacement
still waits for old Pods to stop and must pass control-plane MCO creation admission.
Completed matching generations keep their completion across executor changes or
Job deletion. New desired input or confirmed membership drift records a new
generation; unavailable observations after unchanged completed work can instead
record a read-only discovery generation.

Auth and after-setup retain their existing per-node UID/spec behavior. Auth also
consumes peer addresses at runtime; explicit peer-input invalidation remains a
separate policy decision.

The shared JobController contains no setup-name or transition-specific policy.
Job definitions provide creation admission; no pre-delete hook is needed.

## Recorded update-setup validation

Recorded update-setup jobs require an explicit snapshot name and UID and revalidate
live node identities before mutation. Normal application and repair validate the
exact configured member set with every desired node online. A discovery of
unchanged completed input returns without mutation or an online requirement when
live membership matches, UID checks find no replacement, and no repair remains
in progress.
Actual drift found during discovery uses the normal repair path. These Jobs use
generation-specific persisted attempts rather than this framework's in-memory
retry state. The existing framework continues to manage auth, initial setup,
after-setup, and fencing.

## Related Files

**Job Controller Framework:**
- **[pkg/tnf/pkg/jobs/lifecycle.go](../../pkg/tnf/pkg/jobs/lifecycle.go)** - Core job controller framework (RunNodeJobController, RunClusterJobController, retry logic)
- **[pkg/tnf/pkg/jobs/jobcontroller.go](../../pkg/tnf/pkg/jobs/jobcontroller.go)** - JobController type and sync implementation
- **[pkg/tnf/pkg/jobs/utils.go](../../pkg/tnf/pkg/jobs/utils.go)** - Job status helpers and utility functions
- **[pkg/tnf/pkg/tools/jobs.go](../../pkg/tnf/pkg/tools/jobs.go)** - JobType definitions, timeout constants, ToPascalCase

**Lifecycle Manager:**
- **[lifecycle-manager.md](lifecycle-manager.md)** - PacemakerLifecycleManager, node selection, status collector
- **[pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)** - Lifecycle controller work queue and bootstrap/runtime startup
- **[pkg/tnf/operator/helpers.go](../../pkg/tnf/operator/helpers.go)** - getActivePacemakerNodes(), intersection logic
- **[pkg/tnf/operator/status_collector.go](../../pkg/tnf/operator/status_collector.go)** - Status collector CronJob
- **[pkg/tnf/operator/job_controllers.go](../../pkg/tnf/operator/job_controllers.go)** - Job controller startup logic

**Job Implementations:**
- **[pkg/tnf/setup/runner.go](../../pkg/tnf/setup/runner.go)** - Setup job implementation (creates Pacemaker cluster)
- **[pkg/tnf/update-setup/runner.go](../../pkg/tnf/update-setup/runner.go)** - Update-setup entry point. [recorded.go](../../pkg/tnf/update-setup/recorded.go) implements recorded-generation reconciliation and validation
