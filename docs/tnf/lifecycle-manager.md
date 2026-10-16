# Controller Lifecycle

## Overview

The PacemakerLifecycleManager is a Kubernetes controller that runs continuously as part of the cluster-etcd-operator. It manages TNF job controller startup and provides shared logic for determining which nodes are valid targets for Pacemaker operations.

## Responsibilities

The lifecycle manager ([pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)) handles:

1. **Job Controller Startup** - Starts job controllers when conditions are met (bootstrap or runtime mode)
2. **Node Selection** - Provides `schedulableNodesFunc` for job controllers (intersection logic)
3. **Status Collection** - Runs CronJob to collect Pacemaker status and populate PacemakerCluster CR
4. **Bootstrap Detection** - Responds to node Ready transitions during initial cluster setup

## Startup Sequence

```text
┌──────────────────────────────────────────────────────────────┐
│ cluster-etcd-operator starts                                 │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Static resource controller     │
        │ applies PacemakerCluster CRD   │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ runPacemakerControllers()      │
        │ (background goroutine)         │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Wait for CRD to be established │
        │ (polls with backoff)           │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ newPacemakerLifecycleManager() │
        │ - Creates REST client          │
        │ - Creates PacemakerCluster     │
        │   informer (resync: 1min)      │
        │ - Registers node event handlers│
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Start PacemakerCluster informer│
        │ (go pacemakerInformer.Run())   │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Start lifecycle controller     │
        │ (go lifecycleController.Run()) │
        │ Syncs every 1 minute           │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Start status collector CronJob │
        │ (runs every 1 minute)          │
        └────────────────────────────────┘
```

**Key Points:**
- Lifecycle manager starts regardless of external etcd transition state
- The `sync()` function handles both bootstrap and runtime modes internally
- CRD must be established before informers can start
- Controller syncs every 1 minute to ensure job controllers are running

## Sync Loop

The controller's `sync()` function runs every 1 minute and on informer events:

```text
sync() called (every 1 minute + on events)
        │
        ▼
startJobControllers()
        │
        ├─ Check: External etcd transition complete?
        │
        ├─ NO (Bootstrap Mode):
        │  │
        │  ├─ Wait for exactly 2 ready control plane nodes
        │  ├─ Wait for etcd bootstrap to complete
        │  ├─ Start: auth jobs (per-node)
        │  ├─ Start: setup job (cluster-wide, one-time)
        │  ├─ Start: fencing job (cluster-wide)
        │  └─ Wait for setup job completion → mark transition complete
        │
        └─ YES (Runtime Mode):
           │
           ├─ Ensure auth/after-setup controllers running (per-node)
           ├─ Ensure update-setup controller running (if 2 nodes)
           └─ Ensure fencing controller running (cluster-wide)
```

See [Job Controllers](job-controllers.md) for details on job execution patterns and retry logic.

## Node Selection Logic

### getActivePacemakerNodes()

Job controllers need to know which nodes are valid targets for Pacemaker operations. The `getActivePacemakerNodes()` function (in [pkg/tnf/operator/helpers.go](../../pkg/tnf/operator/helpers.go)) provides this logic:

```text
Get all K8s control plane nodes
        │
        ▼
Filter to Ready nodes only
        │
        ▼
Try to get Pacemaker nodes from PacemakerCluster CR
        │
        ▼
CR exists and fresh (age <= 5min)?
    │
    ├─ YES:
    │   │
    │   ├─ Calculate intersection (K8s Ready ∩ Pacemaker)
    │   │
    │   └─ Intersection not empty?
    │       ├─ YES → Return intersection (sorted by name)
    │       └─ NO  → Fall back to all ready K8s nodes
    │
    └─ NO (CR missing or stale):
        │
        └─ Return all ready K8s nodes (sorted by name)
```

**Staleness Check:**
- PacemakerCluster CR is considered stale if `Status.LastUpdated` age is > 5 minutes
- Stale CR indicates status collector isn't running or Pacemaker isn't responding
- Graceful degradation: fall back to all ready nodes rather than blocking operations

**Intersection Logic:**
- Normal operation: Jobs only run on nodes in BOTH Kubernetes AND Pacemaker
- Prevents targeting nodes that haven't joined Pacemaker yet
- Prevents targeting nodes that have left Pacemaker but still exist in K8s
- Graceful degradation (stale/missing CR): Falls back to all ready K8s nodes, waiving the both-systems requirement to prevent blocking operations

**Deterministic Ordering:**
- Nodes are always sorted by name before returning (in all code paths)
- Critical for round-robin retry logic (NodeIndex must point to same node across syncs)
- Sorting happens directly in `getActivePacemakerNodes()` and `getIntersection()`

## Status Collector CronJob

### Overview

The PacemakerCluster CR is populated by a status collector CronJob that runs every minute:

```text
Every 1 minute:
        │
        ▼
┌────────────────────────────────┐
│ Status Collector Job           │
│ (pod scheduled on one node)    │
└────────────────────────────────┘
        │
        ▼
Run: sudo -n pcs status xml
        │
        ▼
Parse XML into structured data
        │
        ▼
Update PacemakerCluster CR .status:
- state (online/offline/standby)
- lastUpdated (timestamp)
- nodes[] (name, status, IP)
```

### Node Rotation Strategy

The status collector uses **sticky-on-success, rotate-on-failure** node selection to maximize success while providing automatic failover:

**Implementation:**
- Maintains in-memory `JobRetryState` with `NodeIndex`
- Nodes are sorted deterministically by name
- Strategy:
  - **Job succeeded** → Keep same node (sticky behavior, minimize overhead)
  - **Job failed** → Rotate to next node in sorted list
  - **Node list changed** → Reset to first node

**Example Flow (2 nodes: master-0, master-1):**
```text
Sync 1: No job exists → Schedule on master-0 (index 0)
Sync 2: Job succeeded → Stay on master-0
Sync 3: Job succeeded → Stay on master-0
Sync 4: Job failed    → Rotate to master-1 (index 1)
Sync 5: Job succeeded → Stay on master-1
Sync 6: Job succeeded → Stay on master-1
```

**Failure Detection:**
- Detects jobs with `Failed` or `FailureTarget` conditions (status=True)
- `FailureTarget` is a newer Kubernetes condition (1.31+) for jobs exceeding `activeDeadlineSeconds`
- Failed jobs are automatically deleted after rotation state is updated
- Deletion unblocks the CronJob's `concurrencyPolicy: Forbid` to allow next run

**CronJob Concurrency:**
- Uses `concurrencyPolicy: Forbid` to prevent overlapping jobs
- When a job fails (especially with FailureTarget on a node with kubelet issues):
  1. CronJob hook detects failure condition
  2. Updates rotation state to next node
  3. Deletes failed job (with background propagation policy)
  4. Next CronJob schedule creates new job on rotated node
- Without deletion, Forbid policy would block new jobs until terminating pods cleanup (can be indefinite if kubelet down)

**Why This Strategy:**
- Minimizes unnecessary job churn (don't rotate on success)
- Automatic failover when node has issues (rotate on failure)
- No retry budget exhaustion (status collector doesn't set Degraded)
- Staleness detection happens via CR timestamp check instead

See [pkg/tnf/operator/status_collector.go](../../pkg/tnf/operator/status_collector.go) for implementation.

## Node Event Handlers

The lifecycle manager registers an `UpdateFunc` event handler on the node informer to trigger update-setup job restarts:

### UpdateFunc (Node Ready Transition)

```text
Node updated
        │
        ▼
Not Ready → Ready transition?
        │
    ┌───┴───┐
    No      Yes
    │       │
    ▼       ▼
 Ignore  Trigger: restartUpdateSetupJob()
            │
            ▼
         (Goroutine spawned)
            │
            ▼
         Check transition complete?
            │
        ┌───┴───┐
        No      Yes (post-transition only)
        │       │
        ▼       ▼
     Skip    Check 2 ready nodes?
                │
            ┌───┴───┐
            No      Yes
            │       │
            ▼       ▼
         Skip    Restart update-setup job
```

**Why Goroutine?**
- Event handlers must return quickly
- Job restart involves blocking cleanup (delete, wait for deletion)
- Prevents blocking the informer event loop

**Use Case:**
- Node becomes ready after replacement → update-setup job is restarted to handle node replacement operations
- Without this event handler, update-setup would remain in Complete state even if it finished before auth ran on the new node
- The restart ensures update-setup reruns its validation (expects exactly 2 online nodes in Pacemaker)

## Integration with Job Controllers

The lifecycle manager provides `schedulableNodesFunc` to job controllers:

```go
schedulableNodesFunc := func() ([]*corev1.Node, error) {
    return lifecycleManager.getActivePacemakerNodes()
}
```

This function is passed to:
- `RunClusterJobController()` for setup, update-setup, and fencing jobs
- Status collector CronJob for node pinning

Job controllers call this function to:
1. Determine which nodes to target for round-robin retry
2. Detect node list changes (triggers retry state reset)
3. Ensure jobs only run on valid Pacemaker members

See [Job Controllers - Multi-Node Job Pattern](job-controllers.md#multi-node-job-pattern) for how `schedulableNodesFunc` integrates with retry logic.

## Related Files

- **[pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)** - PacemakerLifecycleManager controller, sync loop, node event handlers
- **[pkg/tnf/operator/helpers.go](../../pkg/tnf/operator/helpers.go)** - getActivePacemakerNodes(), intersection logic, staleness checking, node sorting
- **[pkg/tnf/operator/status_collector.go](../../pkg/tnf/operator/status_collector.go)** - Status collector CronJob, node rotation logic
- **[pkg/tnf/operator/job_controllers.go](../../pkg/tnf/operator/job_controllers.go)** - startJobControllers(), bootstrap vs runtime mode
- **[pkg/tnf/pkg/tools/nodes.go](../../pkg/tnf/pkg/tools/nodes.go)** - Node helpers (IsNodeReady, GetNodeNames, StringSlicesEqual, ListNodesFromInformer)
