# TNF Job Controllers

## Overview

Job controllers manage the lifecycle of Kubernetes Jobs that configure and maintain the Pacemaker cluster. `StartJobControllers()` adapts its behavior based on whether the external etcd transition has completed (see [Dual-Mode Operation](architecture-overview.md#dual-mode-operation) in the architecture overview).

## Bootstrap vs Runtime Behavior

### Bootstrap Mode (Pre-Transition)

Drives initial transition from static pod etcd to Pacemaker control:
- Requires exactly 2 ready nodes (Pacemaker limitation)
- Exponential backoff retry (5s to 2min, ~10min total)
- Sets `TNFJobControllersDegraded` condition
- Uses mutex to prevent concurrent attempts

### Runtime Mode (Post-Transition)

Ensures job controllers stay running:
- Supports 1 or 2 ready nodes (handles node replacement)
- Idempotent - safe to call repeatedly
- No explicit retry (controller framework retries sync())
- On operator restart: skips bootstrap if setup job already complete

## StartJobControllers() Decision Flow

```text
StartJobControllers()
 │
 ├─ Check HasExternalEtcdCompletedTransition()
 │
 ├─ Pre-transition (Bootstrap):
 │   - Require exactly 2 ready nodes
 │   - Exponential backoff retry (5s→2min, 9 steps, ~10min total)
 │   - Set TNFJobControllersDegraded condition on failure
 │
 └─ Post-transition (Runtime):
     - Require all nodes ready (supports 1 or 2 nodes)
     - Idempotent (uses jobControllersStarted flag)
     - On operator restart: skip bootstrap if setup job complete
```

## startTnfJobcontrollers() Bootstrap Sequence

**Fast path (operator restart, setup job complete):**  
Skip bootstrap flow and start all job controllers without re-running bootstrap checks (etcd informer sync, stable revision waits). Controllers are idempotent and won't recreate completed jobs.

**Bootstrap path (initial setup):**
1. Wait for etcd informer sync, etcd bootstrap completed, stable revision
2. Start per-node controllers: auth, after-setup
3. Start cluster-wide controllers: setup, fencing

## Job Types and Behaviors

See [Job Types](architecture-overview.md#job-types) in architecture overview for the complete table.

**Node-Specific (auth, after-setup):**
- Labeled with node UID, cleaned up on node deletion
- Started when node becomes Ready
- **auth:** Sets hacluster password (required before Pacemaker operations)
- **after-setup:** Disables kubelet systemd service (Pacemaker owns it now)

**Cluster-Wide (setup, fencing, update-setup):**
- Multi-node retry with round-robin selection
- **setup:** Creates Pacemaker cluster, marks transition complete
- **fencing:** Configures STONITH for BMC power control
- **update-setup:** Adds/removes nodes (Day 2 reconciliation)

## Multi-Node Retry Logic

Cluster-wide jobs (setup, fencing, update-setup) use `targetNodesFunc` to retry across multiple nodes on failure. Round-robin selection tries each valid target node before degrading.

**Retry State Tracking:**
- `AttemptNumber`: Current attempt (1-N)  
- `NodeIndex`: Which node to try next (round-robin)
- `LastJobConfig`: Detects config drift (resets state if config changes)

**Node Selection:** Tries all nodes per attempt, with 3 max attempts:
- Attempt 1: master-0, then master-1 (if master-0 fails)
- Attempt 2: master-0, then master-1 (if master-0 fails)
- Attempt 3: master-0, then master-1 (if master-0 fails)
- Total: up to 6 tries (3 attempts × 2 nodes)
- All attempts exhausted → set Degraded condition, reset to attempt 1 (allows recovery)

**Config Drift:** If `targetNodesFunc` returns different nodes or `jobConfigFunc` returns different config, retry state resets to attempt 1 (prevents using stale decisions).

**Example: Fencing Job Config**
```go
// jobConfigFunc captures node UIDs and fencing secret UIDs to detect infrastructure changes
fencingJobConfigFunc := func() (string, error) {
    config := map[string]interface{}{
        "nodeUIDs": []string{node1.UID, node2.UID},           // Detect node replacements
        "secretUIDs": []string{secret1.UID, secret2.UID},     // Detect secret rotation
    }
    return json.Marshal(config)
}
```
If a node is replaced (same name, different UID) or fencing secrets are rotated, the config changes and the fencing job is recreated with updated BMC credentials.

## Job Flow Diagrams

### Bootstrap Job Sequence

StartJobControllers starts job controllers (not jobs directly):

```text
1. Start auth + after-setup controllers (per-node)
2. Start setup + fencing controllers (cluster-wide)
3. Return (startTnfJobcontrollers complete)

Job controllers run independently:
- Auth jobs set hacluster password
- Setup job configures Pacemaker cluster and marks transition complete
- After-setup jobs disable kubelet
- Fencing job configures STONITH

Bootstrap complete when startTnfJobcontrollers returns.
```

**Important timing:**
- **Transition complete**: Marked by setup JOB when it calls etcd.RemoveStaticContainer()
- **Reconciliation guard**: Waits for setup JOB completion (ensures Pacemaker cluster exists)
- Jobs run independently - setup may finish before after-setup completes

### Reconciliation Job Sequence (Node Added)

```text
New node master-2 becomes Ready
 │
 ├─ StartJobControllers() triggered (Ready transition)
 │  └─ auth-master-0, auth-master-1, auth-master-2 controllers started
 │     (Start running auth jobs to set hacluster password)
 │
 ├─ ReconcilePacemakerConfig() triggered (drift detection)
 │  └─ update-setup job started
 │     │
 │     ├─ Wait for ALL auth jobs to complete
 │     │  (Ensures hacluster password set before Pacemaker operations)
 │     │
 │     └─ Add master-2 to Pacemaker cluster
 │
 ├─ after-setup controllers (started by StartJobControllers)
 │  └─ Wait for setup job complete, then disable kubelet
 │
 └─ Reconciliation complete
```

**Auth job dependency:** Update-setup waits for auth jobs to complete before running Pacemaker operations (similar to how setup job waits for auth during bootstrap). This prevents failures from missing hacluster passwords.

### Reconciliation Job Sequence (Node Removed)

```text
Node master-1 deleted from K8s
 │
 ├─ ReconcilePacemakerConfig() triggered (drift detection)
 │  └─ update-setup job started (removes master-1 from Pacemaker)
 │
 ├─ CleanupOrphanedJobs() triggered (periodic sync)
 │  └─ Delete auth-master-1, after-setup-master-1 (node UID no longer exists)
 │
 └─ Reconciliation complete
```

## Fencing Secret Changes

**What:** Automatic fencing job restart when BMC credentials are updated  
**When:** After external etcd transition completes  
**Why:** Allows dynamic credential updates without manual intervention

### Flow Overview

```text
User updates secret (tnf-fencing-*)
        │
        ▼
Secret informer detects change
        │
        ▼
Is it a fencing secret?
        │
    ┌───┴───┐
    No      Yes
    │       │
    ▼       ▼
  Skip   Data changed?
            │
        ┌───┴───┐
        No      Yes
        │       │
        ▼       ▼
      Skip   Transition complete?
                │
            ┌───┴───┐
            No      Yes
            │       │
            ▼       ▼
          Skip   Restart fencing job
                    │
                    ▼
            New credentials applied
```

### Three Fencing Configuration Points

Fencing (STONITH) is configured at three distinct points:

| When | Trigger | Method |
|------|---------|--------|
| **Bootstrap** | Both nodes Ready | Standalone fencing job in bootstrap sequence |
| **Secret changes** | `tnf-fencing-*` updated | Automatic job restart (this section) |
| **Membership changes** | Nodes added/removed | Inline during update-setup reconciliation |

**Why three?** Different lifecycle phases need different approaches - bootstrap uses a job, secret changes trigger reactive restart, membership changes update inline during reconciliation.

### Operational Workflow

To update BMC credentials:

```text
1. Edit secret: oc edit secret tnf-fencing-master-0 -n openshift-etcd
        │
        ▼
2. Save changes
        │
        ▼
3. Automatic: Fencing job restarts within seconds
        │
        ▼
4. New credentials applied to Pacemaker STONITH
```

**No manual intervention required.**

## Concurrency Protection

- **`startJobControllersMu` mutex:** Prevents concurrent job controller startup
- **Job-level idempotency:** Jobs check if work already done (e.g., `clusterExists()` before cluster creation)
- **`reconcilePacemakerConfigMutex`:** Serializes drift detection and reconciliation

## Error Handling

**Bootstrap (pre-transition):**  
- Exponential backoff retry (5s to 2min, ~10min total)
- After exhausting retries → set `TNFJobControllersDegraded=True`

**Runtime (post-transition):**  
- No explicit retry in StartJobControllers (controller framework retries sync() every 30s)
- Multi-node jobs retry across nodes (see Multi-Node Retry Logic above)

## Integration with Other Components

**StartJobControllers** (this document):
- Starts auth, after-setup controllers when nodes become Ready
- Starts setup, fencing controllers during bootstrap
- Independent of drift detection

**ReconcilePacemakerConfig** (see [reconciliation.md](reconciliation.md)):
- Detects drift between K8s and Pacemaker
- Creates update-setup ConfigMap and starts update-setup job
- **Does NOT start auth/after-setup jobs** (those run independently via StartJobControllers)

**CleanupOrphanedJobs** (see [lifecycle_cleanup.go](../../pkg/tnf/operator/lifecycle_cleanup.go)):
- Deletes jobs for deleted nodes (by node UID)
- Runs independently during periodic sync

**MonitorHealth** (see [lifecycle_health.go](../../pkg/tnf/operator/lifecycle_health.go)):
- Reads PacemakerCluster CR status
- Reports operator conditions
- No interaction with job controllers

