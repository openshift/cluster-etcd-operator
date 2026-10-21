# Partial local etcd backups

`cluster-backup.sh` is installed on nodes running the etcd static pod, including
arbiters. The default invocation still requires all four local static-pod
components and creates the normal `snapshot_*.db` and
`static_kuberesources_*.tar.gz` pair:

```sh
cluster-backup.sh /path/to/backup
```

## Explicitly collecting only available local resources

On a node without all the control-plane static pods, opt in explicitly:

```sh
cluster-backup.sh --available-static-resources /path/to/partial-backup
```

This mode requires the active etcd manifest, its referenced local revision
directory (including its pod manifest), a successful local etcd snapshot, and
successful snapshot integrity verification. It collects locally present
kube-apiserver, kube-controller-manager and kube-scheduler revision directories
referenced by their installed manifests, not the newest directory on disk.
Absent optional manifests or referenced optional revision directories are
reported as omissions. Existing unreadable inputs, invalid/ambiguous revision
references, incomplete existing revision directories, and archive errors fail
the operation rather than becoming omissions. It does not obtain resources
from the API or from another node, and does not infer the node's role from
missing manifests.

Both filenames start with `partial_`, even when all components are present:

- `partial_snapshot_<timestamp><force-marker>_<unique-id>.db`
- `partial_static_kuberesources_<timestamp><force-marker>_<unique-id>.tar.gz`

The archive contains `PARTIAL_BACKUP.txt`, with the selected revisions, omissions
and whether rollout checks were skipped. Prominent warnings identify partial
output. Separate invocations use distinct names, including within one second.
Failed invocations remove their output pair and temporary metadata; they do not
report success. As with other backup files, protect and restrict access to the
output: static-pod resources can contain sensitive configuration.

Both modes stage files in a private temporary directory on the destination
filesystem and publish only after snapshot verification. Publishing uses hard
links and never overwrites existing names; the filesystem must support hard
links. A normal full backup repeated within the same timestamp second fails
rather than replacing the previous backup. Partial runs use unique suffixes.

## Rollout checks and credentials

The new option does **not** disable operator rollout checks. All four operators
are checked unless `--force` is explicitly supplied. The sourced common helper
uses the local kube-apiserver `localhost.kubeconfig`; merely setting an alternate
`KUBECONFIG` before invocation does not replace it. If that local kubeconfig is
absent (as expected on an arbiter), or API access fails, checks fail. No
credentials are fetched or copied to work around this.

Partial mode also requires the local `oc` tool to decode installed and revision
Pod manifests and select actual `hostPath` references (not comments or arbitrary
matching text). This uses `oc set env --local`, a null kubeconfig and an inert
loopback server setting, not API access or kube-apiserver authentication. It
does not change the manifests or resolve Secret references. `--force` skips only
rollout queries, not this offline input validation.

To deliberately skip those checks on an arbiter:

```sh
cluster-backup.sh --available-static-resources --force /path/to/partial-backup
```

`--force` keeps its existing meaning: a rollout might be in progress and the
backup might be inconsistent. The `__POSSIBLY_DIRTY__` marker is retained.
It does not bypass required etcd resources, certificate selection, snapshot
creation/integrity or archive errors. The existing etcd certificate fallback
uses local `static-pod-resources/etcd-certs` when `static-pod-certs` is absent.
The snapshot still targets this node's etcd endpoint. API unavailability does
not prevent a forced local snapshot if the local etcd endpoint, credentials and
tools work; etcd unavailability still fails the operation.

## Recovery limitations

**These artifacts are not qualified full control-plane recovery backups.**
They may omit resources required for recovery, and neither revision/version
consistency across a rollout nor a usable full-cluster restore is guaranteed.
Use the normal control-plane backup procedure for the complete backup contract.

The shell restore scripts (including TNF) and the Go restore command do not
discover `partial_` filenames. Do not rename partial artifacts to normal backup
names or mix them into a standard restore workflow. This option does not add
partial-cluster restore support or change any restore validation. Even a partial
invocation with no omissions is deliberately kept separate from that contract.
Use a separate backup directory to avoid confusing partial output with older
complete backups. Successful fixture tests do not constitute live restore
qualification.
