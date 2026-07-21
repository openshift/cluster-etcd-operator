# TNF Job Controllers

## Overview

Job controllers manage the lifecycle of Kubernetes Jobs that configure and maintain the Pacemaker cluster. The framework provides both single-node and multi-node job execution patterns with automatic retry logic and status tracking.

## Condition Model: Blocked vs Degraded

Job controllers use a two-condition model to track health status. Both conditions are set on the etcd Operator CR and propagate to the ClusterOperator CR:

**Blocked Condition** (transient):
- Type format: `<JobName>Blocked` (e.g., `TNFSetupJobBlocked`)
- Indicates a job cannot proceed due to **transient issues** (nodes not ready)
- Set after 10-minute timeout when nodes remain not ready
- Automatically cleared when nodes become ready
- Reason: `NodesNotReady`

**Degraded Condition** (permanent failure):
- Type format: `<JobName>Degraded` (e.g., `TNFSetupJobDegraded`)
- Indicates a job has **exhausted all retry attempts** across all nodes
- Set immediately when max retry attempts exhausted
- Remains set until the job succeeds (retries continue with degraded condition active)
- Reason: `MaxRetriesExceeded`

Both conditions are set on the **etcd Operator CR** and propagate to the **ClusterOperator CR** via the cluster status controller. This allows per-job granularity on the Operator CR while the ClusterOperator shows overall degraded state to cluster administrators.

## Job Controller Framework

### RunNodeJobController

Entry point for starting node-specific job controllers. Provides duplicate prevention (safe to call repeatedly) and integrates with the operator's controller framework.

**Characteristics:**
- Checks single node readiness before creating job
- Labeled with node UID for cleanup on node deletion
- Uses Kubernetes backoffLimit for retries on same node
- Examples: auth, after-setup

**Signature:**
```go
RunNodeJobController(ctx, jobType, nodeTarget, retries, ...)
```

### RunClusterJobController

Entry point for starting cluster-wide job controllers with multi-node retry logic.

**Characteristics:**
- Round-robin retry across schedulable nodes
- Config drift detection triggers job restart
- Uses backoffLimit=0 with manual retry state management
- Examples: setup, fencing, update-setup

**Signature:**
```go
RunClusterJobController(ctx, jobType, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc, retries, ...)
```

### RestartNodeJobOrRunController / RestartClusterJobOrRunController

Handle job restart logic with blocking cleanup:

- Deletes existing job and waits for deletion (blocking)
- Starts new job controller after cleanup completes
- Used when job config changes or manual restart needed
- Separate functions for node jobs and cluster jobs

## Job Types and Characteristics

**Node-Specific (single-node pattern):**
- **auth:** Sets hacluster password (required before Pacemaker operations)
- **after-setup:** Disables kubelet systemd service (Pacemaker owns it now)

**Cluster-Wide (multi-node pattern):**
- **setup:** Creates Pacemaker cluster, marks transition complete
- **fencing:** Configures STONITH for BMC power control
- **update-setup:** Adds/removes nodes (Day 2 reconciliation)

## Multi-Node Retry Logic

Multi-node jobs use `schedulableNodesFunc`, `affectedNodesFunc`, and `jobConfigFunc` to implement round-robin retry with drift detection:

**Function Roles:**
- `schedulableNodesFunc`: Returns nodes where job can run (e.g., nodes already in Pacemaker cluster). Changes trigger retry reset.
- `affectedNodesFunc`: Returns nodes that must be ready before job runs (can include nodes being added). Job is blocked if any not ready.
- `jobConfigFunc`: Returns config string for drift detection. Changes trigger retry reset.

**Retry State Tracking:**
Framework maintains internal retry state tracking:
- Current attempt number (1 to maxRetryAttempts)
- Node index for round-robin selection
- Last job config for drift detection

**Node Selection:** Round-robin tries each schedulable node before incrementing attempt:
- Attempt 1: master-0, if master-0 fails then master-1, if master-1 fails then continue
- Attempt 2: master-0, if master-0 fails then master-1, if master-1 fails then continue
- Attempt 3: master-0, if master-0 fails then master-1, if master-1 fails then job has failed
- Total: up to 6 tries (3 attempts × 2 nodes) with maxRetryAttempts=3
- All attempts exhausted → set Degraded condition, reset to attempt 1 (allows recovery)

**Retry State Reset Triggers:**
- `schedulableNodesFunc` returns different nodes → reset to attempt 1 (Pacemaker membership changed)
- `jobConfigFunc` returns different config → reset to attempt 1 (infrastructure changed)

Config typically includes generation counters and secret UIDs. Affected nodes are static within a generation and don't trigger resets.

**State Management:**
`syncMultiNodeJobState` manages retry logic:
1. Check affected nodes readiness → block job if not ready, set "Blocked" condition after 10min
2. Check schedulable nodes changed → reset retry state, delete old job
3. Check config changed → reset retry state, delete old job
4. Check max attempts exhausted → set Degraded condition, reset state (recovery path)

## Job Controller State Machine

### syncMultiNodeJobState Flow

Called before job creation/update to manage retry state:

```text
syncMultiNodeJobState(jobName, schedulableNodesFunc, affectedNodesFunc, jobConfigFunc)
 │
 ├─ Check affectedNodesFunc() → any nodes not ready?
 │  ├─ YES: Block job, set "Blocked" condition after 10min, return nil
 │  │  (NO retry state created - job creation will be skipped)
 │  └─ NO: Clear "Blocked" condition, continue
 │
 ├─ Get or initialize retry state
 │
 ├─ Check schedulableNodesFunc() → nodes changed?
 │  └─ YES: Reset state (attempt=1, index=0), delete old job, return nil
 │
 ├─ Check jobConfigFunc() → config changed?
 │  └─ YES: Reset state (attempt=1, index=0), delete old job, return nil
 │
 ├─ Get current job from cluster
 │
 ├─ Job exists and failed?
 │  ├─ Round-robin to next node (NodeIndex++)
 │  ├─ If all nodes tried: increment attempt (AttemptNumber++)
 │  └─ Max attempts exhausted? → Set Degraded, reset to attempt 1
 │
 └─ Return nil (retry state exists, job can be created)
```

### configureMultiNodeJob Flow

Pure function that configures job based on current retry state. Only called when retry state exists (meaning affected nodes are ready):

```text
configureMultiNodeJob(job, schedulableNodesFunc, ...)
 │
 ├─ Get schedulable nodes from schedulableNodesFunc()
 │
 ├─ Read retry state (NodeIndex, AttemptNumber)
 │
 ├─ Select node: schedulableNodes[NodeIndex]
 │
 └─ Configure job to run on selected node
```

**Job admission control:**
1. getJob hook calls `syncMultiNodeJobState`
2. If affected nodes not ready → no retry state created
3. Hook checks if retry state exists
4. No state → return nil (skip job creation, retry on next sync)
5. State exists → call `configureMultiNodeJob` to create job

### Single-Node Job Flow

Single-node jobs use simpler logic via `checkNodesReadinessAndSetCondition`:

```text
RunNodeJobController(jobType, nodeTarget, ...)
 │
 ├─ Create JobController with getJob hook
 │  │
 │  └─ getJob hook:
 │     ├─ Check node readiness via checkNodesReadinessAndSetCondition
 │     │  ├─ Not ready < 10min? → Return (false, nil) to skip job creation, retry on next sync
 │     │  ├─ Not ready >= 10min? → Set Blocked condition, return (false, nil)
 │     │  │  (Blocked propagates to Degraded on ClusterOperator)
 │     │  └─ Ready? → Clear Blocked condition, return (true, nil)
 │     │
 │     └─ If ready, create job spec (scheduled on target node, labeled with UID)
 │
 └─ JobController framework syncs every minute
```

## Job Configuration and Drift Detection

Multi-node jobs use `jobConfigFunc` to detect infrastructure changes that require job restart. Returns a config string capturing all inputs that affect job behavior. When the config changes, the framework automatically:
1. Resets retry state (prevents acting on stale decisions)
2. Deletes the old job
3. Creates a new job on next sync

**Common config components:**
- Generation counters (detect config changes)
- Node UIDs (detect node replacements)
- Secret UIDs (detect credential rotation)

When any component changes, `syncMultiNodeJobState` detects the drift and triggers job recreation.

## Related Files

- **[pkg/tnf/pkg/jobs/lifecycle.go](../../pkg/tnf/pkg/jobs/lifecycle.go)** - Core job controller framework (RunNodeJobController, RunClusterJobController, retry logic)
- **[pkg/tnf/pkg/jobs/jobcontroller.go](../../pkg/tnf/pkg/jobs/jobcontroller.go)** - JobController type and sync implementation
- **[pkg/tnf/pkg/jobs/utils.go](../../pkg/tnf/pkg/jobs/utils.go)** - Job status helpers and utility functions
- **[pkg/tnf/pkg/tools/jobs.go](../../pkg/tnf/pkg/tools/jobs.go)** - JobType definitions, timeout constants, ToPascalCase
- **[pkg/tnf/setup/runner.go](../../pkg/tnf/setup/runner.go)** - Setup job implementation (creates Pacemaker cluster)
