# TNF lifecycle architecture

## Two operating phases

`ExternalEtcdHasCompletedTransition=True` on the etcd Operator resource is the
durable boundary between initial installation and runtime reconciliation.

| Phase | Admission | Work |
| --- | --- | --- |
| Bootstrap | Exactly two Ready control-plane nodes, completed etcd bootstrap, stable revision | Existing auth, setup, fencing, and after-setup controllers. |
| Runtime | A nonempty registered node set for controller startup; valid one- or two-node identities for generation work | Existing runtime controllers plus recorded-generation reconciliation. An actual Job needs an eligible execution node. |

Startup of runtime controllers does not require every node to be Ready. That
would prevent the recovery those controllers are intended to provide.

The initial setup controller remains registered in both phases. Placement changes
alone refresh the shared framework's execution targets without deleting Jobs.
Setup explicitly includes eligible executor identities in its configuration, so
changes can delete even a completed setup Job. Its creation admission hook refuses
replacement after the durable transition, preserving existing Available and
Progressing status while creation is deferred. The shared JobController has no
setup-specific transition branch, and the setup runner retains its defensive
transition check. Resetting the transition condition and deleting the Job permits
a deliberate bootstrap rerun.

## Component architecture

```text
                        CEO operator context
                                 |
              +------------------+-------------------+
              |                                      |
   shared Kubernetes informers              PacemakerCluster informer
   Etcd / Nodes                                      |
   ConfigMaps / Jobs / Pods / Secrets                |
   CronJobs / DaemonSets                             |
              +------------------+-------------------+
                                 |
                       lifecycle work queue
                      one worker, 1-minute resync
                                 |
                   +-------------+----------------+
                   |                              |
        start existing controllers        reconcileGeneration
                   |                              |
       +-----------+-----------+          immutable ConfigMap N
       |           |           |                  |
      auth       setup      fencing       exact name + UID binding
       |           |           |                  |
       +------- after-setup ---+          Job N-A on eligible node
                   |                              |
                   +---------------------- recorded runner
                                                  |
                                  live API guard + local CIB
                                                  |
                                    Pacemaker / Corosync / etcd
```

The controller uses informer events to enqueue decisions, then uses live reads
for persisted records, Jobs, Pods, and upgrade checks. Informers are registered
for those resources as well: live reads prevent use of old decisions, and watches
make changed state enqueue new decisions promptly. The one-minute resync is a
fallback, rather than a substitute for relevant watches.

## Responsibilities

### Lifecycle controller

[The lifecycle manager](../../pkg/tnf/operator/lifecycle_manager.go) owns startup
and its work queue. [Generation reconciliation](../../pkg/tnf/operator/lifecycle_reconciliation.go)
validates desired identities, waits for old executions, checks observed
membership and upgrade state, persists a generation, creates attempts, records
completion, and prunes history.

No custom Node event handler starts reconciliation goroutines. Add, Update, and
Delete events all converge on the same sync function.

### Generation contract

[The generation package](../../pkg/tnf/pkg/generation/generation.go) defines a
versioned immutable snapshot, canonical node identities, metadata labels, exact
record decoding, live identity validation, and membership comparisons. It also
provides the shared control-plane MCO policy and after-setup dependency checks.

Generation numbers and attempts come from API objects. The controller can
reconstruct them after restart; there is no process-local generation counter.

### Existing job framework

Auth, setup, fencing, and after-setup continue to use
[the job framework](job-controllers.md). Its skip-capable hooks, fresh node UID
lookup, configuration drift detection, and retry degradation handling remain in
place. Recorded update-setup attempts have a separate persisted state machine.

A completed legacy setup Job is the dependency before transition. Afterward,
fencing uses the durable transition condition, while after-setup requires the
current node's matching recorded-generation completion.

### Independent runtime controllers

PLM manages health-controller startup and lifetime. After transition,
[`runPacemakerHealthCheckController`](../../pkg/tnf/operator/lifecycle_manager.go)
creates the existing health controller with PLM's operator/Kubernetes clients,
event recorder, and PacemakerCluster informer, then starts it with the shared
lifecycle context. It does not create another Pacemaker informer or API watch.
Context cancellation stops both controller loops.

The health controller retains its own factory work queue and 30-second resync;
PLM's generation reconciliation uses a separate queue and one-minute resync.
Health reporting does not wait for a successful repair or run inside generation
reconciliation. PHC condition evaluation is not an admission dependency for
update-setup. Sharing dependencies does not require merging those control loops.

```text
Pacemaker on a selected Ready node
       |
       +-- status collector CronJob --> PacemakerCluster CR
                                                |
                       +------------------------+-------------------+
                       |                        |                   |
                 health controller        metrics controller   notifications
                       |
              Operator conditions/events

CA files on each host --> certificate watcher --> local etcd restart
```

The status collector rotates eligible targets using the existing freshness and
fallback logic. The health controller interprets the CR and maintains
`PacemakerHealthCheckDegraded`; it does not change membership. Metrics and console
notifications share the CR informer. The certificate watcher observes host files
and operates independently of API availability.

## Data flow and identity

There are three deliberately different inputs:

| Input | Meaning | Safety boundary |
| --- | --- | --- |
| Registered control-plane Nodes | Desired members, including NotReady nodes | Validate name, UID, and canonical IP; reject incomplete or unsupported sets. |
| PacemakerCluster CR | Observed membership and health | Use a fresh, complete observation when available; otherwise discover through Ready candidates. Offline members remain configured members. |
| Runner's local PCS/CIB reads | Membership and progress at execution time | Check live configuration and durable repair attributes before mutation. |

The controller records both desired and previous identities. That lets it detect
same-name/IP UID replacement. Persistent `tnf_node_uid_<name>` CIB properties
checkpoint the identity accepted by the runner. A Job's owner reference and
environment bind it to its exact ConfigMap, while after-setup checks that same
record's completed Job and current Kubernetes UID.

A historical success cannot certify a new identity or a new generation.

## Concurrency and lifetime

| Mechanism | Protects |
| --- | --- |
| One lifecycle worker | Generation decisions and Job creation in this operator process. |
| Existing startup mutexes and framework duplicate prevention | Starting bootstrap/runtime, health, and collector controllers. |
| Persisted Jobs plus nonterminal Pod checks | Serialization across attempts, generations, and operator restart. |
| Exact ConfigMap UID and repeated live node checks | Preventing a runner from silently using replacement or superseded input. |
| Durable CIB repair generation and timestamp | Continuing a partial repair without manufacturing a new recovery claim on each retry. |

These are separate boundaries. A controller mutex alone cannot serialize a Job
that survives CEO restart. A terminal Job condition alone does not prove its Pods
have stopped. The reconciler checks both before starting another attempt.

The operator context controls informer and controller lifetime. Kubernetes Jobs
are API resources: cancelling CEO does not cancel or delete an already running
Job. The next CEO process observes it and waits. See
[controller lifecycle](controller-lifecycle.md#cancellation-and-shutdown).

PCS commands and Kubernetes updates do not form a transaction. In particular,
node addition and its subsequent UID checkpoint are separate operations. A crash
between them can require repeating reconciliation. Do not interpret the durable
checkpoints as an exactly-once guarantee.

## Upgrade policy and dependencies

Routine work waits for control-plane MCO activity: desired/current
config annotations differ, state is Working, post-config-action is Rebooting, or a drain/uncordon
request is unacknowledged. ClusterVersion progression alone is not a gate.
Actual deleted/replaced/missing membership can be repaired during that activity,
because restoring Pacemaker may be necessary for MCO to finish.

A retry can be admitted to inspect local repair progress. During progression,
the runner requires either current membership repair or a durable claim proving
that this generation already started repair before it permits routine steps.
A `repair` flag in a ConfigMap alone does not grant that exception.

Runtime replacements use main's `WaitForStableRevision` helper before stopping
or removing members, ensuring the latest manifests are delivered first.
Add-only recovery of an already-missing member skips that wait, as it does on main.

Without usable Pacemaker status, new work waits until MCD finishes. Previously
recorded repair retries retain their existing exception. Discovery itself never
treats missing status as proof of membership drift.

Dependencies include the PacemakerCluster CRD, working Kubernetes/config APIs,
current node identities, an eligible executor, the operator image, host PCS/CIB
commands, fencing credentials, and the existing etcd resource-agent contract.
Unavailable or malformed authoritative inputs stop mutations; they do not imply
that members should be deleted.

## Scope boundaries

General cleanup removes auth/after-setup Jobs for retired node identities and
orphan TNF Pods. Generation reconciliation also retires stopped legacy
update-setup Jobs and prunes recorded history. When Pacemaker status is unavailable,
recorded attempts try Ready candidates and inspect live PCS membership. Discovery
of unchanged desired state does not mutate a matching cluster. The collector
retains its independent fallback and rotation.

Health evaluation, metrics, notifications, certificate watching, and collector
rotation remain separate. For deliberately held-offline PHC tests, first wait for
generation completion with no active attempts: matching discovery does not restart
a stopped member. Restoring and validating the relaxed assertions remains work in
[OCPBUGS-129429](https://redhat.atlassian.net/browse/OCPBUGS-129429); pause support
is deferred unless cluster testing demonstrates a remaining need.
