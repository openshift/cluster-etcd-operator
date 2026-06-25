# TNF (Two-Node with Fencing) Lifecycle Documentation

This directory contains comprehensive documentation for the TNF lifecycle system in the cluster-etcd-operator.

## Quick Reference

| Topic | Document | Description |
|-------|----------|-------------|
| **System Design** | [architecture-overview.md](architecture-overview.md) | High-level architecture, dual-mode operation, component overview |
| **Controller Behavior** | [controller-lifecycle.md](controller-lifecycle.md) | Startup sequence, periodic sync loop, event handling, health monitoring |
| **Job Management** | [job-controllers.md](job-controllers.md) | Bootstrap vs runtime job behavior, retry logic, node selection |
| **Drift Detection** | [reconciliation.md](reconciliation.md) | When reconciliation runs, drift scenarios, safety checks |

## System Overview

The TNF lifecycle system manages the integration between OpenShift control plane nodes and a Pacemaker high-availability cluster. It operates in two modes:

**Bootstrap Mode** (Pre-Transition)
- Drives initial etcd transition from static pods to Pacemaker control
- Requires exactly 2 ready control plane nodes
- Uses exponential backoff retry logic
- Sets degraded conditions on failure

**Runtime Mode** (Post-Transition)
- Monitors Pacemaker cluster health every 30 seconds
- Detects drift between Kubernetes nodes and Pacemaker membership
- Automatically reconciles differences by adding/removing nodes
- Supports single-node scenarios during node replacement

## Key Components

**PacemakerLifecycleManager** (`pkg/tnf/operator/lifecycle_manager.go`)
- Main controller coordinating all TNF operations
- Runs periodic sync loop (every 30 seconds)
- Registers event handlers for node Add/Update/Delete

**StartJobControllers** (`pkg/tnf/operator/lifecycle_job_controllers.go`)
- Manages TNF job lifecycle
- Dual-mode behavior (bootstrap vs runtime)
- Handles operator restarts gracefully

**ReconcilePacemakerConfig** (`pkg/tnf/operator/lifecycle_reconciliation.go`)
- Detects and fixes drift between K8s and Pacemaker
- Creates update-setup ConfigMaps with reconciliation decisions
- Protected by mutex to serialize reconciliation attempts

**MonitorHealth** (`pkg/tnf/operator/lifecycle_health.go`)
- Monitors Pacemaker cluster health
- Updates operator conditions (Degraded, Progressing, Available)
- Records events with deduplication

## Job Types

| Job Type | Scope | When | Purpose |
|----------|-------|------|---------|
| **auth** | Per-node | Bootstrap + reconciliation | Set hacluster password |
| **setup** | Cluster | Bootstrap only | Initial Pacemaker cluster creation |
| **after-setup** | Per-node | Bootstrap only | Add node to newly created cluster |
| **fencing** | Cluster | Bootstrap + secret changes | Configure STONITH fencing agents |
| **update-setup** | Cluster | Reconciliation | Add/remove nodes from existing cluster |

## Common Workflows

**Initial Bootstrap**
1. Wait for etcd bootstrap completion
2. Wait for all nodes at latest revision
3. Run auth jobs on all nodes
4. Run setup job (creates Pacemaker cluster)
5. Run fencing job (configures STONITH)
6. Run after-setup jobs on all nodes
7. Mark transition complete

**Node Replacement (Reconciliation)**
1. Detect drift (K8s has different nodes than Pacemaker)
2. Create update-setup ConfigMap with desired state
3. Run auth jobs on all current K8s nodes
4. Run update-setup job (add/remove nodes from Pacemaker)
5. Run after-setup jobs on all current K8s nodes
6. Clean up orphaned jobs for deleted nodes

**Health Monitoring**
1. Read PacemakerCluster CR status (updated every 1 minute)
2. Check staleness (CR updated recently?)
3. Parse cluster state (nodes online, fencing status, errors)
4. Update operator conditions
5. Record events with deduplication

## Design Principles

**Self-Healing**
- Periodic reconciliation detects and fixes drift automatically
- Recovers from missed events or operator downtime

**Graceful Degradation**
- Operations continue independently
- Health check failure doesn't prevent reconciliation

**Idempotency**
- Safe to call repeatedly
- Operations detect current state and only act when needed

**Event + Periodic Hybrid**
- Node events trigger immediate action
- Periodic sync provides backup detection and recovery
