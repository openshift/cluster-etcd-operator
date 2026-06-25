# Pacemaker Reconciliation Flow

## Overview

Reconciliation ensures that Pacemaker cluster membership stays synchronized with Kubernetes control plane nodes. It detects drift (mismatches between K8s and Pacemaker) and automatically fixes it by adding or removing nodes from the Pacemaker cluster.

## When Reconciliation Runs

Reconciliation is triggered by multiple sources:

1. **Periodic Sync** - Every 30 seconds (from `sync()` loop)
2. **Node Add Event** - When a new control plane node joins the cluster
3. **Node Update Event** - When a node's IP address changes while Ready
4. **Node Delete Event** - When a control plane node is removed from the cluster

## High-Level Flow

```text
┌──────────────────────────────────────────────────────────────┐
│ ReconcilePacemakerConfig()                                   │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Safety Checks:                 │
        │ - Node informer synced?        │
        │ - Valid node count (<= 2)?     │
        │ - Transition complete?         │
        │ - Setup job complete?          │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Get Current State:             │
        │ - K8s nodes (from informer)    │
        │ - Pacemaker nodes (from CR)    │
        │ - Stopped jobs (from API)      │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Detect Drift:                  │
        │ - Compare node names           │
        │ - Compare node IPs             │
        │ - Check for stale CR           │
        └────────────────────────────────┘
                         │
                 ┌───────┴────────┐
                 │                │
            No drift         Drift detected
                 │                │
                 ▼                ▼
        ┌────────────────┐  ┌──────────────────────┐
        │ Check for      │  │ Acquire mutex        │
        │ stopped job    │  │ (serialize updates)  │
        └────────────────┘  └──────────────────────┘
                 │                │
            ┌────┴────┐           ▼
            No    Yes        ┌──────────────────────┐
            │     │          │ Re-check drift       │
            ▼     ▼          │ (after lock)         │
        Return  Delete       └──────────────────────┘
                stopped               │
                job            ┌──────┴──────┐
                               │             │
                          Still drift   No drift
                               │             │
                               ▼             ▼
                    ┌──────────────────────┐  Return
                    │ Check current        │
                    │ generation ConfigMap │
                    └──────────────────────┘
                               │
                        ┌──────┴──────┐
                        │             │
                  Matches desired  Different state
                        │             │
                        ▼             ▼
                ┌──────────────┐  ┌──────────────────┐
                │ Reuse gen N  │  │ Create new       │
                │ Check if job:│  │ ConfigMap gen N+1│
                │ - Running?   │  └──────────────────┘
                │ - Complete?  │         │
                └──────────────┘         │
                        │                │
                    ┌───┴────┐           │
                    │        │           │
              Running or  Failed or      │
              Complete    Not exists     │
                    │        │           │
                    ▼        │           │
                 Return      └───────────┘
                                  │
                                  ▼
                    ┌──────────────────┐
                    │ Run auth jobs    │
                    │ on all nodes     │
                    └──────────────────┘
                               │
                               ▼
                    ┌──────────────────┐
                    │ Run update-setup │
                    │ job              │
                    └──────────────────┘
                               │
                               ▼
                    ┌──────────────────┐
                    │ Wait for         │
                    │ completion       │
                    └──────────────────┘
                               │
                               ▼
                    ┌──────────────────┐
                    │ Run after-setup  │
                    │ jobs on all nodes│
                    └──────────────────┘
```

## Drift Detection

Drift occurs when Kubernetes node state doesn't match Pacemaker cluster membership.

### What is Compared

```text
┌─────────────────────────┐         ┌─────────────────────────┐
│   Kubernetes Nodes      │         │   Pacemaker Nodes       │
│   (from node informer)  │   vs    │   (from CR status)      │
└─────────────────────────┘         └─────────────────────────┘
         │                                      │
         │                                      │
    ┌────┴────┐                            ┌────┴────┐
    │ Names   │                            │ Names   │
    │ IPs     │                            │ IPs     │
    └─────────┘                            └─────────┘
```

### Drift Scenarios

**Scenario 1: Node Added to Kubernetes**

```text
Kubernetes:  [master-0, master-1, master-2]
Pacemaker:   [master-0, master-1]
             
Drift: YES - master-2 exists in K8s but not Pacemaker
Action: Add master-2 to Pacemaker cluster
```

**Scenario 2: Node Deleted from Kubernetes**

```text
Kubernetes:  [master-0]
Pacemaker:   [master-0, master-1]
             
Drift: YES - master-1 exists in Pacemaker but not K8s
Action: Remove master-1 from Pacemaker cluster
```

**Scenario 3: Node IP Changed**

```text
Kubernetes:  master-0 IP = 192.168.1.10
Pacemaker:   master-0 IP = 192.168.1.11
             
Drift: YES - Node name matches but IP differs
Action: Update Pacemaker with new IP (remove old, add new)
```

**Scenario 4: No Drift**

```text
Kubernetes:  [master-0 (192.168.1.10), master-1 (192.168.1.11)]
Pacemaker:   [master-0 (192.168.1.10), master-1 (192.168.1.11)]
             
Drift: NO - Names and IPs match exactly
Action: None
```

## Update-Setup ConfigMap

When drift is detected, a ConfigMap is created (or reused) with the desired node state.

### ConfigMap Structure

**Name:** `tnf-update-setup-<generation>` in `openshift-etcd` namespace

**Data Fields:**
- `generation`: Ordering number for reconciliation ordering
- `nodes`: JSON array of desired node state - `[{"name": "master-0", "ip": "192.168.1.10"}, ...]`
- `timestamp`: When ConfigMap was created

### Generation Counter

The generation counter ensures correctness when multiple reconciliations are triggered close together, and prevents redundant job creation when state hasn't changed.

**Generation Reuse:**

When drift is detected, reconciliation first checks if the current generation's ConfigMap already describes the desired state:

```text
Current generation ConfigMap exists with nodes: [master-0, master-1]
Desired state: [master-0, master-1]
         ↓
States match → Reuse generation N
         ↓
Check if job already running or completed
         ↓
If yes → Skip restart (avoid interrupting work)
If no/failed → Restart job with same generation
```

**New Generation Creation:**

Only create a new generation if desired state differs from current:

```text
Current generation ConfigMap: [master-0, master-1]
Desired state: [master-0, master-1, master-2]
         ↓
States differ → Create new generation N+1
         ↓
Start update-setup job for generation N+1
```

**Multiple Concurrent Reconciliations:**

```text
Event 1: Node deleted at T=0
         ↓
      Generate ConfigMap with generation=1
         
Event 2: Different node deleted at T=0.1s (before job starts)
         ↓
      Generate ConfigMap with generation=2
         
update-setup job reads BOTH ConfigMaps, uses highest generation (2)
         ↓
      Reconciles both deletions in a single run
```

**Key Properties:** 
- Highest generation always wins, ensuring most recent state is applied
- Reuse generation when desired state unchanged, preventing redundant job restarts
- Skip restart if job already running or completed successfully (avoid interruption)

## Update-Setup Job Execution

The update-setup job runs on a Pacemaker node and reconciles Pacemaker membership to match the desired state from the ConfigMap.

### Job Flow

```text
┌──────────────────────────────────────────────────────────────┐
│ update-setup job (runs on pacemaker node)                    │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Read ConfigMaps:               │
        │ - Find all tnf-update-setup-*  │
        │ - Parse desired node list      │
        │ - Use highest generation       │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Remove Nodes:                  │
        │ - pcs cluster node remove      │
        │ - Remove unstarted etcd members│
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Add Nodes:                     │
        │ - pcs cluster node add         │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Update Fencing:                │
        │ - Update STONITH configuration │
        │ - Add/remove fence agents      │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Update Etcd Resource:          │
        │ - Update node IP list          │
        │ - Trigger resource restart     │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ If nodes added:                │
        │ - Force new cluster            │
        │ - Restart cluster              │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Sync and Start:                │
        │ - pcs cluster sync             │
        │ - pcs cluster start --all      │
        └────────────────────────────────┘
```

### Auth Jobs (Pre-Reconciliation)

Before running update-setup, auth jobs run on **all nodes** to ensure Pacemaker authentication is current:

```text
For each K8s node:
  ┌────────────────────────────────┐
  │ auth job                       │
  │ - Set hacluster password       │
  │ - Sync pacemaker auth          │
  └────────────────────────────────┘
```

**Why?** Ensures nodes can communicate with Pacemaker before membership changes.

### After-Setup Jobs (Post-Reconciliation)

After update-setup completes, after-setup jobs run on **all nodes**:

```text
For each K8s node:
  ┌────────────────────────────────┐
  │ after-setup job                │
  │ - Verify node in cluster       │
  │ - Start pacemaker services     │
  └────────────────────────────────┘
```

**Why?** Ensures all nodes have Pacemaker running after membership changes.

## Concurrency Protection

Multiple goroutines can call `ReconcilePacemakerConfig()` concurrently (periodic sync + node events). The function uses a two-phase check pattern:

### Phase 1: Unlocked Drift Detection

```text
Goroutine A           Goroutine B
    │                      │
    ▼                      ▼
Check drift          Check drift
    │                      │
Drift detected       Drift detected
    │                      │
    ▼                      ▼
```

**Purpose:** Allow concurrent drift detection without blocking.

### Phase 2: Serialized Update

```text
Goroutine A           Goroutine B
    │                      │
    ▼                      │
Lock mutex               │
    │                      ▼
Re-check drift         Wait for lock
    │                      │
Still drift              │
    │                      ▼
Create ConfigMap      Lock mutex
    │                      │
Start job              ▼
    │                Re-check drift
    ▼                      │
Unlock mutex          No drift (A fixed it)
                           ▼
                      Unlock mutex
```

**Purpose:** Prevent duplicate reconciliation while allowing eventual consistency.

### Generation Counter Protection

The mutex also protects the generation counter increment:

```go
reconcilePacemakerConfigMutex.Lock()
generation = updateSetupGeneration++
reconcilePacemakerConfigMutex.Unlock()
```

**Why?** Ensures unique, monotonically increasing generation numbers.

## Error Handling

Reconciliation is designed for eventual consistency:

**Transient Errors:**
- Node informer not synced → Skip this attempt, retry in 30s
- Setup job not complete → Skip this attempt, retry in 30s (prevents update-setup from running during initial setup)
- CR not available → Discovery mode (try nodes sequentially)
- Job creation fails → Error propagates to sync(), retry in 30s

**Permanent Failures:**
- More than 2 control plane nodes → Warning logged, no action
- No valid target nodes → Error returned, manual intervention may be needed
- All discovery attempts failed → Error returned

**Job State Handling:**
- Job running + desired state matches current generation → Skip restart (wait for completion)
- Job completed successfully + desired state matches current generation → Skip restart (wait for convergence)
- Job failed + desired state matches current generation → Restart job (retry with same generation)
- Job not exists + desired state matches current generation → Start job
- Job any state + desired state differs from current generation → Create new ConfigMap, restart job

## Discovery Mode

When PacemakerCluster CR is unavailable or stale, reconciliation enters discovery mode:

```text
┌──────────────────────────────────────────────────────────────┐
│ Discovery Mode (no actionable CR)                            │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Check stopped job:             │
        │ - Which node was last tried?   │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Try other node:                │
        │ - Run update-setup on untried  │
        │   node to discover pacemaker   │
        └────────────────────────────────┘
                         │
                 ┌───────┴────────┐
                 │                │
            Success          Failure
                 │                │
                 ▼                ▼
        ┌────────────────┐  ┌──────────────────┐
        │ CR populates   │  │ Try next node    │
        │ Exit discovery │  │ (next sync)      │
        └────────────────┘  └──────────────────┘
```

**Purpose:** Recover from scenarios where CR is missing or corrupted.

## Node Replacement Scenario

A common scenario where reconciliation is critical:

```text
Initial State:
  K8s:       [master-0, master-1]
  Pacemaker: [master-0, master-1]

Step 1: Delete master-1
  K8s:       [master-0]
  Pacemaker: [master-0, master-1]  ← DRIFT
  
  Reconciliation triggered:
    - Remove master-1 from Pacemaker
    
  K8s:       [master-0]
  Pacemaker: [master-0]  ← SYNCED

Step 2: New master-1 boots and registers
  K8s:       [master-0, master-1]
  Pacemaker: [master-0]  ← DRIFT
  
  Reconciliation triggered:
    - Add master-1 to Pacemaker
    
  K8s:       [master-0, master-1]
  Pacemaker: [master-0, master-1]  ← SYNCED
```

## Integration with Health Monitoring

Reconciliation and health monitoring work together:

**Reconciliation:**
- Fixes drift (operational correctness)
- Triggered by node events + periodic sync
- Creates jobs and ConfigMaps

**Health Monitoring:**
- Reports status (observability)
- Triggered by periodic sync only
- Updates operator conditions and records events

Both operations run independently in `sync()` - a health check failure doesn't prevent reconciliation.
