# Lifecycle part 3: review criteria and evidence

This audit evaluates the final recorded-generation implementation against the
comments collected on October 22, 2026. "Implemented" below means the code and
named regression evidence support the behavior. Cluster acceptance remains open
until payload/QE results exist for the candidate SHA. Earlier part-2 CI results
are not validation of this patch.

## Collected comment disposition

| Source | Criterion | Assessment |
| --- | --- | --- |
| [CEO #1668: queue](https://github.com/openshift/cluster-etcd-operator/pull/1668#discussion_r3737830083) and [review context](https://github.com/openshift/cluster-etcd-operator/pull/1668#discussion_r3737673394) | Remove custom event-handler goroutines; reconcile Node/Pacemaker mismatch in sync. | Implemented. Factory informer events enqueue one lifecycle worker. Relevant execution/config watches and the one-minute resync drive reevaluation. |
| [CEO #1668: API membership](https://github.com/openshift/cluster-etcd-operator/pull/1668#discussion_r4195845152) and [reboot concern](https://github.com/openshift/cluster-etcd-operator/pull/1668#discussion_r4193996661) | Separate desired members from Ready executors; do not force-remove a registered rebooting node merely because it is offline. | Implemented and unit covered. Kubernetes name/UID/IP defines desired membership, while readiness selects execution targets. Real MCO/replacement evidence remains required. |
| [CEO #1675: generation and idempotency](https://github.com/openshift/cluster-etcd-operator/pull/1675#discussion_r3864383115), later [marked fixed](https://github.com/openshift/cluster-etcd-operator/pull/1675#discussion_r3905312133) | Put generation in update-setup Job names; preserve no-op handling for unchanged certificate input. | Generation binding implemented and unit covered. Existing certificate-watcher hash/baseline behavior is preserved unchanged; certificate rotation still needs candidate-payload validation. |
| [CEO #1729](https://github.com/openshift/cluster-etcd-operator/pull/1729#issuecomment-6341729899) / [OCPBUGS-132800](https://redhat.atlassian.net/browse/OCPBUGS-132800) | Never trigger initial setup after transition, including scheduling drift or deleted completed Jobs; avoid the associated Available blip. | Implemented through setup-specific creation admission and unit covered. Completed Job deletion remains permitted; missing-Job, image/spec, and executor-driven replacements are refused after transition. Deferred creation preserves availability. Cluster/QE acceptance remains open. |
| [CEO #1729](https://github.com/openshift/cluster-etcd-operator/pull/1729#issuecomment-6341729899) / [OCPBUGS-132819](https://redhat.atlassian.net/browse/OCPBUGS-132819) | Defer routine update-setup during upgrade/MCO, reevaluate afterward, allow re-addition needed for MCO completion. | Implemented using control-plane Node MCO activity, with an explicit recovery exception and unit evidence. Global ClusterVersion progression and worker-only rollouts do not block admission; only retries of a recorded repair may inspect durable local repair state; routine retries are deferred. Live upgrade/QE acceptance and any TNF monitor exception remain open. |
| [CEO #1729: QE request](https://github.com/openshift/cluster-etcd-operator/pull/1729#issuecomment-6341729899) | Obtain QE verification because of patch size and preserve stable recovery/certrotation behavior. | Outstanding. Local tests/builds cannot substitute for QE and candidate-payload results. |
| [OCPBUGS-129429](https://redhat.atlassian.net/browse/OCPBUGS-129429) | Future mechanism to pause recovery while PHC tests deliberately hold a node offline. | Explicitly deferred to that follow-up. No pause or restoration of relaxed Origin assertions is included. API membership alone does not fulfill this contract. |
| [OCPBUGS-90558](https://redhat.atlassian.net/browse/OCPBUGS-90558) | Avoid mistaking an MCO-triggered reboot for permanent node replacement and causing split-brain. | Root-cause policy addressed by registered identity membership and upgrade gating. Actual performance-profile/MCO reboot validation remains required. |

## Regression evidence

Unit tests focus on the lifecycle's critical business transitions. MCO admission
is exercised through controller and runner behavior, without an annotation-state
matrix or separate helper-policy tests.

- `TestGenerationPersistsAcrossRestartAndReplacement`: execution uses persisted
  input after restart; readiness changes and Job cleanup do not rerun completed
  membership work; a Kubernetes UID replacement records new desired input.
- `TestGenerationExecutorDriftCancelsPendingAndResetsFailedBudget`: cancel a
  pending attempt that lost its executor, wait for its Pod to stop, then resume
  on the survivor with a fresh budget and the same membership record.
- `TestGenerationDefersRebootButPermitsMissingMember`: defer routine work during
  a control-plane rollout, while allowing membership repair needed for recovery.
- `TestGenerationAdmissionDefersEveryRoutineRetry`: an ordinary retry also waits
  for MCD activity to finish and resumes without consuming budget while deferred.
- `TestRegisteredOfflineNodeIsNotRemoved`: a registered member's temporary
  offline state cannot authorize removal, addition, or force-new-cluster work.
- `TestPartialReplacementResumesWithoutRemovingNodeAgain`: one injected IP-map
  failure after replacement verifies durable recovery claims, claim-before-resource
  ordering, and restart without repeating membership changes during MCD activity.
- `TestLiveStateChangesStopFurtherMutation`: a Kubernetes UID change during
  reconciliation stops subsequent membership mutations.
- `TestGenerationNeverFallsBackToLatest`: a runner needs explicit recorded input
  and the exact ConfigMap UID.
- `TestAfterSetupRequiresItsCompletedGeneration`: after-setup requires current
  generation/node identity and valid completion proof, including after Job cleanup.
- `TestSuccessfulGenerationDoesNotHideNewDrift`: confirmed Pacemaker membership
  mismatch after success records new repair work against the Kubernetes desired set.
- `TestFailedGenerationRemainsDegradedUntilSuccess`: exhausted recovery stays
  degraded through renewed attempts until the Job succeeds.
- `TestGenerationRetiresLegacyJobBeforeRecording`: old fixed-name execution must
  stop before migration; its success cannot certify a recorded generation.
- `TestSetupCreationAdmissionUsesTransitionAndReadiness`: completed transition
  blocks setup creation; manual reset and readiness determine bootstrap admission.
- `TestDeferredCreationPreservesCompletedStatus`: deletion followed by denied
  replacement preserves completed availability and progression status.
- `TestSyncMultiNodeJobState_DriftDetection`: three cases distinguish placement
  changes, explicit configuration changes, and retrying on current executors.

Factory queue/informer wiring, validation helpers, and history pruning are
checked by source inspection. Existing health, metrics, certificate-watcher,
collector, workload-template, bootstrap-member, and framework retry tests remain.
Unit checks do not substitute for candidate-payload/QE evidence.

Kubernetes/PCS operations are not transactional. A crash between member addition
and its UID checkpoint can still require repeated reconciliation; the retained
partial-progress test exercises failure after that checkpoint.

## Documentation preservation

The original full-refactor branch's documentation topics are restored at the
same paths, rewritten against the final implementation:

| Original coverage | Final location / correction |
| --- | --- |
| README, quick reference, component/job roles, common workflows | [README](README.md). |
| Dual-mode architecture, diagrams, data flow, dependencies, concurrency | [Architecture](architecture-overview.md). Health remains independent; there is no global in-memory generation counter. |
| Startup, informer events/resync, framework, context/shutdown | [Controller lifecycle](controller-lifecycle.md). Queue-driven events replace custom side-effect handlers. Submitted Jobs survive CEO cancellation. |
| Health states, unknown grace, events, metrics, troubleshooting | [Controller lifecycle](controller-lifecycle.md#health-monitoring). Current independent health/disruption implementation is authoritative. |
| Drift examples, ConfigMap format, generation algorithm, job walkthrough | [Reconciliation](reconciliation.md). Explicit immutable JSON name/UID binding replaces latest-record selection. |
| Reconciliation concurrency, errors, discovery, replacement, health integration | [Reconciliation](reconciliation.md). Persisted serialization replaces two-phase local locking; fresh observation replaces direct discovery mutation. |
| Job framework state machines, retries, job configuration/drift | [Job controllers](job-controllers.md), preserving current framework behavior and documenting setup's durable completion exception. |

General orphan-node-job and Pod cleanup is retained in a separate change, using
live identity/ownership checks. `TestCleanupRetiresNodeJobsAndOrphanPods` covers
retired UIDs, Pod ownership, collector preservation, and failed Node reads.
In-controller health relocation, old generation
locks, latest-ConfigMap discovery, and a post-transition stable-revision wait
were not carried forward. These omissions are explicit, rather than
undocumented claims that every historical branch behavior remains present.

## Local validation

The candidate source tree passed these checks on October 22, 2026, before the
final commit was amended:

- `go test ./pkg/tnf/... ./pkg/operator/ceohelpers`
- Race detector for `pkg/tnf/operator`, `pkg/tnf/pkg/generation`,
  `pkg/tnf/update-setup`, and `pkg/tnf/pkg/jobs`, using the local validation
  container's C toolchain.
- `make verify` and `make build` (all four binaries).
- Relative documentation file links, named regression references, and
  `git diff --check`.

These are local source-tree results, not candidate-payload or QE results.

## Remaining acceptance work

No cluster payload/QE evidence was produced by this local implementation audit.
Before merge, record the exact candidate SHA, payload, job URL, result, and logs
for each of the following:

1. TNF recovery 1/3, 2/3, 3/3, degraded, certrotation, and upgrade payload lanes.
2. Initial transition followed by execution-node eligibility changes: no
   scheduling-triggered setup recreation or associated Available blip. Verify
   manual reset/deletion and distinguish image/config-driven executions from
   executor-driven changes. Post-transition creation must be denied in every case.
3. Ordinary MCO/performance-profile reboot: registered identity retained, routine
   update-setup deferred, eventual recovery, and no forced removal/split-brain.
4. A genuinely missing member during MCO: necessary re-addition progresses rather
   than waiting on readiness/revision state that depends on the repair.
5. Replacement, including reused name/IP with a new UID, plus CEO restart during
   active and partially completed reconciliation.
6. QE sign-off on OCPBUGS-132800 / OCPBUGS-132819 and the final part-3 PR. Resolve
   any TNF-specific monitor exception from actual evidence.

PHC pause ownership remains with the OCPBUGS-129429 follow-up; this patch neither
adds that mechanism nor claims its previously relaxed assertions are restored.
