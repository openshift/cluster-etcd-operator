# TNF lifecycle and recorded generations

The lifecycle controller starts bootstrap and runtime controllers and reconciles
post-transition Pacemaker membership through one work queue. Operator, Node, and
PacemakerCluster and execution-resource events enqueue work; a
one-minute resync covers missed events.
Node handlers do not launch reconciliation goroutines. See the [documentation
index](README.md) for architecture, controller lifetime, detailed reconciliation,
and the [comment-by-comment review](review-criteria.md).

PLM also manages health-controller startup after transition and supplies its
shared clients, informer, and lifecycle context. Health evaluation and reporting
run on that controller's own queue, independently of generation reconciliation.
See [runtime controller ownership](architecture-overview.md#independent-runtime-controllers).

## Bootstrap and setup completion

Bootstrap still requires two Ready control-plane nodes, completed etcd bootstrap,
and a stable revision. Auth, initial setup, fencing, and after-setup use the
existing job framework.

`ExternalEtcdHasCompletedTransition=True` is the durable transition boundary.
Setup's job-specific creation admission hook refuses new Jobs after that point,
even after deletion of the completed Job or image/config drift. Deletion remains
permitted and creation deferral preserves existing availability status. The
shared JobController contains no setup-name or transition-specific policy.
Resetting the transition condition and deleting the Job permits a deliberate
rerun. The setup runner retains its defensive transition check.

Readiness requirements live in creation admission callbacks: setup checks all
registered control-plane nodes, fencing checks nodes with fencing secrets, and
node-specific jobs check their current target. The framework keeps placement,
configuration invalidation, and creation admission separate.

Fencing uses the durable transition condition after bootstrap. After-setup uses
bootstrap Job completion before transition, and its node's recorded-generation
completion afterward. The setup runner retains its defensive transition check.

## Desired membership and execution targets

Desired membership is the complete registered control-plane node set, including
NotReady nodes. A snapshot records each node's name, UID, and canonical IP. Missing
identity data, an unsynced node cache, or zero/more than two nodes is an error,
not an instruction to remove members.

Execution targets prefer Ready nodes in the Kubernetes/Pacemaker intersection.
When Pacemaker status is unavailable, discovery tries Ready Kubernetes candidates
and the runner reads live PCS membership before deciding on changes. The collector
retains its independent fallback and rotation; PHC does not gate update-setup.
An offline registered member is not removed merely because it is offline.

## Generation records and jobs

The controller creates immutable `tnf-update-setup-N` ConfigMaps in
`openshift-etcd`. `config.json` contains schema version, generation number,
operator image, desired node identities, previous identities, and optional repair
and discovery flags. Discovery rechecks unchanged completed input when Pacemaker
observation is unavailable. ConfigMap metadata holds completion and executor/retry state;
immutable data never doubles as mutable execution status.

Jobs are named `tnf-update-setup-N-A`, where A is the unique execution sequence. Their owner reference
and environment bind them to the exact ConfigMap name and UID. A runner refuses
missing, recreated, unsupported, or stale records; it never selects the globally
latest ConfigMap. It rereads Kubernetes identities between mutation steps.

The lifecycle controller runs one generation attempt at a time. It waits for
running or terminating pods from earlier attempts, including legacy fixed-name
Jobs. Stopped legacy Jobs are retired before the first generation runs. A retry
rotates to the next eligible node; the attempt and generation are reconstructed
from API objects after CEO restart.

Each execution configuration has six attempts and a fifteen-minute fallback
deadline per Job. For unfinished work, changing eligible executors immediately
cancels stale execution and resets its budget. The configuration, retry boundary, and reserved Job
sequence are persisted before cancellation, so CEO restart cannot reuse an old
budget or Job name. Membership generations and durable repair checkpoints are
retained. Replacement waits for all old Pods to stop.
Exhaustion sets `TNFUpdateSetupJobDegraded`; after ten minutes a new budget can
retry the same generation. Degraded stays set until actual success.
Completed matching generations remain complete across executor changes and Job
deletion. This decision belongs to reconciliation; creation admission checks
whether an unfinished attempt may run now. Completed generations do not hide
later membership drift: after a two-minute
convergence window and a newer status observation, drift records another repair.
Unavailable observations can instead record discovery of unchanged completed
input; matching discovery completes without mutation, even if a member is offline.
The five newest records are retained. Older records and their owned Jobs are
pruned only after reconciliation has verified that all executions have stopped.

After-setup checks the newest record, its node UID, and recorded completion.
It validates the successful Job's owner/generation binding while the Job remains;
after Job cleanup it requires persisted completion bound to the record's exact UID.
An older success cannot satisfy a newer
generation or a replacement node with the same name and IP.

## Upgrade and MCO sequencing

Creation admission is evaluated for every attempt, including retries. Routine
reconciliation is deferred while a control-plane node has an unfinished MCO
config change, Working state, a Rebooting post-config-action, or an
unacknowledged drain/uncordon request. ClusterVersion Progressing alone and worker-only rollouts do not block
admission; the remaining cluster upgrade need not finish before TNF reconciles.
Deferral rereads live state rather than preserving an earlier decision.

Membership repair is the exception: removing a deleted/replaced identity or
re-adding a missing registered member may be necessary for an MCO reboot to
finish. A blanket wait for MCO or all nodes to become Ready would deadlock that
repair. Replacements that remove and add members retain main's
`WaitForStableRevision` before claiming recovery, stopping, or removing a member.
Add-only recovery of an already-missing member skips that wait, as on main.
Read-only discovery also requires no revision wait. Normal application and repair validate the configured member
set and all desired nodes online before reporting success, so missing
auth/manifests remain retryable failures. Matching read-only discovery has no
online requirement and does not start a stopped member.

A Pacemaker `tnf_reconciliation_generation` attribute records when a repair has
actually started. A retry of a recorded repair can inspect that claim during MCO activity; an old
collector observation alone does not authorize routine work during an upgrade.
Persistent `tnf_node_uid_<node-name>` CIB properties distinguish same-name/IP
replacement and checkpoint successful node addition before later fencing/resource
work. The durable `tnf_reconciliation_force_new_cluster` timestamp is reused by
retries of unfinished repairs, so completing a partial repair does not manufacture a fresh claim. These
properties do not depend on the replacement node already being online in the CIB.
Completed membership history survives executor-budget changes. Historical UID
replacement and old CIB claims cannot authorize routine work during a later
MCO rollout. New membership drift after success requires a new generation.
PCS addition and the UID checkpoint remain separate operations; this is not an
exactly-once transaction.

The runner preserves timestamp-based `force_new_cluster` claims. It avoids
rewriting an unchanged etcd IP map and only removes unstarted etcd members whose
peer URLs do not belong to the desired node set.

## Regression evidence and remaining validation

The focused tests cover Kubernetes-driven membership, persisted generations and
completion, serialized retries, reboot admission with necessary repair, and
replacement recovery after partial failure. The named cases and their business
invariants are listed in [review criteria](review-criteria.md#regression-evidence).

Before merging, run TNF recovery 1/3, 2/3, 3/3, degraded, certrotation, and upgrade
payload lanes on the exact candidate SHA. QE must verify initial transition,
MCO-induced reboot, actual replacement, and CEO restart with recorded evidence.

For held-offline PHC tests, wait for the current generation to complete and for
all attempts to stop before stopping the member. Matching read-only discovery
then leaves it stopped; an unfinished normal application or repair may still
start it. Restoring and validating the relaxed Origin assertions is tracked in
[OCPBUGS-129429](https://redhat.atlassian.net/browse/OCPBUGS-129429). Pause support
is deferred unless cluster validation demonstrates a remaining need. This patch
does not change those assertions. Health remains a separate controller managed
by PLM; metrics, notifications, certificate watching, and status collection keep
their own responsibilities.
