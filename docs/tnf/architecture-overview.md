# TNF Lifecycle Architecture Overview

## High-Level Design

The TNF (Two-Node Fencing) lifecycle system ensures that a Pacemaker high-availability cluster stays synchronized with OpenShift control plane nodes. It operates as a Kubernetes controller that runs continuously, executing a periodic sync loop every 30 seconds and responding to node lifecycle events.

## Dual-Mode Operation

The lifecycle manager adapts its behavior based on whether the external etcd transition has completed:

### Bootstrap Mode (Pre-Transition)

**State:** External etcd is transitioning from static pods to Pacemaker control

**Operations:**
- Drive the initial transition by starting TNF jobs
- Requires exactly 2 ready control plane nodes
- Uses exponential backoff retry (5s to 2min, ~10 min total)
- Sets `TNFJobControllersDegraded` condition on failures
- Cleans up orphaned jobs from previous attempts

**Goal:** Successfully transition etcd from static pod management to Pacemaker cluster control

### Post-Transition Mode (Runtime)

**State:** External etcd transition is complete, Pacemaker is managing etcd

**Operations:**
- Monitor Pacemaker cluster health every 30 seconds
- Detect drift between Kubernetes nodes and Pacemaker membership
- Reconcile differences by adding/removing nodes from Pacemaker
- Support single-node scenarios during node replacement
- Clean up orphaned jobs when nodes are deleted

**Goal:** Maintain a healthy, synchronized Pacemaker cluster that matches the Kubernetes control plane

## System Architecture

```text
┌─────────────────────────────────────────────────────────────────┐
│                   PacemakerLifecycleManager                     │
│                    (Main Controller)                            │
└─────────────────────────────────────────────────────────────────┘
                             │
                             │ Periodic Sync (30s)
                             │ + Node Events (Add/Update/Delete)
                             ▼
        ┌────────────────────────────────────────────┐
        │          Check External Etcd               │
        │          Transition Complete?              │
        └────────────────────────────────────────────┘
                             │
        ┌────────────────────┴────────────────────┐
        │ false (Bootstrap)  │ true (Runtime)    │
        ▼                    ▼                    ▼
┌──────────────────┐   ┌──────────────────┐
│ Bootstrap Mode   │   │ Post-Transition  │
│                  │   │ Mode             │
│ 1. StartJob      │   │ 1. StartJob      │
│    Controllers   │   │    Controllers   │
│    (drive        │   │    (ensure       │
│     transition)  │   │     running)     │
│                  │   │                  │
│ 2. CleanupOrphan │   │ 2. CleanupOrphan │
│    edJobs        │   │    edJobs        │
│    (bootstrap    │   │    (deleted      │
│     cleanup)     │   │     nodes)       │
└──────────────────┘   │                  │
                       │ 3. MonitorHealth │
                       │    (health       │
                       │     checks)      │
                       │                  │
                       │ 4. Reconcile     │
                       │    Pacemaker     │
                       │    Config        │
                       │    (drift fix)   │
                       └──────────────────┘
```

## Key Components

### 1. PacemakerLifecycleManager

**Location:** `pkg/tnf/operator/lifecycle_manager.go`

The main controller that orchestrates all TNF operations:
- Runs a periodic sync loop (every 30 seconds)
- Registers event handlers for node Add/Update/Delete events
- Coordinates startup, health monitoring, reconciliation, and cleanup
- Maintains state for health transitions and event deduplication

### 2. StartJobControllers

**Location:** `pkg/tnf/operator/lifecycle_job_controllers.go`

Manages the lifecycle of TNF Kubernetes jobs:
- **Bootstrap Mode:** Drives initial transition with retry logic and degraded conditions
- **Runtime Mode:** Ensures jobs stay running, handles operator restarts
- Adapts behavior based on transition state (dual-mode function)
- Protected by mutex to prevent concurrent startup attempts

### 3. ReconcilePacemakerConfig

**Location:** `pkg/tnf/operator/lifecycle_reconciliation.go`

Detects and fixes drift between Kubernetes and Pacemaker:
- Compares K8s nodes (from informer) vs Pacemaker membership (from CR)
- Creates update-setup ConfigMaps with reconciliation decisions
- Triggers update-setup job to add/remove nodes
- Uses mutex to serialize reconciliation attempts
- Only runs after external etcd transition completes

### 4. MonitorHealth

**Location:** `pkg/tnf/operator/lifecycle_health.go`

Monitors Pacemaker cluster health and reports status:
- Reads PacemakerCluster CR for cluster state
- Detects staleness (CR not updated recently)
- Updates operator conditions (Degraded, Progressing, Available)
- Records events with deduplication (5 min default, 24 hr for fencing)
- Only runs after external etcd transition completes

### 5. CleanupOrphanedJobs

**Location:** `pkg/tnf/operator/lifecycle_cleanup.go`

Removes jobs and pods for deleted nodes:
- Lists TNF jobs (auth, after-setup, update-setup)
- Identifies jobs tied to non-existent nodes (via node label)
- Deletes orphaned jobs to prevent resource leaks
- **Also deletes orphaned pods** whose owner Jobs no longer exist
- Runs in both bootstrap and runtime modes

## Periodic Sync Loop

The controller runs `sync()` every 30 seconds, executing this sequence:

```text
┌─────────────────────────────────────────────────────────────┐
│ sync() - called every 30 seconds                            │
└─────────────────────────────────────────────────────────────┘
                         │
                         ▼
        Check HasExternalEtcdCompletedTransition()
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Run in both modes:             │
        │ 1. StartJobControllers()       │
        │ 2. CleanupOrphanedJobs()       │
        └────────────────────────────────┘
                         │
                         ▼
                 Transition complete?
                         │
                    ┌────┴────┐
                    No        Yes
                    │         │
                    ▼         ▼
                Return   ┌────────────────────┐
                         │ 3. MonitorHealth() │
                         │ 4. Reconcile       │
                         │    PacemakerConfig│
                         └────────────────────┘
                                  │
                                  ▼
                            Return errors
```

**Error Handling:**
- Each operation continues even if others fail
- Errors are aggregated and returned
- Controller framework marks operator as Degraded if errors persist
- Periodic sync ensures eventual consistency

## Node Event Handlers

The controller responds to Kubernetes node lifecycle events:

### Add Event

```text
Node added to cluster
        │
        ▼
Trigger ReconcilePacemakerConfig()
        │
        ▼
Drift detection
        │
        ▼
If new node detected:
  Create update-setup ConfigMap
  Run update-setup job
  Add node to Pacemaker
```

### Update Event (Ready Transition)

```text
Node transitions NotReady → Ready
        │
        ▼
Trigger StartJobControllers()
        │
        ▼
Bootstrap Mode: Retry transition
Runtime Mode: Ensure jobs running
```

### Update Event (IP Change)

```text
Node IP changes while Ready
        │
        ▼
Trigger ReconcilePacemakerConfig()
        │
        ▼
Drift detection
        │
        ▼
Update Pacemaker with new IP
```

### Delete Event

```text
Node deleted from cluster
        │
        ▼
Trigger ReconcilePacemakerConfig()
        │
        ▼
Drift detection
        │
        ▼
If node still in Pacemaker:
  Create update-setup ConfigMap
  Run update-setup job
  Remove node from Pacemaker
        │
        ▼
CleanupOrphanedJobs()
        │
        ▼
Delete auth/after-setup jobs
```

## Data Flow

```text
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Kubernetes    │     │  TNF Lifecycle  │     │   Pacemaker     │
│   API Server    │────▶│    Manager      │────▶│   Cluster       │
└─────────────────┘     └─────────────────┘     └─────────────────┘
        │                        │                        │
        │ Node informer          │ PacemakerCluster       │
        │ (watch nodes)          │ informer (watch CR)    │
        │                        │                        │
        ▼                        ▼                        ▼
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│ Node Events:    │     │ Periodic Sync:  │     │ Status Updates: │
│ - Add           │     │ - Every 30s     │     │ - Cluster state │
│ - Update        │     │ - Drift check   │     │ - Node list     │
│ - Delete        │     │ - Health check  │     │ - Fencing status│
└─────────────────┘     └─────────────────┘     └─────────────────┘
```

## Concurrency Model

The lifecycle manager handles concurrent operations safely:

### Instance-Level Mutexes

**recordedEventsMu** - Protects event deduplication map
- Prevents concurrent map access
- Short critical sections (map reads/writes only)

**previousMu** - Protects previous health status
- Serializes health state transitions
- Prevents race conditions in event recording

**startJobControllersMu** - Serializes job controller startup
- Prevents duplicate job creation
- Critical during bootstrap retry attempts

### Package-Level Mutexes

**reconcilePacemakerConfigMutex** (in `lifecycle_reconciliation.go`)
- Serializes all reconciliation attempts
- Shared across all controller instances
- Protects update-setup ConfigMap generation counter

### Context Management

**lifecycleCtx / lifecycleCtxCancel**
- Manages goroutine lifecycle for event handlers
- Cancelled during controller shutdown
- Prevents goroutine leaks

## Job Types

TNF jobs are Kubernetes Jobs that configure and manage Pacemaker:

| Job Type | Scope | When | Purpose |
|----------|-------|------|---------|
| **auth** | Per-node | Bootstrap + reconciliation | Set hacluster password for Pacemaker auth |
| **setup** | Cluster | Bootstrap only | Initial Pacemaker cluster creation (Day 1) |
| **after-setup** | Per-node | Bootstrap only | Add node to newly created cluster (Day 1) |
| **fencing** | Cluster | Bootstrap + secret changes | Configure STONITH fencing agents (BMC credentials) - see [Fencing Secret Changes](job-controllers.md#fencing-secret-changes) |
| **update-setup** | Cluster | Reconciliation | Add/remove nodes from existing cluster (Day 2+) |

**Node-specific jobs** (auth, after-setup):
- Labeled with `node: <node-name>`
- Tied to node lifecycle via UID
- Cleaned up when node is deleted

**Cluster-wide jobs** (setup, fencing, update-setup):
- Not tied to specific nodes
- Persist across node changes
- Update-setup regenerated during reconciliation

## External Dependencies

The lifecycle manager integrates with several systems:

**Kubernetes API:**
- Node informer (control plane nodes)
- Job API (TNF jobs)
- ConfigMap API (update-setup decisions)
- Secret API (fencing credentials)

**Operator Framework:**
- StaticPodOperatorClient (operator status)
- Degraded/Progressing/Available conditions
- Event recorder

**Pacemaker:**
- PacemakerCluster CR (cluster status)
- Status collector CronJob (periodic `pcs status xml`)
- Node membership via `pcmk_node_list`

## Design Principles

**Self-Healing:** Periodic reconciliation detects and fixes drift automatically, recovering from missed events or operator downtime

**Dual-Mode Behavior:** Single codebase handles both bootstrap (driving transition) and runtime (maintaining health) without separate code paths

**Graceful Degradation:** Operations continue independently - a health check failure doesn't prevent reconciliation

**Idempotency:** Safe to call repeatedly - operations detect current state and only take action when needed

**Event + Periodic Hybrid:** Node events trigger immediate action, but periodic sync provides backup detection and recovery
