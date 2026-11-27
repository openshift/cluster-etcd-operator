# Membership reconciliation

## Desired state and records

Every registered control-plane Node contributes its name, Kubernetes UID, and
canonical IP, including NotReady nodes. An offline Pacemaker member still belongs
when its Kubernetes identity is unchanged. Empty, incomplete, duplicate, or
more-than-two-node input stops reconciliation.

After external-etcd transition, changed identities or operator image create a new
record. Membership drift after success also creates a repair record, after a
two-minute convergence window and a status observation at least as recent as
completion. Missing, stale, or incomplete Pacemaker status uses Ready Kubernetes
candidates; the runner must inspect live PCS membership before changing it.

Each immutable `openshift-etcd/tnf-update-setup-N` ConfigMap holds `config.json`:
version, generation, image, desired identities, previous identities, and optional
repair/discovery flags. Its annotations hold executor/retry state and completion.
Jobs `tnf-update-setup-N-A` bind to that exact record name and UID through owner
references and `TNF_UPDATE_SETUP_CONFIGMAP` / `TNF_UPDATE_SETUP_CONFIGMAP_UID`.
The runner rejects missing, recreated, unsupported, or stale input.

Source: `pkg/tnf/pkg/generation/generation.go` and
`pkg/tnf/operator/lifecycle_reconciliation.go`.

## Attempts and completion

```text
persist record -> wait for earlier Jobs AND Pods to stop -> admit attempt
                                                            |
                          next eligible executor <- failure +-> success
                                  |                              |
                                  +-- backoff, retry same record  +-- record completion
```

One lifecycle worker handles informer events and a one-minute resync. Each attempt
uses the recorded image, `backoffLimit: 0`, and a fifteen-minute deadline. Failed
attempts set `TNFUpdateSetupJobDegraded`; retries start after fifteen seconds,
grow by 1.5× to a ten-minute cap, and preserve Degraded until success. Generation,
attempt numbers, and failed Job timestamps preserve progress and backoff across
CEO restart.

Changed executors cancel unfinished work after persisting its retry boundary and
next sequence. Replacement waits for all previous Pods, including terminating and
legacy fixed-name execution. Denied admission consumes no sequence. Completion
is bound to the record UID and survives executor changes and successful-Job
cleanup. New membership drift requires a new record.

When observation disappears after unchanged completed work, a discovery record
checks local membership. Matching membership with no unfinished repair completes
without mutation or requiring every member online. Repeated discovery waits two
minutes; it does not start a Job on every sync.

## Runner and checkpoints

```text
load exact record -> check live Kubernetes identity + local PCS/CIB
  -> compare configured names/IPs and saved Kubernetes UIDs
  -> matching discovery with no unfinished repair? finish without mutation
  -> replacement? wait for stable etcd revision
  -> remove obsolete members -> save replacement UID -> add missing members
  -> configure fencing -> membership changed here? clean etcd members + claim FNC
  -> update changed IP map -> save UIDs -> enable/sync/start -> alerts -> validate
```

The executor must be a configured member and cannot remove itself. Before each
mutating command, the runner rechecks Kubernetes identities. A completed record
cannot authorize new membership repair. Normal application and repair require
exact configured membership and every desired node online before success.

CIB properties checkpoint progress: `tnf_reconciliation_generation` identifies
unfinished repair; `tnf_node_uid_<name>` stores the accepted Kubernetes UID. UID
comparison falls back to previous recorded identities when no property exists.
Saving the new UID after removal and before addition means an interrupted
successful add is retained on retry. Add-only recovery skips the revision wait.

`force_new_cluster` requires successful add/remove commands in this invocation.
A saved repair marker cannot authorize it. Before claiming FNC, etcd cleanup only
removes unstarted members whose peer addresses are outside the desired set.

Source: `pkg/tnf/update-setup/recorded.go`.

## MCO removal guards

Admission defers a Job whose observed membership and recorded identities imply
removal, including name/IP/UID replacement, while control-plane MCO activity is
in progress. If membership observation is unavailable, the runner decides from
live PCS/CIB state. Admission may defer a recorded replacement retry even if an
earlier attempt already repaired it; local UID checkpoints prevent repeat removal.

The runner checks again before stopping or removing a member, including after
revision waiting and the best-effort stop. A saved repair marker cannot bypass
this check. Other work needs no MCO exception, and recording decisions continues.

MCO activity means desired/current config differs, state is `Working`,
post-config-action is `Rebooting`, or a nonempty `desiredDrain` differs from
`lastAppliedDrain`. These control-plane Node annotations cover upgrades and
MachineConfig rollouts; ClusterVersion progression and worker-only rollouts do
not gate removal. Source: `pkg/tnf/pkg/generation/policy.go`.

## Cleanup

The latest attempt retains status and executor rotation; older stopped attempts
are deleted with their Pods. Once all execution stops, the five newest records
are retained and older records and owned Jobs are pruned. Stopped legacy
update-setup Jobs are retired before recorded work begins.

While Managed with a nonempty live node set, cleanup also deletes auth/after-setup
Jobs for retired UIDs and orphan TNF Pods, checking owner UIDs and using deletion
preconditions. It preserves running unowned Pods and limits orphan deletions to
50 per sync. Cleanup errors are logged without blocking recovery. Removing API
workloads permits container garbage collection; this does not prune images.
