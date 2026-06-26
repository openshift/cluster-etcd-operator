# Controller Lifecycle

## Overview

The PacemakerLifecycleManager is a Kubernetes controller that runs continuously as part of the cluster-etcd-operator. It manages the integration between OpenShift control plane nodes and the Pacemaker high-availability cluster.

## Startup Sequence

### Operator Initialization

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
        │ NewPacemakerLifecycleManager() │
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
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Create status collector CronJob│
        │ (periodic pcs status xml)      │
        └────────────────────────────────┘
```

**Key Points:**
- Lifecycle manager does NOT wait for external etcd transition before starting
- The `sync()` function handles both bootstrap and runtime modes internally
- CRD must be established before informers can start

### Controller Initialization

```text
┌──────────────────────────────────────────────────────────────┐
│ NewPacemakerLifecycleManager()                               │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Create REST client for         │
        │ PacemakerCluster CR            │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Create SharedIndexInformer     │
        │ - Resource: PacemakerCluster   │
        │ - Resync: 30 seconds           │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Initialize struct:             │
        │ - recordedEvents: empty map    │
        │ - previous: nil                │
        │ - lifecycleCtx: from context   │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Create factory.Controller      │
        │ - Sync: c.sync                 │
        │ - Resync: 30 seconds           │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Register node event handlers   │
        │ - AddFunc                      │
        │ - UpdateFunc                   │
        │ - DeleteFunc                   │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Return:                        │
        │ - controller                   │
        │ - manager                      │
        │ - informer                     │
        └────────────────────────────────┘
```

## Periodic Sync Loop

The controller runs `sync()` every 30 seconds plus whenever informers detect changes.

### Sync Flow

```text
┌──────────────────────────────────────────────────────────────┐
│ sync(ctx, syncCtx) - called every 30 seconds                 │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Check ExternalEtcd             │
        │ TransitionCompleted?           │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Initialize error aggregator    │
        │ var errs []error               │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Operation 1:                   │
        │ StartJobControllers()          │
        │ - Runs in both modes           │
        │ - Append error if fails        │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Operation 2:                   │
        │ CleanupOrphanedJobs()          │
        │ - Runs in both modes           │
        │ - Deletes orphaned jobs        │
        │ - Deletes orphaned pods        │
        │ - Append error if fails        │
        └────────────────────────────────┘
                         │
                         ▼
                 Transition complete?
                         │
                    ┌────┴────┐
                    No        Yes
                    │         │
                    ▼         ▼
        ┌──────────────┐  ┌────────────────────┐
        │ Skip ops 3-4 │  │ Operation 3:       │
        └──────────────┘  │ MonitorHealth()    │
                          │ - Append error     │
                          └────────────────────┘
                                   │
                                   ▼
                          ┌────────────────────┐
                          │ Operation 4:       │
                          │ Reconcile          │
                          │ PacemakerConfig()  │
                          │ - Append error     │
                          └────────────────────┘
                                   │
                                   ▼
        ┌────────────────────────────────┐
        │ Return errors.NewAggregate(errs)│
        └────────────────────────────────┘
```

**Error Handling:**
- Each operation continues even if others fail
- Errors are aggregated and returned together
- Controller framework marks operator Degraded if errors persist
- Next sync attempt happens in 30 seconds

**Why Independent Operations?**
- Health check failure shouldn't prevent reconciliation
- Job startup failure shouldn't prevent cleanup
- Partial progress is better than complete failure

## Node Event Handlers

The controller responds to Kubernetes node lifecycle events in addition to periodic sync.

### AddFunc (Node Added)

```text
Node added to cluster
        │
        ▼
Is it a control plane node?
        │
    ┌───┴───┐
    No      Yes
    │       │
    ▼       ▼
 Ignore  Log: "node added: <name>"
            │
            ▼
        Spawn goroutine
            │
            ▼
        ReconcilePacemakerConfig(lifecycleCtx)
            │
            ▼
        (Drift detection & reconciliation)
```

**Why Goroutine?** Event handlers must return quickly - reconciliation can take time (job creation, API calls).

**Why ReconcilePacemakerConfig?** Adding a node creates drift (K8s has node, Pacemaker doesn't).

### UpdateFunc (Node Updated)

```text
Node updated
        │
        ▼
Is it a control plane node?
        │
    ┌───┴───┐
    No      Yes
    │       │
    ▼       ▼
 Ignore  Check transition type
            │
        ┌───┴───────────────┐
        │                   │
   NotReady→Ready      IP changed while Ready
        │                   │
        ▼                   ▼
   Spawn goroutine     Spawn goroutine
        │                   │
        ▼                   ▼
   StartJobControllers  ReconcilePacemakerConfig
   (lifecycleCtx)       (lifecycleCtx)
        │                   │
        ▼                   ▼
   (Bootstrap or       (Drift detection &
    ensure running)     reconciliation)
```

**Two Triggers:**

1. **NotReady→Ready:** Node became available
   - Bootstrap mode: Trigger initial transition retry
   - Runtime mode: Ensure jobs are running on newly Ready node

2. **IP Changed:** Node IP address changed while Ready
   - Trigger reconciliation to update Pacemaker with new IP
   - Rare scenario but critical for correctness

### DeleteFunc (Node Deleted)

```text
Node deleted from cluster
        │
        ▼
Is it a control plane node?
        │
    ┌───┴───┐
    No      Yes
    │       │
    ▼       ▼
 Ignore  Check for tombstone
            │
        ┌───┴───────────┐
        │               │
    Regular delete  Tombstone
        │               │
        └───────┬───────┘
                │
                ▼
        Log: "node deleted: <name>"
                │
                ▼
        Spawn goroutine
                │
                ▼
        ReconcilePacemakerConfig(lifecycleCtx)
                │
                ▼
        (Drift detection & reconciliation)
```

**Tombstone Handling:** If informer cache is cleared before handler runs, we get a tombstone. Handler extracts the node from tombstone to log the deletion.

**Why ReconcilePacemakerConfig?** Deleting a node creates drift (Pacemaker has node, K8s doesn't).

## Lifecycle Context Management

The controller uses context to manage goroutine lifecycle spawned by event handlers.

### Context Creation

```text
NewPacemakerLifecycleManager()
        │
        ▼
lifecycleCtx, lifecycleCtxCancel = context.WithCancel(parentCtx)
        │
        ▼
Store in struct:
  - lifecycleCtx: context.Context
  - lifecycleCtxCancel: context.CancelFunc
```

### Context Usage

```text
Event handler fires (Add/Update/Delete)
        │
        ▼
Spawn goroutine
        │
        ▼
Pass lifecycleCtx to operation:
  - StartJobControllers(c.lifecycleCtx)
  - ReconcilePacemakerConfig(c.lifecycleCtx)
        │
        ▼
Operation uses context for:
  - API calls (can be cancelled)
  - Job creation (can be cancelled)
  - Reconciliation (can be cancelled)
```

### Context Cancellation (Shutdown)

```text
Controller shutdown initiated
        │
        ▼
lifecycleCtxCancel() called
        │
        ▼
All goroutines spawned by event handlers
receive cancellation signal
        │
        ▼
Operations abort gracefully:
  - API calls return context.Canceled
  - Goroutines exit
  - No goroutine leaks
```

**Why Important?** Without proper context management, event handler goroutines would leak during operator restarts.

## Informer Resync

Both the PacemakerCluster informer and the controller factory use a 30-second resync interval.

### What Resync Does

```text
Every 30 seconds:
        │
        ▼
Informer re-lists resources from API server
        │
        ▼
Compares with local cache
        │
    ┌───┴───────────┐
    │               │
  Changed       Unchanged
    │               │
    ▼               ▼
Trigger sync()   No action
```

**Why 30 Seconds?**
- Balance between responsiveness and API load
- Matches Pacemaker health check frequency
- Provides eventual consistency even if events are missed

**What It Recovers:**
- Missed informer events (network blip, operator restart)
- Stale cache state (API server changes not propagated)
- Operator downtime (catches up on changes during restart)

## Controller Framework Integration

The lifecycle manager integrates with the OpenShift library-go controller framework.

### Factory Controller

```text
factory.Controller created with:
        │
        ├─ Sync function: c.sync
        │
        ├─ Informers:
        │  - operatorClient (etcd operator status)
        │  - pacemakerInformer (PacemakerCluster CR)
        │  - nodeInformer (K8s nodes)
        │
        └─ Resync interval: 30 seconds
                │
                ▼
        Controller framework runs:
                │
                ├─ Call sync() periodically
                │
                ├─ Call sync() on informer updates
                │
                ├─ Track degraded conditions
                │
                └─ Implement exponential backoff on errors
```

### Degraded Condition Handling

```text
sync() returns error
        │
        ▼
Controller framework checks error
        │
    ┌───┴───────────┐
    │               │
  nil error     Has error
    │               │
    ▼               ▼
Clear degraded  Set degraded condition
condition       in operator status
    │               │
    ▼               ▼
Available=True  Degraded=True
                Available=False
```

**Automatic Retry:** Framework retries failed sync operations with exponential backoff.

## Shutdown Sequence

```text
Operator shutdown signal received
        │
        ▼
Stop accepting new informer events
        │
        ▼
Cancel lifecycleCtx
        │
        ▼
Wait for in-flight goroutines to exit
        │
        ▼
Stop informers
        │
        ▼
Stop controller
        │
        ▼
Cleanup complete
```

**Graceful Shutdown:** In-flight operations receive cancellation signal and can clean up before exiting.

## Concurrency Model

The controller uses multiple synchronization primitives to handle concurrent operations safely.

### Mutex Overview

| Mutex | Scope | Protects | Location |
|-------|-------|----------|----------|
| recordedEventsMu | Instance | Event deduplication map | lifecycle_health.go |
| previousMu | Instance | Previous health status | lifecycle_health.go |
| startJobControllersMu | Instance | Job controller startup | lifecycle_job_controllers.go |
| reconcilePacemakerConfigMutex | Package | Reconciliation + generation counter | lifecycle_reconciliation.go |

### Concurrent Callers

```text
Periodic Sync (every 30s)
        │
        ├─> StartJobControllers()
        ├─> CleanupOrphanedJobs()
        │     ├─> cleanupOrphanedJobs()
        │     └─> cleanupOrphanedPods()
        ├─> MonitorHealth() [if transition complete]
        └─> ReconcilePacemakerConfig() [if transition complete]

Node Add Event
        │
        └─> ReconcilePacemakerConfig()

Node Update Event (Ready)
        │
        └─> StartJobControllers()

Node Update Event (IP change)
        │
        └─> ReconcilePacemakerConfig()

Node Delete Event
        │
        └─> ReconcilePacemakerConfig()
```

**All operations can run concurrently** - mutexes protect critical sections.

## Health Monitoring

Health monitoring checks the Pacemaker cluster status and reports it to the OpenShift operator framework. It runs every 30 seconds as part of the periodic sync loop and only operates after the external etcd transition completes.

### When Health Monitoring Runs

```text
sync() called (every 30 seconds)
        │
        ▼
Check ExternalEtcdTransitionCompleted
        │
    ┌───┴───┐
    No      Yes
    │       │
    ▼       ▼
 Skip    MonitorHealth()
        (health monitoring)
```

**Why Only After Transition?** Before transition, Pacemaker cluster doesn't exist yet - nothing to monitor.

### Health Monitoring Flow

```text
┌──────────────────────────────────────────────────────────────┐
│ MonitorHealth(ctx)                                           │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ getPacemakerStatus()           │
        │ - Read PacemakerCluster CR     │
        │ - Check staleness              │
        │ - Build health status          │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ updateOperatorStatus()         │
        │ - Set Degraded condition       │
        │ - Set Progressing condition    │
        │ - Set Available condition      │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ recordHealthCheckEvents()      │
        │ - Cleanup expired events       │
        │ - Record warnings              │
        │ - Record errors                │
        └────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ recordHealthTransitionEvents() │
        │ - Detect state changes         │
        │ - Record transition events     │
        └────────────────────────────────┘
```

### Get Pacemaker Status

Status is retrieved from the PacemakerCluster custom resource:

```text
┌──────────────────────────────────────────────────────────────┐
│ getPacemakerStatus()                                         │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Read PacemakerCluster CR       │
        │ from informer cache            │
        └────────────────────────────────┘
                         │
                 ┌───────┴────────┐
                 │                │
            CR exists        CR not found
                 │                │
                 ▼                ▼
        ┌────────────────┐  ┌──────────────────┐
        │ Check staleness│  │ Return Unknown   │
        │ (5 min window) │  │ health status    │
        └────────────────┘  └──────────────────┘
                 │
        ┌────────┴────────┐
        │                 │
   Not stale         Stale
        │                 │
        ▼                 ▼
┌────────────────┐  ┌──────────────────┐
│ Build health   │  │ Return Unknown   │
│ status from CR │  │ health status    │
└────────────────┘  └──────────────────┘
```

**Staleness Check:** CR status is considered stale if `lastUpdated` is more than 5 minutes old. Status collector runs every minute, so missing 5 consecutive updates indicates a problem.

**Health States:**
- **Active:** Pacemaker cluster is healthy (all nodes online, quorum established, no errors)
- **Degraded:** Pacemaker cluster has issues (node offline, fencing failures, resource failures)
- **Unknown:** Cannot determine health (CR doesn't exist, status is stale, parsing error)

### Update Operator Status

Operator conditions are set based on health status:

```text
Health Status        Degraded    Progressing    Available
─────────────────────────────────────────────────────────
Active               False       False          True
Degraded             True        False          False
Unknown              False       True           False
```

**Condition Messages:**
- Active: `"Pacemaker cluster is active with 2 nodes online"`
- Degraded: `"Node master-1 is offline; Fencing failure on node master-0"`

### Event Recording with Deduplication

Events are recorded for warnings and errors with deduplication to prevent spam.

**Deduplication Windows:**

| Event Type | Window | Reason |
|------------|--------|--------|
| Default | 5 minutes | Prevent log spam for transient issues |
| Fencing | 24 hours | Fencing events are critical - record daily until resolved |

**Event Flow:**

```text
Event occurs: "Node master-1 is offline"
        │
        ▼
Generate event key: "warning:Node master-1 is offline"
        │
        ▼
Check recordedEvents map
        │
    ┌───┴────────────────┐
    │                    │
Key exists          Key doesn't exist
    │                    │
    ▼                    ▼
Last recorded < 5min  Record event
    │                    │
    ▼                    ▼
Skip (duplicate)    Add to map with timestamp
```

**Event Cleanup:** Every `MonitorHealth()` call removes expired events from the deduplication map to prevent unbounded growth.

### Health State Transitions

State transitions between Active, Degraded, and Unknown are recorded as separate events:

```text
┌──────────────────────────────────────────────────────────────┐
│ recordHealthTransitionEvents(current)                        │
└──────────────────────────────────────────────────────────────┘
                         │
                         ▼
        ┌────────────────────────────────┐
        │ Get previous health status     │
        │ (protected by previousMu)      │
        └────────────────────────────────┘
                         │
                 ┌───────┴────────┐
                 │                │
          previous = nil    previous exists
                 │                │
                 ▼                ▼
        ┌────────────────┐  ┌──────────────────┐
        │ Record:        │  │ Compare states   │
        │ "Unknown→      │  └──────────────────┘
        │  <current>"    │         │
        └────────────────┘    ┌────┴────┐
                              │         │
                         Same state  Different state
                              │         │
                              ▼         ▼
                         Skip      Record transition
                                   Update previous
```

**Example Transition Events:**
- `"Pacemaker health transitioned from Unknown to Active"` (Type: Normal)
- `"Pacemaker health transitioned from Active to Degraded"` (Type: Warning)
- `"Pacemaker health transitioned from Degraded to Active"` (Type: Normal)

### Integration with PacemakerCluster CR

The PacemakerCluster CR is populated by a status collector CronJob that runs every minute:

```text
Every 1 minute:
        │
        ▼
┌────────────────────────────────┐
│ Status Collector CronJob       │
│ (runs on pacemaker node)       │
└────────────────────────────────┘
        │
        ▼
Run: pcs status xml
        │
        ▼
Parse XML into structured data
        │
        ▼
Update PacemakerCluster CR .status
        │
        ▼
Set .status.lastUpdated timestamp
```

**CR Status Fields:** `state`, `lastUpdated`, `nodes[]` (name, status, IP), `warnings[]`, `errors[]`, `clusterSummary`

### Health vs Reconciliation

Health monitoring and reconciliation are independent operations with different purposes:

**Health Monitoring:**
- Purpose: Observability - report status to operators
- Actions: Read CR, update conditions, record events
- Does NOT: Modify Pacemaker, create jobs, trigger reconciliation

**Reconciliation:**
- Purpose: Operational correctness - fix drift
- Actions: Compare K8s vs Pacemaker, create jobs, add/remove nodes
- Does NOT: Update operator conditions, record health events

**Why Separate?** Health check failure shouldn't prevent drift correction, and vice versa. Both run independently in `sync()` for partial progress.

## Observability

The controller provides multiple observability mechanisms:

### Logging

- **klog.V(2):** Important state transitions (job startup, reconciliation)
- **klog.V(4):** Verbose debugging (sync start/end, early returns)
- **klog.Info:** Significant events (drift detected, jobs started)
- **klog.Warning:** Unexpected but recoverable conditions (stale CR, missing nodes)
- **klog.Error:** Errors requiring attention (API failures, job creation failures)

### Operator Conditions

Reported via StaticPodOperatorClient:

- **TNFJobControllersDegraded:** Set during bootstrap if job startup fails after retry
- **PacemakerHealthDegraded:** Set if Pacemaker cluster is unhealthy
- **PacemakerHealthProgressing:** Set if Pacemaker is transitioning between states

### Kubernetes Events

Recorded via EventRecorder:

- Health state transitions (Unknown→Active, Active→Degraded)
- Fencing failures (repeated every 24 hours if not resolved)
- Configuration drift detected
- Job completion/failure

### Metrics

Status exposed via PacemakerCluster CR:
- Cluster health (Active, Degraded, Unknown)
- Node membership list
- Last update timestamp
- Error messages
