# AI agent instructions for the cluster-etcd-operator codebase.

## What This Repo Is

The cluster-etcd-operator (CEO) manages the etcd cluster in OpenShift, handling:
- etcd cluster scaling during bootstrap and steady state operation
- TLS certificate provisioning and rotation
- Backup and disaster recovery
- Static pod lifecycle management via library-go framework
- etcd health monitoring and defragmentation
- Bootstrap teardown and scaling strategies (HA, Delayed-HA, Two-Node, etc.)

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed architecture documentation.

## Critical Rules

1. **Never edit `vendor/` files** - managed by `go mod vendor`
2. **Never edit generated files** - anything in `vendor/` or matching `zz_generated.*` patterns
3. **Always run `make verify` before committing** - includes linting, generated file checks, and vendor verification
4. **Always run `go mod tidy && go mod vendor`** after dependency changes
5. **Never modify manifests without regenerating** - alerts/dashboards are generated from jsonnet (see [Alerts and Dashboards](#alerts-and-dashboards))
6. **Always use `make build`** not `go build` - for proper version injection

## Repository Structure

```
cluster-etcd-operator/
├── bindata/             # Embedded YAML manifests and scripts
│   ├── bootkube/        # Bootstrap manifests
│   ├── etcd/            # etcd manifests and scripts (cluster backup, cluster restore, quorum restore)
│   └── tnfdeployment/   # Two Node Fencing (TNF) manifests
├── cmd/
│   ├── cluster-etcd-operator/            # Main operator binary
│   ├── cluster-etcd-operator-tests-ext/  # OTE test binary
│   ├── tnf-monitor/                      # Two Node Fencing monitor
│   └── tnf-setup-runner/                 # Two Node Fencing setup
├── docs/                # Documentation (HACKING, FAQ, overview)
├── hack/                # Build and generation scripts
├── jsonnet/             # Alert and dashboard definitions
├── manifests/           # CVO-managed YAML (deployment, RBAC, monitoring)
├── pkg/
│   ├── operator/        # Core operator logic (20+ controllers)
│   ├── etcdcli/         # etcd client wrapper
│   ├── tlshelpers/      # TLS certificate utilities
│   └── dnshelpers/      # DNS resolution helpers
└── test/e2e/            # End-to-end tests
```

## Key Patterns to Follow

### Controller Pattern

Controllers follow the library-go factory pattern:

```go
func NewMyController(
    livenessChecker *health.MultiAlivenessChecker,
    operatorClient v1helpers.StaticPodOperatorClient,
    eventRecorder events.Recorder,
) factory.Controller {
    c := &MyController{
        operatorClient: operatorClient,
    }

    syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
    livenessChecker.Add("MyController", syncer)

    return factory.New().
        ResyncEvery(time.Minute).
        WithInformers(
            operatorClient.Informer(),
        ).
        WithSync(syncer.Sync).
        ToController("MyController", eventRecorder.WithComponentSuffix("my-controller"))
}
```

Key elements:
- Constructor function named `NewXxxController`
- Wraps sync function with health checker
- Uses library-go factory builder
- Returns `factory.Controller` interface
- **All resources the controller reads must have a corresponding informer registered via `.WithInformers()`** — missing informers are one of the most common bugs in this codebase. When adding or modifying a controller, verify that every lister/getter used in the sync function has its informer wired up in the factory builder, otherwise the controller will operate on stale data

### Status Conditions

Use `v1helpers.UpdateStatus` to set conditions:

```go
_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
    v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
        Type:    "MyControllerDegraded",
        Status:  operatorv1.ConditionTrue,
        Reason:  "SyncError",
        Message: err.Error(),
    }),
)
```

Guidelines for status conditions:
- **Use `Degraded` very sparingly** — only set it after a meaningful amount of time has passed (etcd moves slowly due to static pod machinery) and only when it requires **actionable** human intervention
- **Avoid conditions that flap** — always add an inertia component (e.g., require the error state to persist for several sync cycles before reporting Degraded). The operator has historically bumped inertia from 5 to 10 minutes to reduce flapping during rollouts.

### Bootstrap Scaling Strategies

Openshift supports anywhere from 1 to 5 control-plane nodes depending on the level of high availability needed. The bootstrap scaling strategy describes the invariants which will be enforced when scaling the etcd cluster.

The operator enforces different quorum requirements during bootstrap:
- `HAScalingStrategy` - requires at least 3 nodes to scale up (default)
- `DelayedHAScalingStrategy` - allows 2 nodes during bootstrap, then requires 3
- `TwoNodeScalingStrategy` - requires 2 nodes (for Two Node OpenShift with Fencing deployments)
- `DelayedTwoNodeScalingStrategy` - allows 1 node during bootstrap, then requires 2 (for Two Node OpenShift with Fencing deployments)
- `BootstrapInPlaceStrategy` - used with assisted installer where the bootstrap node is repurposed as the final master node, allowing a 3-node cluster to be deployed with only 3 total machines (no separate bootstrap node). The bootstrap node performs a live ISO pivot to become a control-plane member. Particularly useful for bare metal installs
- `UnsafeScalingStrategy` - scaling without regard to node count or quorum

Check `pkg/operator/ceohelpers/bootstrap.go` for implementation.

## Important Constraints

1. **DNS Independence** - The `EtcdEndpointsController` must never depend on DNS (directly or transitively) as it maintains the IP-based ConfigMap that DNS depends on. DNS has no guarantee that a single static IP is returned. Previously, member nodes confused their identity by alternating between IPs. Using only IPs is a core design choice in this operator
2. **CVO Resource Management** - Resources in `manifests/` are managed by CVO; removal requires special handling (see [CVO Resource Removal](#cvo-resource-removal)). The numeric ordering of file names in `manifests/` determines CVO deployment order — incorrect ordering has caused outages in CI (e.g., when a manifest was deployed before its dependency)
3. **Never Remove Controllers** - An agent should never remove a controller. Controller removals are extremely rare (only one in the history of the project) and require cleaning up stale status conditions to prevent upgrade blockers. This is a human decision
4. **Static Pod Revisions** - Follow library-go static pod revision pattern; don't invent custom update logic
5. **Namespace** - Primary namespace for the operator resources is `openshift-etcd-operator`; target namespace for the operand is `openshift-etcd`. Constants are found in `pkg/operator/operatorclient/interfaces.go` and should always be used

## Build and Test

### Common Commands

```bash
# Build all binaries (operator + test extension)
make build

# Run unit tests
make test-unit

# Run e2e tests (requires OpenShift cluster)
make test-e2e

# Verify linting, generated files, vendor
make verify

# Update dependencies
go mod tidy && go mod vendor
```

### Live Debugging

When debugging live systems, use commands in [profiling and debugging](docs/profiling_and_debugging.md).

To run CEO locally against a cluster:
```sh
# Build executable
make build

# Get config
kubectl -n openshift-etcd-operator get configmap etcd-operator-config -o jsonpath='{.data.config\.yaml}' > ./config.yaml

# Scale down cluster-etcd-operator deployment in cluster and exclude from CVO
./hack/unmanage.sh

# "Missing Version" markers
export OPERATOR_IMAGE_VERSION=0.0.1-snapshot
export OPERAND_IMAGE_VERSION=0.0.1-snapshot

# Run local operator
./cluster-etcd-operator \
    --config ./config.yaml
    --namespace openshift-etcd-operator \
    --kubeconfig /path/to/kubeconfig
```

### OTE Test Framework

This repo uses [OpenShift Tests Extension (OTE)](https://github.com/openshift-eng/openshift-tests-extension):

```bash
# Build test binary
make build  # produces cluster-etcd-operator-tests-ext

# List test suites
./cluster-etcd-operator-tests-ext list suites

# Run specific suite with concurrency
./cluster-etcd-operator-tests-ext run-suite openshift/cluster-etcd-operator/all -c 1

# Run with JUnit output
./cluster-etcd-operator-tests-ext run-suite openshift/cluster-etcd-operator/all \
    --junit-path=/tmp/junit-results/junit.xml
```

Test files are in `test/e2e/` and registered via the OTE framework in `cmd/cluster-etcd-operator-tests-ext/`.

## Alerts and Dashboards

Prometheus alerts and Grafana dashboards are **generated from jsonnet**, not hand-edited. In general, avoid adding new alerts — they are difficult to maintain in CI and we add them very rarely.

### Jsonnet Structure

Almost all files in the `jsonnet/` directory can be changed, but the lock file (`jsonnetfile.lock.json`) and `vendor/` folder are off limits.

- `main.jsonnet` - Imports upstream etcd mixin alerts and merges with custom overrides. Contains the `excludedAlerts` list for upstream alerts we replace or disable. **The exclusion list must be in sorted order** — the comparison uses a sort-dedupe, so unsorted entries will silently break
- `custom.libsonnet` - Used for overrides of upstream alerts and addition of new downstream-specific alerts
- `dashboard.jsonnet` - Dashboard definitions. Custom dashboards/panels should be their own dedicated files (e.g., `cpu_iowait_dashboard.json`) and merge-appended into `dashboard.jsonnet`

### Update Process

1. **If changing upstream alerts** (e.g., different thresholds): update upstream alerts first, then exclude the original in `main.jsonnet` and add the override in `custom.libsonnet`
2. **For new downstream alerts**: add them in `custom.libsonnet`
    - New alerts are added only very rarely. They should be avoided unless absolutely necessary
3. Regenerate manifests: `hack/generate.sh`
4. This updates:
   - `manifests/0000_90_etcd-operator_03_prometheusrule.yaml`
   - `manifests/0000_90_cluster-etcd-operator_01-dashboards.yaml`
5. **Verify the alert is reflected correctly in the generated output** — always diff the generated YAML to confirm your changes took effect

**Never** edit the generated YAML files directly.

### Prerequisites

Requires: `jsonnet`, `jb` (jsonnet-bundler), `gojsontoyaml`
See https://github.com/google/jsonnet

## CVO Resource Removal

Resources in `manifests/` are managed by CVO. To remove one:

1. **Don't just delete the manifest file** - it won't be removed from clusters
2. Add the `release.openshift.io/delete: "true"` annotation to the resource
3. CVO will garbage collect it on next update
4. Keep the annotation in manifests for 1 release, then remove the file

See:
- https://github.com/openshift/enhancements/blob/master/enhancements/update/object-removal-manifest-annotation.md
- https://github.com/openshift/cluster-etcd-operator/pull/1016

## Known Bug Patterns

1. **Missing or wrong informer scope** — Always verify informer scope matches what the controller actually needs, and wait for caches to sync (e.g. using the general node lister instead of master-only node lister caused controllers to see non-master nodes and make incorrect scaling decisions)
2. **Insufficient inertia on status conditions** — controllers reacting too quickly to transient state changes cause flapping
3. **Bootstrap member management and quorum** — non-deterministic IP selection, revision stability vs. member removal timing, and quorum safety during member add/remove are the single most-fixed area. Changes here interact differently across release branches
4. **Health check sensitivity** — Liveness probe timeouts and failure thresholds need careful tuning. Too aggressive kills healthy pods. Too relaxed misses real failures
5. **Shell script fragility** — `cluster-backup.sh` and `cluster-restore.sh` have been sources of repeated bugs (caching issues, path errors, retention logic). Changes to these scripts need extra scrutiny
6. **Static pod revision tracking** — The static pod revision mechanism is the critical path for config rollout. Getting the balance wrong between "wait for stability" and "don't block bootstrap" causes cascading issues. Each release branch has subtly different behavior, making backports risky
7. **Upgrade path considerations** — resources and conditions from N-1 releases may still exist. Never assume a clean slate

## TLS and Cipher Suites

TLS certificates are managed by the `etcdcertsigner` and `etcdcertcleaner` controllers. Cipher suite configuration is a known pain point:

- **Bootstrap ciphers** are defined in `pkg/cmd/render/env.go` based on the `TLSProfileIntermediateType` profile from `configv1.TLSProfiles`
- **Runtime ciphers** are sourced from `observedConfig` in `pkg/etcdenvvar/etcd_env.go` and filtered through `tlshelpers.SupportedEtcdCiphers()` which validates each cipher against what etcd actually supports
- **Not all OpenShift ciphers are supported by etcd** — `SupportedEtcdCiphers()` in `pkg/tlshelpers/tlshelpers.go` filters out unsupported ciphers using `tlsutil.GetCipherSuite()`. When OpenShift adds new ciphers, they may silently be dropped if etcd doesn't support them
- When modifying cipher handling, verify that the resulting cipher list is non-empty and that both bootstrap and runtime paths produce consistent results

## UnsupportedConfigOverrides

The operator supports an escape-hatch mechanism via `UnsupportedConfigOverrides` on the operator spec. This is used for unsupported, non-production overrides (e.g., `useUnsupportedUnsafeNonHANonProductionUnstableEtcd`). See `pkg/operator/ceohelpers/unsupported_override.go`.

This mechanism exists because the operator historically had severe stability problems and needed a way to override behavior without a full release cycle. For new feature development, prefer using proper feature gates instead of adding new unsupported config keys.

## Fencing Scripts

Fencing scripts in `bindata/etcd/` (shell scripts) and `bindata/tnfdeployment/` (deployment manifests) should be reviewed for injection vulnerabilities when modified.

## Key Files

- `pkg/operator/starter.go` - Operator entrypoint, initializes all controllers
- `pkg/operator/ceohelpers/bootstrap.go` - Bootstrap scaling logic
- `pkg/operator/etcdendpointscontroller/` - Critical DNS-independent controller
- `pkg/etcdcli/etcdcli.go` - etcd client abstraction
- `bindata/etcd/cluster-backup.sh` - Backup script
- `bindata/etcd/cluster-restore.sh` - Restore script
- `docs/HACKING.md` - Maintenance procedures

## Resources

- [Deep dive overview](docs/overview/overview.md)
- [FAQ](docs/FAQ.md)
- [Telemetry queries](README.md#telemetry-queries)
- [OpenShift Tests Extension](https://github.com/openshift-eng/openshift-tests-extension)
- [library-go documentation](https://github.com/openshift/library-go)
