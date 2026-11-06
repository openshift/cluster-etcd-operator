# TNF lifecycle documentation

Two Node with Fencing (TNF) uses Pacemaker, Corosync, and STONITH to manage
external etcd and kubelet. These guides describe bootstrap, runtime recovery,
and the recorded generations used by lifecycle part 3.

## Reading guide

| Guide | Purpose |
| --- | --- |
| [Lifecycle summary](lifecycle-manager.md) | Start here for the final generation contract and policy. |
| [Architecture](architecture-overview.md) | Components, data flow, dependencies, and concurrency boundaries. |
| [Controller lifecycle](controller-lifecycle.md) | Startup, events, cancellation, health monitoring, and troubleshooting. |
| [Reconciliation](reconciliation.md) | Drift decisions, immutable records, runner steps, retries, and worked examples. |
| [Job controllers](job-controllers.md) | Existing node/cluster framework, conditions, and retry state machines. |
| [Review criteria](review-criteria.md) | Collected comments, implementation evidence, and outstanding cluster validation. |

## System overview

```text
Kubernetes Nodes                 PacemakerCluster status
       |                                  |
       +----------> lifecycle queue <-----+
                          |
             +------------+-------------+
             |                          |
        before transition          after transition
             |                          |
   auth -> setup -> after-setup     recorded ConfigMap N
             |                          |
      durable transition           Job N-attempt
             |                          |
             +--------------------- Pacemaker / etcd
```

The Kubernetes API defines desired membership. Pacemaker status describes
observed configuration and health. Readiness selects where a Job can execute;
it does not decide which registered node deserves membership.

## Components and job types

- The lifecycle controller starts the existing job controllers and serializes
  recorded-generation decisions through its work queue.
- Auth configures the node's hacluster credentials. Initial setup creates the
  Pacemaker cluster once. Fencing configures STONITH from fencing Secrets.
- Update-setup reconciles an explicitly recorded set of node names, UIDs, and IPs.
- After-setup disables the kubelet systemd service once its setup dependency is
  satisfied; it does not start Pacemaker.
- PLM starts the health controller after transition and supplies its shared
  clients, PacemakerCluster informer, and lifecycle context. Health keeps its own
  work queue and health-reporting logic.
- Status collection, metrics, console notifications, and certificate watching
  keep their separate responsibilities.

## Common workflows

**Initial installation:** wait for two Ready control-plane nodes, etcd bootstrap,
and stable static-pod revision; run the bootstrap jobs and complete the external
etcd transition. Setup creation admission then refuses replacements, including
after its historical Job is deleted. The runner retains its transition check; an
explicit condition reset and Job deletion permits a deliberate bootstrap rerun.

**Operator restart:** reconstruct generations and attempts from ConfigMaps and
Jobs. Start runtime controllers, respect any running or terminating execution,
and resume the recorded work. An in-memory counter is not the source of truth.

**Planned reboot:** retain a registered NotReady node in desired membership.
Defer routine work during control-plane MCO activity. If membership is actually missing,
permit the recovery required to let the reboot finish.

**Replacement:** record the new Kubernetes identity, wait for previous execution
to stop, then reconcile from an eligible surviving member. Same name and IP with
a different UID still represents replacement.

## Design principles

1. Persist the input before starting the work, and bind the Job to that exact
   ConfigMap name and UID.
2. Revalidate live node identity and MCO admission before mutation.
3. Keep only one update-setup execution active, including during migration from
   legacy fixed-name Jobs.
4. Never infer desired removals from incomplete observations; use generation-bound
   discovery and live PCS validation when Pacemaker status is unavailable.
5. Preserve current upstream health, certificate, template, retry, and status
   collection behavior while changing membership reconciliation.
6. Distinguish unit-test evidence from cluster acceptance. The remaining QE and
   payload requirements are recorded in [review criteria](review-criteria.md).
