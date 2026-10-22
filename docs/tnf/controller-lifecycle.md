# Controller lifecycle

## Startup sequence

```text
HandleDualReplicaClusters
  |
  +-- wait for Infrastructure informer; confirm ExternalEtcd topology
  +-- start external-etcd support and TNF static-resource controllers
  +-- background Pacemaker startup
        |
        +-- wait for PacemakerCluster CRD Established
        +-- start ClusterVersion informer for console
        +-- construct lifecycle controller, retrying transient creation errors
        +-- start PacemakerCluster and newly requested namespace informers
        +-- run lifecycle controller with one worker
        +-- run metrics and console notification controllers
```

[starter.go](../../pkg/tnf/operator/starter.go) owns this sequence. Informer
construction does not itself run a cache. The caller starts the Pacemaker
informer, the shared config factory, and any namespace informer types requested
after the main operator started its factory. The lifecycle factory waits for its
registered caches before processing sync work.

Constructor failures set `PacemakerLifecycleManagerDegraded` with reason
`StartupFailed` and are retried. That startup condition is cleared after successful
construction; runtime health is reported by the runtime controllers.

## Sync flow

```text
sync
  |
  +-- startJobControllers
  |     |
  |     +-- node cache not synced? wait
  |     +-- before transition?
  |     |     +-- require exactly two Ready nodes
  |     |     +-- retry bootstrap startup with exponential backoff
  |     |     +-- wait for etcd bootstrap and stable revision
  |     |     +-- start existing job controllers
  |     |
  |     +-- after transition?
  |           +-- ensure existing per-node and cluster controllers
  |           +-- ensure collector, health, and certificate watcher
  |
  +-- reconcileGeneration
        +-- Managed and transition complete?
        +-- validate desired node identities
        +-- wait for old Jobs and Pods to stop
        +-- retire stopped legacy update-setup Jobs
        +-- read recorded generations and fresh Pacemaker membership
        +-- reevaluate control-plane MCO policy
        +-- record generation / create attempt / observe completion / prune
```

Bootstrap startup uses exponential backoff from five seconds to a two-minute
cap, for roughly ten minutes overall. A persistent startup failure sets
`TNFJobControllersDegraded`. Runtime startup is idempotent and retried through the
controller framework. Node readiness is checked at admission to an individual
Job; runtime controller startup itself does not require every node Ready.

The lifecycle resync interval is one minute. The health controller has its own
30-second resync. The collector's normal CronJob schedule is every two minutes.
These are independent mechanisms and are not interchangeable.

## Events and queue behavior

| Event | Queue response and effect |
| --- | --- |
| Node added | Ensure per-node controllers; compare the new identity with recorded desired state. |
| Node Ready changes | Recompute eligible execution targets; retain registered membership. |
| Node UID or IP changes | Record new desired input after old execution stops; revalidate live state in the runner. |
| Node deleted | Compare the remaining registered set; reject an empty set rather than inferring "remove everything." |
| PacemakerCluster changes | Reevaluate fresh observed membership and convergence. |
| Control-plane Node MCO annotations change | Reevaluate deferral or necessary recovery. |
| ConfigMap, Job, or Pod changes | Observe persisted input, completion, and whether old execution has stopped. |
| Secret, CronJob, or DaemonSet changes | Revisit startup/runtime resources and their dependent configuration. |

Handlers enqueue only. Repeated events can collapse into the same queue item;
every sync derives its decision from current inputs. There is no per-event
reconciliation goroutine or captured Node object used as the final authority.
Factory error handling retries errors, and the periodic resync covers missed
notifications.

## Context and startup guards

The operator supplies the context used by controller Run loops, informer stop
channels, bootstrap waits, retries, and API calls. Job-controller constructors use
the existing context and duplicate-prevention mechanisms.

The lifecycle manager retains startup mutexes for job controllers, health, and
status collection. These guard startup state; they are not a global lock around
external PCS work. The single worker serializes decisions, and persisted
Job/Pod checks serialize actual update-setup execution across restarts.

The old full-refactor design's package-level generation counter and separate
reconciliation mutex are unnecessary here. API records hold generation state.

## Cancellation and shutdown

1. Cancellation stops future controller work and signals informer shutdown.
2. Bootstrap polls, retry loops, and API operations observe the cancelled context.
3. No detached Node-handler goroutine remains to start new reconciliation.
4. An already submitted Kubernetes Job remains an independent workload until
   completion, failure, or its active deadline.
5. The next CEO process reconstructs generation/attempt state and waits for any
   earlier active or terminating execution before creating another Job.

Cancelling CEO is not a mechanism for pausing TNF recovery. Deliberately holding
a node offline needs the separately tracked PHC pause contract.

## Health monitoring

The lifecycle manager starts the existing health controller after transition.
It shares the PacemakerCluster informer but owns its own sync and status logic.

```text
PacemakerCluster observation
       |
       +-- missing / no status / stale --> Unknown
       |                                  |
       |                         use previous valid timestamp
       |                         apply existing unknown grace
       |
       +-- fresh --> evaluate cluster, nodes, resources, fencing
                         |
                         +-- Healthy / Warning --> clear PHC Degraded
                         +-- Error -------------> set PHC Degraded
                         +-- publish deduplicated events
```

Freshness is determined from `status.lastUpdated`. The staleness threshold and
unknown-status degradation threshold are both five minutes. Unknown observations
do not replace the previous valid health state. On the first unknown observation
without a valid predecessor, the controller does not immediately degrade.
An unchanged valid observation is skipped to avoid redundant status writes.

Health evaluation distinguishes a current operational error from a warning about
redundancy. An offline node, unavailable critical resource, or maintenance state
can be an error; warning states remain observable without forcing degradation.
The exact resource/fencing rules live in
[healthcheck.go](../../pkg/tnf/pkg/pacemaker/healthcheck.go), together with the
existing disruption tracking.

Conditions are updated only when their relevant value/message changes. Event
history is deduplicated with the existing five-minute default window and
24-hour fencing window. Lifecycle part 3 does not replace that controller or
introduce a general monitor suppression.

## Conditions, events, metrics, and logs

| Signal | Owner and interpretation |
| --- | --- |
| `PacemakerLifecycleManagerDegraded` | Lifecycle construction failure. |
| `TNFJobControllersDegraded` | Bootstrap job-controller startup failure. |
| `TNFSetupJobAvailable` | Initial setup completion; remains true after durable transition. |
| `TNFUpdateSetupJobProgressing` | A recorded attempt is running or old execution is terminating. |
| `TNFUpdateSetupJobDegraded` | Execution configuration exhausted its six attempts; preserved until successful reconciliation. |
| `PacemakerHealthCheckDegraded` | Independent observed Pacemaker health. |
| Pacemaker events and metrics | Independent health/disruption history and current CR-derived metrics. |

Generation deferral is reported with reason `Deferred`. Retry exhaustion uses
`MaxRetriesExceeded`; a fresh generation may retry after ten minutes. Generic
lifecycle sync errors use factory error handling and are distinct from the
explicit retry-exhaustion policy.

Use CEO logs to establish which decision was made, Job/Pod status to establish
whether execution stopped, and the CR timestamp to establish whether observed
membership is fresh. A healthy old CR is not proof of current convergence.

## Troubleshooting walkthrough

Start with read-only API inspection:

```sh
oc get etcd.operator.openshift.io cluster -o yaml
oc get nodes -l node-role.kubernetes.io/control-plane -o yaml
oc get clusterversion version -o yaml
oc get pacemakerclusters.etcd.openshift.io cluster -o yaml
oc -n openshift-etcd get configmaps -l app.kubernetes.io/component=tnf-update-setup -o yaml
oc -n openshift-etcd get jobs -l app.kubernetes.io/name=tnf-update-setup-job -o yaml
oc -n openshift-etcd get pods -o wide
oc -n openshift-etcd-operator logs deployment/etcd-operator -c etcd-operator
```

For a specific attempt, inspect its logs, owner reference, generation/attempt
labels, and `TNF_UPDATE_SETUP_CONFIGMAP` / `TNF_UPDATE_SETUP_CONFIGMAP_UID` values.
Compare those with the exact ConfigMap, rather than choosing the newest record
and assuming it was the input to an older Job.

**No initial jobs:** inspect topology, node-cache sync, node readiness, etcd
bootstrap, and stable revision prerequisites.

**No runtime attempt:** check transition and Managed state; validate node
identities, fresh CR membership, MCO node annotations, and an eligible
executor. Look for earlier Jobs with active or terminating Pods.

**After-setup waits:** check the scheduled node UID, newest record, completion
annotations, and completed Job owner/UID/generation binding. A completed legacy
setup Job is insufficient after transition.

**Repeated failed repair:** inspect local PCS/CIB errors, fencing credentials,
etcd member cleanup, and node availability. Preserve generation records and
Job logs as evidence. A missing or malformed durable repair claim is an error;
it must not silently become a fresh destructive claim.

**Healthy reconciliation but PHC degraded:** inspect independent resource health,
fencing conditions, collector timestamp, and disruption history. Membership
convergence alone does not establish that every resource is healthy.
