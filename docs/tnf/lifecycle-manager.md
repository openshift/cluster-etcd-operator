# Controller Lifecycle

## Overview

The PacemakerLifecycleManager is a Kubernetes controller that runs continuously as part of the cluster-etcd-operator. It manages TNF job controller startup and provides shared logic for determining which nodes are valid targets for Pacemaker operations.

## Responsibilities

The lifecycle manager ([pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)) handles:

1. **Job Controller Startup** - Starts job controllers when conditions are met (bootstrap or runtime mode)
2. **Node Selection** - Provides `schedulableNodesFunc` for job controllers (intersection logic)
3. **Status Collection** - Runs CronJob to collect Pacemaker status and populate PacemakerCluster CR
4. **Informer Events** - Enqueues lifecycle work when observed resources change
5. **Membership Reconciliation** - Records Kubernetes node identities and serializes update-setup attempts; see [reconciliation.md](reconciliation.md)

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
        │ - Enqueues informer events     │
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
- If the CRD is unreadable, unestablished, or its served schema does not match the gate, CEO logs a warning after five minutes and every five minutes thereafter until it is ready.
- Controller syncs every 1 minute to ensure job controllers are running
- Lifecycle-manager construction failures set `PacemakerLifecycleManagerDegraded` while startup retries every 30 seconds; the condition clears after construction succeeds. The ClusterOperator's Degraded condition applies its existing 10-minute inertia.

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
        │  └─ Start after-setup jobs; setup runner marks transition
        │
        └─ YES (Runtime Mode):
           │
           ├─ Ensure auth/after-setup controllers running (per-node)
           ├─ Ensure setup job controller running (maintains conditions)
           ├─ Ensure fencing controller running (cluster-wide)
           ├─ Start status collector CronJob
           └─ Start Pacemaker health check controller
```

After controller startup, sync cleans orphaned Jobs/Pods and reconciles recorded
update-setup work. See [reconciliation.md](reconciliation.md) for membership,
attempts, and cleanup, and [Job Controllers](job-controllers.md) for the shared
framework.

## Node Selection Logic

This selects execution targets. Desired membership includes every registered
control-plane Node, including NotReady nodes; see
[reconciliation.md](reconciliation.md#desired-state-and-records).

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

The PacemakerCluster CR is populated by a status collector CronJob that runs every minute.
`TNFObservability` controls alert-agent status, its contribution to health, and periodic repairs.
When disabled, the collector omits alert-agent status (including previous entries) and skips repairs.
The PacemakerCluster CRD remains available; CEO selects its schema to match the gate and waits for
that schema before starting Pacemaker controllers. Strict field validation rejects unsupported fields.
CRD selection and controller wiring are fixed at CEO startup; a resolved gate change (including a
`CustomNoUpgrade` override) makes CEO exit and restart to apply the new selection.
The static-resource controller creates or updates the same CRD in place, replacing its schema
with the enabled or disabled variant during updates and upgrades as well as initial installation.
The healthcheck and console notification readers use the same resolved gate and skip alert-agent
entries when it is disabled. Existing setup-time alert registration remains independent of this gate.

Each run resolves the gate from `FeatureGate/cluster.status.featureGates` for its own
`OPERATOR_IMAGE_VERSION`, preserving release-specific behavior during upgrades. Manual/development
runs fall back to ClusterVersion's desired version or latest history entry. A gate absent from a
resolved release entry is disabled. API errors or a missing release entry leave the gate unresolved:
the collector logs a warning and refreshes base status while retaining existing alert-agent
observations and their contribution to node and cluster health. It skips new alert-agent observations
and repairs until the gate can be resolved. Nodes without previous alert-agent entries continue to
report base status only. If the CR cannot be read, collection fails without overwriting status.
Gate lookups have a five-second timeout within the collector's deadline so a lookup failure leaves
time for base-status collection.

When gate resolution fails, nodes with retained alert-agent observations also report
`AlertAgentsObserved=False` with reason `FeatureGateUnresolved`. This optional condition is a
freshness signal, not a health failure: it shows that the updated `status.lastUpdated` describes
base Pacemaker status, not refreshed alert-agent observations. Its transition time records the
start of the unresolved period and is preserved across repeated failures. Agent conditions and their
transition times are retained; those times describe health transitions, not observation freshness.
The freshness condition is omitted once the gate resolves. Individual observation failures then
use the normal carry-forward/Pending behavior.

The collection flow is:

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
Observe alert registration and per-node MCO state
(when TNFObservability is enabled)
        │
        ▼
Update PacemakerCluster CR .status:
- state (online/offline/standby)
- lastUpdated (timestamp)
- nodes[] (name, status, IP)
- alert-agent conditions (when enabled)
        │
        ▼
Read fresh CIB configuration and reconcile alerts
(when TNFObservability is enabled)
        │
        ▼
Next collection confirms recovery in status
```

### Alert-Agent Reconciliation

With `TNFObservability` enabled, the collector reports `nodes[].alertAgents` and the
`AlertAgentsHealthy` node condition. Each agent has:

- `Configured`: CIB registration matches the expected script path and fencing/node selectors.
- `Available`: False only when the node's Machine Config Operator (MCO) state is `Degraded` and
  its reason names the script path; otherwise True for a successfully read node. This is an
  indirect signal, not a filesystem check.
- `Healthy`: combines these signals. Confirmed failures affect node and cluster health;
  unknown observations alone do not. Failed observations retain previous conditions or become
  Unknown/Pending; agents with no observations or previous entry remain omitted.

After updating status, [`pcs.ReconcileAlerts()`](../../pkg/tnf/pkg/pcs/alertreconcile.go) reads fresh
configuration from Pacemaker's Cluster Information Base (CIB) and repairs these registrations:

| Alert ID | Expected script under `/var/lib/pacemaker/alerts/` | Expected event selection |
| --- | --- | --- |
| `tnf-taint-alert` | `tnf-taint-alert.sh` | Fencing events (`<select_fencing/>`) |
| `tnf-untaint-alert` | `tnf-untaint-alert.sh` | Node events (`<select_nodes/>`) |

Repairs recreate missing registrations with a `30s` execution timeout, correct script paths in place,
and add or replace event filters. Before each repair, the expected script must be executable on the
collector's host (`test -x`). MCO remains responsible for script delivery, contents, and permissions.
Periodic repairs do not correct timeout-only drift, validate script execution, or replay missed events.

Repairs share a 10-second timeout within the collector's 30-second deadline. Errors are logged and
retried on the next collection without failing the Job or triggering node rotation; the next
collection also confirms recovery in status. Earlier collection failures skip repairs.

The collector CronJob uses `concurrencyPolicy: Forbid`, including across image or target-node
updates. Repairs have no cluster-wide lease or transaction spanning the CIB read and repair
commands, so setup jobs or independently launched collectors can still race with them. A failed
command, including a conflicting write, is logged and retried from fresh CIB state on the next
collection. Convergence assumes concurrent writers agree on the desired alert registrations.

Initial setup and update-setup register alerts independently of this gate; see
[Alert-Agent Registration](job-controllers.md#alert-agent-registration).

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

## Node Events

Node Add/Update/Delete events enqueue the lifecycle controller's single worker.
They do not start reconciliation goroutines or restart Jobs directly. Readiness
changes refresh executor placement; name/UID/IP changes record new desired input.

## Integration with Job Controllers

The lifecycle manager provides `schedulableNodesFunc` to job controllers:

```go
schedulableNodesFunc := func() ([]*corev1.Node, error) {
    return lifecycleManager.getActivePacemakerNodes()
}
```

This function is passed to:
- `RunClusterJobController()` for setup and fencing jobs
- Status collector CronJob for node pinning

Job controllers call this function to:
1. Determine which nodes to target for round-robin retry
2. Refresh placement when eligible executors change, without invalidating running/completed work
3. Ensure jobs only run on valid Pacemaker members

See [Job Controllers - Multi-Node Job Pattern](job-controllers.md#multi-node-job-pattern) for how `schedulableNodesFunc` integrates with retry logic.

## Related Files

- **[pkg/tnf/operator/lifecycle_manager.go](../../pkg/tnf/operator/lifecycle_manager.go)** - PacemakerLifecycleManager controller, sync loop, informer work queue
- **[pkg/tnf/operator/helpers.go](../../pkg/tnf/operator/helpers.go)** - getActivePacemakerNodes(), intersection logic, staleness checking, node sorting
- **[pkg/tnf/operator/status_collector.go](../../pkg/tnf/operator/status_collector.go)** - Status collector CronJob, node rotation logic
- **[pkg/tnf/operator/job_controllers.go](../../pkg/tnf/operator/job_controllers.go)** - startJobControllers(), bootstrap vs runtime mode
- **[pkg/tnf/pkg/tools/nodes.go](../../pkg/tnf/pkg/tools/nodes.go)** - Node helpers (IsNodeReady, GetNodeNames, StringSlicesEqual, ListNodesFromInformer)
