# PacemakerLifecycleManager Architecture

## Overview

PacemakerLifecycleManager is a Kubernetes controller that manages the lifecycle of Two-Node Fencing (TNF) clusters in OpenShift. It runs a periodic sync loop (every 30 seconds) and responds to node events to maintain pacemaker cluster state in sync with Kubernetes control plane nodes.

## File Organization

```
pkg/tnf/operator/
├── lifecycle_manager.go              - Controller initialization and orchestration
├── lifecycle_job_controllers.go      - Job controller startup logic
├── lifecycle_health.go               - Pacemaker health monitoring
├── lifecycle_reconciliation.go       - Drift detection and reconciliation
├── lifecycle_cleanup.go              - Orphaned job cleanup
├── lifecycle_helpers.go              - Helper functions
├── lifecycle_job_controllers_test.go - Job controller tests
├── lifecycle_reconciliation_test.go  - Reconciliation tests
├── lifecycle_manager_test.go         - Health monitoring and cleanup tests
└── starter_test.go                   - Bootstrap tests
```

---

## Controller Initialization

**Function:** `NewPacemakerLifecycleManager()`  
**File:** `lifecycle_manager.go`

```
┌─────────────────────────────────────────────────────────────┐
│ NewPacemakerLifecycleManager()                              │
├─────────────────────────────────────────────────────────────┤
│ 1. Create REST client for PacemakerCluster CR              │
│ 2. Create shared informer for PacemakerCluster             │
│    - Watches: etcd.openshift.io/v1/PacemakerCluster        │
│    - Resync interval: 30 seconds                           │
│ 3. Initialize PacemakerLifecycleManager struct             │
│    - recordedEvents: map for event deduplication           │
│    - previous: nil (first sync treats as "from Unknown")   │
│ 4. Create factory.Controller                               │
│    - Sync function: c.sync                                 │
│    - Resync interval: 30 seconds                           │
│    - Watches: operatorClient, pacemakerInformer, nodeInfo  │
│ 5. Register node event handlers (Add/Update/Delete)        │
├─────────────────────────────────────────────────────────────┤
│ Returns:                                                    │
│ - controller: factory.Controller                           │
│ - manager: *PacemakerLifecycleManager                      │
│ - informer: SharedIndexInformer for PacemakerCluster       │
│ - error                                                     │
└─────────────────────────────────────────────────────────────┘
```

---

## Periodic Sync

**Function:** `sync()`  
**File:** `lifecycle_manager.go`  
**Trigger:** Every 30 seconds + informer events

**Note:** `StartJobControllers` runs in BOTH bootstrap and post-transition modes, but with different behavior based on `HasExternalEtcdCompletedTransition`. See "StartJobControllers" section below for details.

```
┌─────────────────────────────────────────────────────────────┐
│ sync(ctx, syncCtx)                                          │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Check HasExternalEtcdCompletedTransition
                    │
        ┌───────────┴───────────────────┐
        │ false (bootstrap)             │ true (post-transition)
        ▼                               ▼
┌──────────────────┐      ┌────────────────────────────────┐
│ BOOTSTRAP MODE   │      │ POST-TRANSITION MODE           │
│                  │      │                                │
│ Operations:      │      │ Operations:                    │
│ 1. StartJob      │      │ 1. StartJobControllers         │
│    Controllers   │      │    (dual-mode behavior)        │
│    (dual-mode)   │      │ 2. MonitorHealth               │
│                  │      │ 3. ReconcilePacemakerConfig    │
│                  │      │ 4. CleanupOrphanedJobs         │
└──────────────────┘      └────────────────────────────────┘
        │                               │
        └───────────┬───────────────────┘
                    ▼
        Aggregate errors from all operations
                    │
                    ▼
        Return errors.NewAggregate(errs)
```

**Notes:**
- Each operation continues even if others fail
- Errors are aggregated and returned
- Controller framework marks degraded if errors returned
- `StartJobControllers` is a single function that adapts its behavior based on transition state

---

## StartJobControllers

**Function:** `StartJobControllers()`  
**File:** `lifecycle_job_controllers.go`

**Important:** This is a **single function** that runs in both bootstrap and post-transition modes with different behavior based on `HasExternalEtcdCompletedTransition`. It is NOT two separate code paths, but one function with conditional logic.

**Bootstrap Mode** (before transition):
- Requires exactly 2 ready control plane nodes
- Uses exponential backoff retry (5s to 2min, ~10 min total)
- Sets `TNFJobControllersDegraded` condition on failure
- Mutex protects concurrent retry attempts

**Post-Transition Mode** (after transition):
- Accepts 1+ ready control plane nodes (handles single-node case during node replacement)
- Idempotent (safe to call repeatedly, handles operator restarts)
- No retry logic (controller framework retries sync() on error)
- No degraded condition handling (failures propagate to sync())

```
┌─────────────────────────────────────────────────────────────┐
│ StartJobControllers(ctx)                                    │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        nodeInformer.HasSynced() == false?
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       ▼
            Return   Continue
              nil
                        │
                        ▼
        ceohelpers.ListNodesFromInformer(nodeInformer)
                        │
                        ▼
        Check HasExternalEtcdCompletedTransition
                        │
        ┌───────────────┴───────────────┐
        │ false (bootstrap)             │ true (post-transition)
        ▼                               ▼
┌───────────────────┐         ┌──────────────────────┐
│ Bootstrap Path    │         │ Post-Transition Path │
└───────────────────┘         └──────────────────────┘
        │                               │
        ▼                               ▼
    len(nodes) > 2?              len(nodes) == 0?
        │                               │
    ┌───┴───┐                       ┌───┴───┐
    Yes     No                      Yes     No
    │       │                       │       │
    ▼       ▼                       ▼       ▼
  Return  Continue               Return  Continue
    nil                             nil
            │                               │
            ▼                               ▼
    len(nodes) < 2?              All nodes ready?
            │                     (supports 1 or 2 nodes)
        ┌───┴───┐                       ┌───┴───┐
        Yes     No                      Yes     No
        │       │                       │       │
        ▼       ▼                       ▼       ▼
      Return  Continue               Continue  Return
        nil                                      nil
                │                               │
                ▼                               ▼
        All nodes ready?        startJobControllersWithLock()
                │
            ┌───┴───┐
            Yes     No
            │       │
            ▼       ▼
          Continue  Return
                      nil
                │
                ▼
    retryInitialTransitionOrDegrade()
                │
                ▼
        Exponential backoff retry:
        - 5s, 10s, 20s, 40s, 80s
        - Cap: 120s
        - Steps: 9 (~10.6 min total)
                │
        ┌───────┴──────────┐
        │ Success          │ All retries exhausted
        ▼                  ▼
    Set condition:     Set condition:
    TNFJobControllers  TNFJobControllers
    Degraded=False     Degraded=True
```

### startJobControllersWithLock

```
┌─────────────────────────────────────────────────────────────┐
│ startJobControllersWithLock(ctx, nodes)                     │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Acquire startJobControllersMu
                    │
                    ▼
        startTnfJobcontrollersFunc(nodes, ...)
                    │
                    ▼
        Release startJobControllersMu
```

### startTnfJobcontrollers

```
┌─────────────────────────────────────────────────────────────┐
│ startTnfJobcontrollers(nodes, ctx, ...)                     │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Wait for etcd informer to sync
                    │
                    ▼
        waitForEtcdBootstrapCompleted()
                    │
                    ▼
        WaitForStableRevision()
        (all nodes at latest revision)
                    │
                    ▼
        For each node:
        - RunTNFJobController(JobTypeAuth, nodeTarget, nil)
        - RunTNFJobController(JobTypeAfterSetup, nodeTarget, nil)
          where nodeTarget = {Name: node.Name, UID: node.UID}
                    │
                    ▼
        RunTNFJobController(JobTypeSetup, nil, nil)
        RunTNFJobController(JobTypeFencing, nil, nil)
                    │
                    ▼
        waitForTnfAfterSetupJobsCompletion()

**Job Types:**
- **Node-specific jobs** (auth, after-setup): Pass `nodeTarget` to tie job to node lifecycle
- **Cluster-wide jobs** (setup, fencing): Pass `nil, nil` for both parameters
```

---

## Fencing Job Restart

**Function:** `handleFencingSecretChange()`  
**File:** `starter.go`  
**Trigger:** Fencing secret changes (add/update/delete)

The fencing job is created during initial bootstrap (via `StartJobControllers`), but must be restarted when fencing configuration changes. A secret informer watches for changes to fencing secrets and triggers job restart.

```
┌─────────────────────────────────────────────────────────────┐
│ Secret Informer Handler Registration (starter.go)          │
├─────────────────────────────────────────────────────────────┤
│ AddEventHandler on Secrets:                                │
│   - AddFunc: handleFencingSecretChange(nil, obj)           │
│   - UpdateFunc: handleFencingSecretChange(oldObj, newObj)  │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────────────────────┐
│ handleFencingSecretChange(ctx, oldObj, obj)                 │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Is this a fencing secret?
        (name matches "tnf-fencing-*")
                    │
            ┌───────┴────────┐
            No               Yes
            │                │
            ▼                ▼
        Return          Update event?
                             │
                    ┌────────┴─────────┐
                    Yes               No (Add/Delete)
                    │                 │
                    ▼                 │
        Compare old vs new data       │
                    │                 │
            ┌───────┴──────┐          │
            Changed   Not changed     │
            │              │          │
            ▼              ▼          │
        Continue       Return         │
            │                         │
            └─────────┬───────────────┘
                      ▼
        Check HasExternalEtcdCompletedTransition
                      │
            ┌─────────┴──────────┐
            false               true
            │                    │
            ▼                    ▼
        Return              RestartJobOrRunController
        (skip restart       (JobTypeFencing, nil, nil)
         during bootstrap)
```

**Fencing Secret Detection:**
- Secret names matching pattern `tnf-fencing-*` are considered fencing secrets
- Each control plane node has a corresponding secret (e.g., `tnf-fencing-master-0`)

**Change Detection:**
- For update events: compare old vs new secret data byte-by-byte
- Skip restart if data unchanged (avoids unnecessary job churn)
- For add/delete events: always trigger restart

**Transition Guard:**
- Before external etcd transition: fencing is managed by `StartJobControllers` during bootstrap
- After transition: secret changes trigger immediate fencing job restart
- This prevents conflicts between bootstrap-managed and secret-driven fencing

**Job Restart Flow:**
- `RestartJobOrRunController` with `nodeTarget=nil, scheduleOnNode=nil`
- Fencing job is cluster-wide (not node-specific)
- Job controller checks for existing job
- If exists: wait for completion, delete, then recreate
- If new: controller creates job immediately
- Timeout: 25 minutes (`FencingJobCompletedTimeout`)

### Fencing Configuration Triggers

Fencing (STONITH) is configured at three distinct points in the TNF lifecycle:

1. **Initial Bootstrap** (`startTnfJobcontrollers`)
   - During Day 1 cluster setup
   - Standalone fencing job runs as part of bootstrap job sequence
   - Triggered when both nodes transition to Ready
   - Job type: `JobTypeFencing`

2. **Secret Changes** (`handleFencingSecretChange`)
   - When fencing secrets are added, updated, or deleted
   - Only triggers after `ExternalEtcdTransitionCompleted=True`
   - Change detection: byte-by-byte comparison of secret data
   - Prevents unnecessary restarts for metadata-only updates
   - Restarts fencing job via `RestartJobOrRunController`

3. **Membership Changes** (`update-setup/runner.go`)
   - Inline during update-setup reconciliation
   - Triggered when nodes are added or removed from pacemaker
   - Calls `pcs.ConfigureFencing()` directly (not via job restart)
   - Updates STONITH configuration for current cluster membership
   - Runs on the target pacemaker-active node

**Why three triggers?** Different lifecycle phases require different approaches: bootstrap uses a standalone job for initial setup, secret changes need reactive updates without reconciliation, and membership changes need inline configuration to maintain STONITH consistency during drift correction.

---

## MonitorHealth

**Function:** `MonitorHealth()`  
**File:** `lifecycle_health.go`

```
┌─────────────────────────────────────────────────────────────┐
│ MonitorHealth(ctx)                                          │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        getPacemakerStatus(ctx)
                    │
                    ▼
        currentStatus == nil?
        (CR unchanged)
                    │
            ┌───────┴────────┐
            Yes              No
            │                │
            ▼                ▼
        Return nil      Continue
                            │
                            ▼
                updateOperatorStatus(ctx, current, previous)
                            │
                            ▼
                recordHealthCheckEvents(current, previous)
                            │
                            ▼
                    Return nil
```

### getPacemakerStatus

```
┌─────────────────────────────────────────────────────────────┐
│ getPacemakerStatus(ctx)                                     │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Read c.previous (with mutex lock)
                    │
                    ▼
        Get PacemakerCluster CR from informer
                    │
        ┌───────────┴───────────┐
        │ Error/NotFound        │ Found
        ▼                       ▼
    Return Unknown          Check CR.Status.LastUpdated
    (preserve previous)             │
                            ┌───────┴────────┐
                            Zero           Non-zero
                            │              │
                            ▼              ▼
                        Return        Check staleness
                        Unknown       (time since update)
                                          │
                                    ┌─────┴──────┐
                                    Stale      Fresh
                                    │          │
                                    ▼          ▼
                                Return     Timestamp
                                Unknown    changed?
                                              │
                                        ┌─────┴─────┐
                                        No          Yes
                                        │           │
                                        ▼           ▼
                                    Return nil  Build HealthStatus
                                    (skip)      from CR
                                                    │
                                                    ▼
                                                Update c.previous
                                                (if fresher, with mutex)
                                                    │
                                                    ▼
                                            Return (current, previous, nil)
```

### updateOperatorStatus

```
┌─────────────────────────────────────────────────────────────┐
│ updateOperatorStatus(ctx, status, previous)                 │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Switch on status.OverallStatus:
                    │
        ┌───────────┼───────────────────┐
        │           │                   │
        ▼           ▼                   ▼
    StatusError  StatusHealthy/     StatusUnknown
                 StatusWarning
        │           │                   │
        ▼           ▼                   ▼
    Set         Clear              Check grace
    Pacemaker   Pacemaker          period
    Degraded    Degraded               │
    condition   condition              │
                                   ┌───┴───┐
                                   │       │
                                Expired  Active
                                   │       │
                                   ▼       ▼
                                Set     Return
                                Degraded  nil
                                condition
```

### recordHealthCheckEvents

```
┌─────────────────────────────────────────────────────────────┐
│ recordHealthCheckEvents(current, previous)                  │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        cleanupExpiredEvents()
        (remove old entries from recordedEvents map)
                    │
                    ▼
        For each warning in current.Warnings:
                    │
                    ▼
            Is fencing event?
                    │
            ┌───────┴───────┐
            Yes             No
            │               │
            ▼               ▼
        shouldRecord     shouldRecord
        (24h dedup)?     (5min dedup)?
            │               │
        ┌───┴───┐       ┌───┴───┐
        Yes     No      Yes     No
        │       │       │       │
        ▼       ▼       ▼       ▼
    Record   Skip    Record   Skip
    Warning          Warning
    Event            Event
        │               │
        └───────┬───────┘
                │
                ▼
        For each error in current.Errors:
                    │
                    ▼
            shouldRecordEvent(5min dedup)?
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       ▼
            Record   Skip
            Error
            Event
                │
                ▼
        recordHealthTransitionEvents(current, previous)
```

### recordHealthTransitionEvents

Detects and records health state transitions:
- **PacemakerHealthy**: fires on transition to operationally healthy
- **PacemakerWarningsCleared**: fires when warnings are resolved

```
┌─────────────────────────────────────────────────────────────┐
│ recordHealthTransitionEvents(current, previous)             │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Determine previous state:
        - previousWasUnknown = (previous == nil || StatusUnknown)
        - previousWasDegraded = (previous.Status == Error)
        - previousHadWarnings = (len(previous.Warnings) > 0)
                    │
                    ▼
        Determine current state:
        - operationallyHealthy = (StatusHealthy || StatusWarning)
        - currentHasNoWarnings = (len(current.Warnings) == 0)
                    │
                    ▼
        Transitioning to operationally healthy?
        (operationallyHealthy && (previousWasDegraded || previousWasUnknown))
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       │
        Record          │
        PacemakerHealthy│
        Event           │
                │       │
                └───┬───┘
                    │
                    ▼
        Warnings cleared?
        (previousHadWarnings && currentHasNoWarnings)
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       ▼
        Record      Return
        WarningsCleared
        Event

**Transition Examples:**
- Unknown → Healthy: fires PacemakerHealthy
- Error → Healthy: fires PacemakerHealthy
- Warning → Healthy: fires both PacemakerHealthy and WarningsCleared
- Error+Warning → Error: fires WarningsCleared only
- Error+Warning → Healthy: fires both events
```

---

## ReconcilePacemakerConfig

**Function:** `ReconcilePacemakerConfig()`  
**File:** `lifecycle_reconciliation.go`

Detects configuration drift between Kubernetes and pacemaker, triggering reconciliation when membership diverges. The flow includes several safety gates to prevent reconciliation in unsafe states:

- **detectDrift**: Compares K8s node names/IPs against pacemaker's **configured membership** (not online/offline status) to identify true configuration drift
- **reconcilePacemakerConfigMutex**: Serializes the check-and-trigger path after drift detection to prevent time-of-check-time-of-use races where multiple goroutines would redundantly create ConfigMaps and trigger jobs
- **isUpdateSetupRunning**: Prevents concurrent reconciliation attempts (double-check pattern under mutex)
- **getIntersection**: Safety gate that verifies at least one node exists in both K8s and pacemaker configurations; if empty, returns error requiring manual intervention (prevents scenarios where the two systems have completely diverged with no common nodes)
- **updateSetup**: Triggered only when drift exists, no job is running, and intersection is non-empty

```
┌─────────────────────────────────────────────────────────────┐
│ ReconcilePacemakerConfig(ctx)                               │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        nodeInformer.HasSynced() == false?
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       ▼
            Return   Continue
              nil
                        │
                        ▼
            ceohelpers.ListNodesFromInformer(nodeInformer)
                        │
                        ▼
            len(k8sNodes) > 2?
                        │
                    ┌───┴───┐
                    Yes     No
                    │       │
                    ▼       ▼
                Return   Continue
                  nil
                            │
                            ▼
                Any node not ready?
                            │
                        ┌───┴───┐
                        Yes     No
                        │       │
                        ▼       ▼
                    Return   Continue
                      nil
                                │
                                ▼
                    getPacemakerNodes()
                    (from PacemakerCluster CR)
                                │
                        ┌───────┴────────┐
                        │ Error          │ Success
                        ▼                ▼
                    Return nil       Continue
                    (CR not found)
                                        │
                                        ▼
                        detectDrift(k8sNodes, pacemakerNodes)
                                        │
                                ┌───────┴────────┐
                                │ No drift       │ Drift detected
                                ▼                ▼
                            Return nil       Continue
                                                │
                                                ▼
                            Acquire reconcilePacemakerConfigMutex
                            (serialize check-and-trigger to prevent time-of-check-time-of-use race)
                                                │
                                                ▼
                                isUpdateSetupRunning(ctx)?
                                                │
                                        ┌───────┴────────┐
                                        Yes              No
                                        │                │
                                        ▼                ▼
                                    Return nil       Continue
                                                        │
                                                        ▼
                                        getIntersection(k8sNodes, pacemakerNodes)
                                                        │
                                                ┌───────┴────────┐
                                                │ Empty          │ Non-empty
                                                ▼                ▼
                                            Return error     updateSetup()
```

### detectDrift

```
┌─────────────────────────────────────────────────────────────┐
│ detectDrift(k8sNodes, pacemakerNodes)                       │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        len(k8sNodes) != len(pacemakerNodes)?
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       ▼
            Return   Continue
              true
                        │
                        ▼
        For each k8sNode:
                        │
                        ▼
            Node exists in pacemakerNodes?
                        │
                    ┌───┴───┐
                    No      Yes
                    │       │
                    ▼       ▼
                Return   Compare IPs
                  true       │
                        ┌────┴────┐
                        │ Differ  │ Match
                        ▼         ▼
                    Return     Continue
                      true
                                │
                                ▼
        For each pacemakerNode:
                                │
                                ▼
            Node exists in k8sNodes?
                                │
                            ┌───┴───┐
                            No      Yes
                            │       │
                            ▼       ▼
                        Return   Continue
                          true
                                    │
                                    ▼
                            Return false
```

### updateSetup

```
┌─────────────────────────────────────────────────────────────┐
│ updateSetup(validTargetNodes, allK8sNodes, ctx, ...)        │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        getNextUpdateSetupGeneration()
        (increment counter - caller holds reconcilePacemakerConfigMutex)
                    │
                    ▼
        Pick target node (first in validTargetNodes)
                    │
                    ▼
        getPacemakerMembership(ctx)
                    │
                    ▼
        buildK8sNodeMap(allK8sNodes)
                    │
                    ▼
        determineReconciliationActions(k8sMap, pacemakerMap)
        -> returns (nodesToRemove, nodesToAdd)
                    │
                    ▼
        Create ConfigMap:
        - Name: tnf-update-setup-<generation>
        - Data:
          * nodes: encoded node list
          * nodesToAdd: encoded string list
          * nodesToRemove: encoded string list
          * generation: number
          * timestamp: RFC3339
                    │
                    ▼
        Run auth jobs on all nodes
        (ensures pacemaker authentication is current before reconciliation)
                    │
                    ▼
        jobs.RestartJobOrRunController(
            JobTypeUpdateSetup,
            nil,                    // nodeTarget (cluster-wide)
            &targetNode.Name,       // scheduleOnNode (run on pacemaker node)
            ...
        )
                    │
                    ▼
        Wait for update-setup job completion
                    │
                    ▼
        Run after-setup jobs on all nodes
        (via runJobsOnNodes with NodeTarget)
```

**What the update-setup job does internally** (`pkg/tnf/update-setup/runner.go`):

The update-setup job runs on one pacemaker-active node and performs the following operations:

1. **Read reconciliation decisions** from ConfigMap (`nodesToAdd`, `nodesToRemove`)
2. **Remove nodes** from pacemaker cluster (if `nodesToRemove` is non-empty):
   - Run `pcs cluster node remove <node>` for each node
   - Remove unstarted etcd members (those with IPs not matching current K8s nodes)
3. **Add nodes** to pacemaker cluster (if `nodesToAdd` is non-empty):
   - Wait for etcd revision stability (ensures new nodes have etcd manifests)
   - Run `pcs cluster node add <node>` for each node
4. **Update fencing configuration** (if any nodes were added or removed):
   - Call `pcs.ConfigureFencing()` to update STONITH for all current nodes
   - **NOTE:** Fencing is configured three ways: (1) initial bootstrap via standalone fencing job, (2) when fencing secrets change (fencing job is restarted, see "Fencing Job Restart on Secret Changes"), (3) inline here during update-setup when node membership changes
5. **Update etcd resource** with current node IPs (if any membership changes occurred)
6. **Force new cluster and restart** (if nodes were added):
   - Set `force_new_cluster` attribute on current node
   - Enable and start cluster on all nodes
7. **Sync and start cluster** (always runs at end)

---

## CleanupOrphanedJobs

**Function:** `CleanupOrphanedJobs()`  
**File:** `lifecycle_cleanup.go`

```
┌─────────────────────────────────────────────────────────────┐
│ CleanupOrphanedJobs(ctx)                                    │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        nodeInformer.HasSynced() == false?
                    │
                ┌───┴───┐
                Yes     No
                │       │
                ▼       ▼
            Return   Continue
              nil
                        │
                        ▼
            ceohelpers.ListNodesFromInformer(nodeInformer)
                        │
                        ▼
            cleanupOrphanedJobs(ctx, k8sNodes)
```

### cleanupOrphanedJobs

```
┌─────────────────────────────────────────────────────────────┐
│ cleanupOrphanedJobs(ctx, k8sNodes)                          │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Build set of current node UIDs
        (map[string]bool of all node.UID values)
                    │
                    ▼
        List node-specific TNF jobs:
        Label: app.kubernetes.io/name in
               (tnf-auth-job, tnf-after-setup-job)
        Note: Cluster-wide jobs (update-setup, setup, fencing)
              are excluded - they have no node label
                    │
                    ▼
        For each job:
                    │
                    ▼
            Job has "node" label?
                    │
                ┌───┴───┐
                No      Yes
                │       │
                ▼       ▼
              Skip   jobNodeUID = job.Labels["node"]
                     (node UID stored in label)
                                │
                                ▼
                     jobNodeUID exists in currentNodeUIDs?
                                │
                            ┌───┴───┐
                            Yes     No
                            │       │
                            ▼       ▼
                          Skip   Delete job

**Node Replacement Handling:**
- Job labels store node UID, not node name
- When node is replaced (same name, different UID):
  * Old jobs have old UID → not in current set → deleted
  * New jobs have new UID → in current set → kept
- Prevents orphaned jobs from replaced nodes
```

---

## Node Event Handlers

**Function:** `RegisterNodeEventHandlers()`  
**File:** `lifecycle_manager.go`

### Update Handler

```
┌─────────────────────────────────────────────────────────────┐
│ Node Update Event                                           │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        Node transitioned to Ready?
        (NotReady -> Ready)
                    │
            ┌───────┴────────┐
            Yes              No
            │                │
            ▼                ▼
        go StartJob      Check if IP
        Controllers()    changed
                            │
                        ┌───┴───┐
                        No      Yes (while Ready)
                        │       │
                        ▼       ▼
                      Return  go Reconcile
                              PacemakerConfig()
```

### Add Handler

```
┌─────────────────────────────────────────────────────────────┐
│ Node Add Event                                              │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        go ReconcilePacemakerConfig()
```

### Delete Handler

```
┌─────────────────────────────────────────────────────────────┐
│ Node Delete Event                                           │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
        go ReconcilePacemakerConfig()
```

---

## Data Structures

### NodeTarget

**File:** `pkg/tnf/pkg/jobs/tnf.go`

```go
type NodeTarget struct {
    Name string  // Node name for scheduling and job naming
    UID  string  // Node UID for job labeling (enables cleanup on node deletion/replacement)
}
```

Used by `RunTNFJobController` and `RestartJobOrRunController` to distinguish between:
- **Node-specific jobs** (auth, after-setup): Pass `&NodeTarget{Name: node.Name, UID: string(node.UID)}`
  - Job named with node suffix (e.g., `tnf-auth-job-master-0-...`)
  - Job labeled with node UID for cleanup on node deletion/replacement
  - Pod scheduled on the specific node
- **Cluster-wide jobs** (setup, fencing, update-setup): Pass `nil` for nodeTarget
  - Job has standard name (e.g., `tnf-setup-job`)
  - No node label (survives node deletion/replacement)
  - Optional scheduling hint via separate parameter

### PacemakerLifecycleManager

```go
type PacemakerLifecycleManager struct {
    operatorClient             v1helpers.StaticPodOperatorClient
    kubeClient                 kubernetes.Interface
    eventRecorder              events.Recorder
    pacemakerInformer          cache.SharedIndexInformer
    nodeInformer               cache.SharedIndexInformer
    controllerContext          *controllercmd.ControllerContext
    kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces
    etcdInformer               operatorv1informers.EtcdInformer
    
    // Event deduplication
    recordedEventsMu sync.Mutex
    recordedEvents   map[string]time.Time
    
    // Health status tracking
    previousMu sync.Mutex
    previous   *pacemaker.HealthStatus
    
    // Job controller startup protection
    startJobControllersMu sync.Mutex
    
    // Lifecycle context for goroutines spawned by event handlers
    // Created with context.WithCancel() during initialization
    // Used by node Add/Update/Delete event handlers when spawning goroutines
    lifecycleCtx       context.Context
    lifecycleCtxCancel context.CancelFunc
}
```

### Operator Conditions

#### PacemakerHealthCheckDegraded

- **Type:** `PacemakerHealthCheckDegraded`
- **Status:** True when pacemaker cluster has errors
- **Reason:** `PacemakerUnhealthy`
- **Set by:** `updateOperatorStatus()` when `status.OverallStatus == StatusError`
- **Cleared by:** `updateOperatorStatus()` when `status.OverallStatus == StatusHealthy` or `StatusWarning`

#### TNFJobControllersDegraded

- **Type:** `TNFJobControllersDegraded`
- **Status:** True when bootstrap retry exhausted
- **Reason:** `SetupFailed` (degraded) or `AsExpected` (cleared)
- **Set by:** `retryInitialTransitionOrDegrade()` after all retries fail
- **Cleared by:** `retryInitialTransitionOrDegrade()` on successful startup

### Concurrency Protection

TNF lifecycle uses both **instance-level** (struct fields) and **package-level** (module variables) synchronization primitives.

#### Instance-Level Mutexes (PacemakerLifecycleManager struct fields)

**startJobControllersMu**
- **Protects:** `startTnfJobcontrollersFunc()` execution
- **Prevents:** Concurrent job controller creation, duplicate waits
- **Used by:** `startJobControllersWithLock()`
- **Scope:** Per-controller instance

**recordedEventsMu**
- **Protects:** `recordedEvents` map
- **Prevents:** Concurrent map access during event recording and cleanup
- **Used by:** Event recording and cleanup
- **Scope:** Per-controller instance

**previousMu**
- **Protects:** `previous` HealthStatus
- **Prevents:** Race during health monitoring when reading/writing previous state
- **Used by:** `getPacemakerStatus()`, `updateOperatorStatus()`
- **Scope:** Per-controller instance

#### Package-Level Synchronization (lifecycle_reconciliation.go)

These are **shared across all instances** of PacemakerLifecycleManager, though typically only one instance exists.

**reconcilePacemakerConfigMutex** (`var reconcilePacemakerConfigMutex sync.Mutex`)
- **Protects:** Reconciliation trigger path (drift detection → ConfigMap creation → job start)
- **Prevents:** Time-of-check-time-of-use race where multiple goroutines detect drift and redundantly trigger reconciliation
- **Scope:** Serializes `isUpdateSetupRunning()` check and `updateSetup()` call
- **Used by:** `ReconcilePacemakerConfig()` after drift detection
- **Package-level:** Yes - defined as module variable, not struct field
- **Also protects:** `updateSetupGeneration` counter increment in `getNextUpdateSetupGeneration()`

**updateSetupGeneration** (`var updateSetupGeneration int64 = 0`)
- **Purpose:** Monotonic counter for update-setup ConfigMap generation
- **Ensures:** When multiple reconciliation events arrive close together, highest generation wins
- **Critical for:** OCPBUGS-84695 fix - prevents stale ConfigMaps from being processed
- **Incremented by:** `getNextUpdateSetupGeneration()` (protected by `reconcilePacemakerConfigMutex`)
- **Used in:** ConfigMap naming (`tnf-update-setup-<generation>`) and data (`generation` field)
- **Package-level:** Yes - shared state across all reconciliation attempts

**Why package-level?** These protect resources that are inherently shared at the cluster level (ConfigMaps, jobs) rather than controller instance state. Even if multiple controller instances existed (e.g., during rollout), they must serialize access to these shared cluster resources.

### Event Deduplication

Events are deduplicated using a map with event key -> last recorded time:

- **Event key format:** `<type>:<reason>:<object>:<message-hash>`
- **Deduplication windows:**
  - Default events: 5 minutes
  - Fencing events: 24 hours
- **Cleanup:** Events older than their window are removed

### External Etcd Transition

The controller operates in two modes based on `HasExternalEtcdCompletedTransition`:

**Before transition (bootstrap):**
- Only `StartJobControllers` runs
- Requires exactly 2 ready control plane nodes
- Uses exponential backoff retry
- Sets `TNFJobControllersDegraded` condition on failure

**After transition (post-transition):**
- All 4 operations run (`StartJobControllers`, `MonitorHealth`, `ReconcilePacemakerConfig`, `CleanupOrphanedJobs`)
- `StartJobControllers` accepts 1+ ready nodes (handles single-node case)
- No retry logic (controller framework handles retries)
- No special condition handling
