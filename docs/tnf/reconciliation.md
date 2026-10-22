# Recorded-generation reconciliation

## Triggers and decision flow

Node, Operator, PacemakerCluster, and execution-resource events
enter the lifecycle work queue. A one-minute resync provides baseline detection.
The sync loop is the sole entry point for membership decisions.

```text
Managed + external etcd transition complete?
       | no -> no runtime membership work
       v
valid registered control-plane identities?
       | no -> fail safely, do not infer removals
       v
observe existing Jobs/Pods; retire stopped legacy fixed-name Jobs
       v
load persisted records + fresh complete Pacemaker observation
       | missing/stale/invalid -> wait/error; no discovery mutation
       v
compare desired identity/image, executor configuration, completion and drift
       | stale execution -> persist fresh retry boundary, request deletion
       | old Job/Pod still executing -> wait; no replacement or pruning
       |
       +-- new desired state or repair generation needed
       |     +-- reevaluate control-plane MCO policy
       |     +-- persist immutable ConfigMap; return before creating Job
       |
       +-- completed matching generation
       |     +-- record completion metadata if necessary
       |     +-- converged? retain five newest records, clear retry status
       |     +-- recurring drift? wait for convergence window + fresh CR,
       |                         then record a new generation
       |
       +-- pending or failed generation
             +-- exhausted? report Degraded; allow fresh retry budget after 10m
             +-- validate policy and live identities
             +-- choose eligible target; create next generation-bound attempt
```

## Desired membership versus executor selection

Desired state contains every registered control-plane node, including NotReady
nodes. Each identity has a DNS-safe name, Kubernetes UID, and canonical IP. The
schema accepts one or two nodes and rejects zero, more than two, missing data,
duplicate names/UIDs/IPs, and invalid IPs. IPv6 spellings are normalized before
comparison. The writer and readers use the same contract.

Observed membership includes configured offline Pacemaker nodes. A temporary
Ready or online-state change cannot itself instruct the runner to remove a
registered identity.

Execution targets use the existing Ready-node/Pacemaker intersection and fallback
logic. They answer where a Job can run, rather than which nodes should belong.
A recovery attempt must run on a configured survivor: the runner refuses to
remove its own execution node. A failed attempt rotates among currently eligible
targets.

| Change | Desired-state consequence |
| --- | --- |
| Registered member becomes NotReady/offline | Retain its name/UID/IP; do not remove it for that reason. |
| Node is deleted from the API | Record the remaining valid registered set and reconcile removed membership. |
| New node name replaces an old node | Record both the desired identities and previous generation identities. |
| Same name/IP returns with a new UID | Record replacement; compare persistent CIB UID and previous identity. |
| Node IP changes | Record the new canonical address; reconcile old configured address. |
| Operator image changes | Record a new application generation, subject to routine upgrade deferral. |
| Invalid/empty input or stale observation | Stop; do not translate uncertainty into an empty desired set. |

## Immutable ConfigMap format

The record name is `tnf-update-setup-N` in `openshift-etcd`. For example:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: tnf-update-setup-7
  namespace: openshift-etcd
  labels:
    app.kubernetes.io/component: tnf-update-setup
    tnf.etcd.openshift.io/generation: "7"
immutable: true
data:
  config.json: |
    {
      "version": 1,
      "generation": 7,
      "image": "registry.example/operator@sha256:example",
      "repair": true,
      "nodes": [
        {"name": "master-0", "uid": "uid-a", "ip": "10.0.0.1"},
        {"name": "master-1", "uid": "uid-new", "ip": "10.0.0.2"}
      ],
      "previous": [
        {"name": "master-0", "uid": "uid-a", "ip": "10.0.0.1"},
        {"name": "master-1", "uid": "uid-old", "ip": "10.0.0.2"}
      ]
    }
```

The schema version, generation label/name, immutability, and identities are
validated when decoding. The API assigns the ConfigMap UID; the controller
rereads the persisted object before creating an execution.

Completion is mutable metadata, not a modification to immutable input.
`completed-at` and `completed-job` record successful membership reconciliation.
`completed-record-uid` binds that proof to the ConfigMap incarnation whose Job
ownership was validated by the controller.
When the desired input and configured membership still match, completion survives
executor changes and deletion of the successful Job. Placement changes cannot
invalidate that success or create another attempt:

```yaml
metadata:
  annotations:
    tnf.etcd.openshift.io/completed-job: tnf-update-setup-7-2
    tnf.etcd.openshift.io/completed-at: "2026-10-22T15:00:00Z"
    tnf.etcd.openshift.io/completed-record-uid: "record-uid-7"
```

The next generation is derived from recorded API state. Repeated sync with the
same pending input does not continually increment a local counter. A completed
generation can be superseded by changed identities/image or recurring observed
membership drift. Executor changes and renewed retry budgets for unfinished work
retain the same membership generation and its durable repair checkpoint.

## Binding an attempt to its input

Attempt A of generation N is named `tnf-update-setup-N-A`. It carries:

- The generation label and framework attempt label.
- A ConfigMap owner reference containing the exact record name and UID.
- `TNF_UPDATE_SETUP_CONFIGMAP` and `TNF_UPDATE_SETUP_CONFIGMAP_UID` environment
  values naming that same input.
- The recorded operator image and a selected node assignment.
- `backoffLimit: 0` and a fifteen-minute active deadline.

The runner does not search for the latest ConfigMap. Missing environment,
recreated ConfigMap UID, unsupported schema, or a snapshot that no longer matches
live Kubernetes identities stops it. A later generation appearing cannot silently
change the input to an already running Job.

## Runner flow and recovery checkpoints

[recorded.go](../../pkg/tnf/update-setup/recorded.go) performs these steps:

1. Load the explicit ConfigMap and validate its UID/schema.
2. Read the CIB's TNF properties. A CIB error or duplicate TNF property is an
   error, rather than evidence that an identity attribute is absent.
3. Check live Kubernetes identity, control-plane MCO policy, configured membership, and a
   running local Pacemaker cluster. Refuse an unconfigured executor.
4. Compare configured names/IPs and persistent UID properties. For actual
   membership work, persist a recovery timestamp followed by its generation
   marker before removing or adding members.
5. Remove obsolete/replaced members and add desired members. Checkpoint each
   successfully added UID before unrelated fencing/resource work can fail.
6. Configure fencing with the existing idempotent helper.
7. For a started repair, remove only unstarted etcd members whose peer addresses
   do not belong to desired nodes, and apply the persisted timestamp-based
   `force_new_cluster` claim before any etcd IP-map update.
8. Update the etcd resource's `node_ip_map` only when it differs. Record desired
   UID properties, enable/sync/start the cluster, and ensure alert agents.
9. Validate the exact configured member set, every desired node online, and
   unchanged live Kubernetes identities before reporting success.

Every direct mutating command passes the live identity/upgrade guard again.
Fencing and alerts use their existing multi-command helpers after an entry guard;
those helpers are not an atomic transaction with Node updates. Context
cancellation and subsequent checks bound execution, but cannot roll back a PCS
command already accepted by the host.

Durable CIB properties have different meanings:

| Property | Meaning |
| --- | --- |
| `tnf_node_uid_<node>` | Accepted identity for a configured node, checkpointed after addition. |
| `tnf_reconciliation_generation` | This generation started actual membership repair. |
| `tnf_reconciliation_force_new_cluster` | The timestamp claim chosen for that repair and reused by retries. |

A restarted runner can finish fencing, resource updates, or alerts without
removing a successfully checkpointed replacement again. It reuses the claim
rather than generating a new destructive recovery timestamp on every retry.
A repair marker without a valid timestamp fails safely.

There is still no transaction spanning `pcs cluster node add` and the UID write.
A crash inside that narrow boundary can repeat membership reconciliation. The
checkpoints prevent later unrelated failures from undoing acknowledged progress;
they do not provide exactly-once external side effects.

## Auth and after-setup sequencing

Per-node auth is managed through the existing framework and refreshes its node
UID from the informer. A replacement triggers node-job drift rather than relying
on a stale Node captured during controller creation.

Before transition, fencing and after-setup use completed initial setup. After
transition, fencing uses the durable condition. After-setup additionally checks:

1. Its scheduled node UID is still the live Kubernetes UID.
2. The newest record matches the current registered identities.
3. That record includes the node and records a completed Job.
4. If the completed Job remains, it has the matching generation label and
   ConfigMap name/UID owner reference, and actually reports Complete. If it was
   deleted, persisted completion must be bound to the current ConfigMap UID;
   copied completion from an older record incarnation is refused.

An old completed Job cannot satisfy a new generation or a replacement with a
reused name/IP. Initial setup Job deletion does not force bootstrap to run again.

## Upgrade/MCO policy

The controller reads live control-plane Node MCO annotations. Routine work
waits while desired/current configuration differs, state is Working,
post-config-action is Rebooting, or `desiredDrain` differs from `lastAppliedDrain` for a nonempty request. The
latter covers an unacknowledged drain or uncordon even after configuration
converges. These annotation names/states follow the
[MCO 4.22 definitions](https://github.com/openshift/machine-config-operator/blob/release-4.22/pkg/daemon/constants/constants.go).

ClusterVersion Progressing alone does not block admission. A worker-only MCO
rollout also does not hold control-plane reconciliation. After deferral, the
controller rebuilds the decision from current state. The runner applies the
same node-level gate before mutation.

Actual missing/deleted/replaced membership is an exception: a Pacemaker re-add
may be necessary before MCO can finish. A global wait for all Nodes Ready or a
stable revision on this path could deadlock recovery.

Every creation, including retries, passes a job-specific admission hook. Only
retries of a recorded repair may inspect a local durable repair claim when the
CR no longer shows the mismatch; an ordinary retry is deferred. The runner refuses routine mutations during
progression unless current configuration still needs repair or the CIB proves
this generation already began repair. A ConfigMap's `repair: true` alone cannot
authorize work from an obsolete collector observation. Previous UID differences
and CIB repair claims from a generation that already succeeded cannot bypass a
later upgrade/MCO gate. Its runner also refuses new membership repair under that
completed generation; fresh observed membership drift records a new generation.
The convergence window can defer that new record, but cannot delay cancellation
of an execution whose eligible executor inputs already changed.

Consequently this policy is not a blanket prohibition on Job creation while
MCO progresses. It implements the recovery caveat in the collected PR comment.
Its cluster-level behavior and any required TNF-specific monitor exception still
need upgrade-lane and QE evidence; no general monitor suppression is introduced.

## Attempts, completion, and history

Each execution configuration has six attempts. Mutable ConfigMap metadata records
its canonical executor identities, first sequence in the current retry budget,
and last reserved sequence. Job labels distinguish the unique execution sequence
from its position in that retry budget. Both are reconstructed after restart.

For unfinished work, changed executors persist a fresh boundary before cancelling
stale execution.
Pending work does not wait for the fifteen-minute fallback deadline. Failed work
from earlier inputs does not consume the new budget. An API create error may
leave a gap in sequence numbers, but does not consume an execution retry.

A failed or deleted Job can retain running/terminating Pods. The controller waits
for owned Pods and surviving labelled update-setup Pods before replacement. This
also applies to old generations and legacy fixed-name Jobs. Denied creation does
not reserve a Job sequence or consume a retry.

After success, completion metadata is recorded. A matching configured set clears
retry status without restarting work when executors change or the Job is deleted.
Generation selection and completion decide whether work is needed; creation
admission checks whether a proposed attempt may run now. Failed or cancelled
attempts can retry the same unfinished generation. If membership drift persists
or recurs after success, the controller requires a two-minute
convergence window and an observation at least as recent as completion before
recording another repair. Old success therefore cannot indefinitely hide drift.

Exhaustion sets `TNFUpdateSetupJobDegraded` with `MaxRetriesExceeded`. After ten
minutes a fresh budget can retry the same generation; the degradation remains visible through
retries and deferrals until success.

The five newest records are retained. Pruning happens after the global execution
check. Deleting an old ConfigMap permits Kubernetes owner-based garbage
collection of its Jobs. This is bounded generation history, not general cleanup
of every old node-specific workload.

## Discovery, errors, and restart

Missing/stale Pacemaker status stops generation decisions. The old full-refactor
branch's direct discovery-mode mutation was not carried over. Existing collector
fallback/rotation remains the means to recover the observation path; a prolonged
collector failure is visible through the independent health controller.

API, CIB, fencing, or command failures remain retryable execution failures.
Invalid identity and input-binding failures stop the runner before destructive
work. Do not treat a failed CIB query as an absent UID or an empty member list.
See [troubleshooting](controller-lifecycle.md#troubleshooting-walkthrough).

After CEO restart, API records supply N and A, running Jobs remain visible, and
CIB checkpoints supply local repair progress. No context value or local counter
is needed to recover the generation.

## Worked scenarios

### Reboot without replacement

Both Nodes remain registered with the same UIDs/IPs. `master-1` becomes NotReady
and offline during MCO. The desired set remains both nodes, and configured
membership still includes both. Routine work is deferred and no removal is
inferred. Once MCO resolves, a completed unchanged generation remains complete.
If an initial/image generation still needs application, it can run without
removing the registered member.

### Same-name and same-IP replacement

Generation 7 records `master-1` UID `old`; the API now has UID `new`. Generation 8
records the new UID and previous identity even though the IP map is unchanged.
A survivor removes/adds `master-1`, checkpoints UID `new`, then completes the
remaining work. If fencing fails after addition, a retry reads that checkpoint,
keeps the replacement, and completes the same repair with the same timestamp
claim. After-setup accepts only generation 8 completion for UID `new`.

### CEO restarts during a failed attempt

Generation 8 attempt 1 reports FailureTarget, but its Pod is still terminating.
The new CEO process sees both objects and creates no overlapping attempt. Once
execution stops, attempt 2 runs on the next eligible target using generation 8's
exact ConfigMap UID. A newer identity invalidates its snapshot before further
mutations; the controller records a newer generation after old execution stops.

## Relationship to health tests

Reconciliation changes configuration; health monitoring reports observed health.
Keeping registered membership avoids reboot-driven force removal, but
update-setup can still start a deliberately stopped cluster. That does not
establish a contract for holding a PHC test node offline. Pause support and
restoring the temporarily relaxed Origin assertions remain separately tracked in
[OCPBUGS-129429](https://redhat.atlassian.net/browse/OCPBUGS-129429).
