# Restoring etcd backups with API server encryption enabled

Status: draft. Nothing is implemented yet.

## Summary

When encryption at rest is enabled on the API servers, `cluster-restore.sh` can bring
back an etcd snapshot whose encryption keys no longer match the keys that the running
kube-apiserver uses. Depending on how many key rotations happened since the backup was
taken, the outcome ranges from a slow leak of unreadable objects to a cluster where no
secret or configmap can be read.

This document describes the failure modes and the library-go invariants involved, and
proposes a **rollback**: fence every kube-apiserver before the restored etcd serves, then
bring up a single kube-apiserver on the restore node that uses exactly the encryption
config from the time of the backup. Keys created after the backup are discarded. Nothing
ever wrote with them against the restored data, so the cluster's view of its keys and
the data agree again, and library-go rotates forward from there as usual.

## Motivation

Two bug reports, both reproducible every time with the documented restore procedure:

* [OCPBUGS-142084](https://redhat.atlassian.net/browse/OCPBUGS-142084) (4.21): after a
  key rotation and a restore, the kube-apiserver keeps writing with the post-backup key,
  which no longer exists anywhere in the cluster. The next revision the operator rolls
  out doesn't contain that key, and kube-apiserver crashloops on objects written with
  it. Stopping all kube-apiservers and starting the one from the backup worked around
  it, until a node client CA rotation between backup and restore broke node
  authentication.
* [OCPBUGS-104617](https://redhat.atlassian.net/browse/OCPBUGS-104617) (4.18): a restore
  from a backup older than the 7-day rotation interval. The key controller created a
  key with the same ID but different material, the restore node couldn't decrypt data
  written by the other nodes (`cipher: message authentication failed`,
  `no matching key was found for the provided AES transformer`), and the encryption
  controllers deadlocked on `APIServerRevisionNotConverged`. During the outage the
  revision pruner also deleted the revision directories the healthy nodes were running
  from. Recovery took manual patching of the key secret and the on-disk config, then
  disabling and re-enabling encryption.

Neither the restore documentation nor `cluster-restore.sh` mentions encryption today.

## Background

### How library-go manages encryption keys

The encryption controllers in `library-go/pkg/operator/encryption` run inside the
kube-apiserver, openshift-apiserver and authentication operators. For each component:

* **Key secrets** live in `openshift-config-managed` as
  `encryption-key-<component>-<keyID>`, labeled
  `encryption.apiserver.operator.openshift.io/component=<component>`. The data key
  `encryption.apiserver.operator.openshift.io-key` holds the raw key material. Annotations
  record the mode (`aescbc`, `aesgcm`, `identity`, `KMS`), the migration state
  (`migrated-timestamp`, `migrated-resources`) and why the key was created.
* **Key IDs only ever increase.** The newest key is the write key. The provider name in
  the `EncryptionConfiguration` is the key ID, and every value in etcd is prefixed with
  it, for example `k8s:enc:aescbc:v1:3:`.
* **A new key is a read key everywhere before it becomes the write key.** The state
  controller first rolls out a config that only adds the new key for reading, and makes
  it the write key in a later revision. So any key that any API server writes with can
  be read by all of them.
* **The desired config** is computed by the state controller and written to
  `openshift-config-managed/encryption-config-<component>`, then synced into the operand
  namespace as `encryption-config`.
* **How the config reaches the API server depends on the component:**

  | Component | Encrypts | Runs as | Requests arrive | Storage | Encryption config comes from |
  |-----------|----------|---------|-----------------|---------|------------------------------|
  | kube-apiserver | secrets, configmaps | static pod | directly | etcd, directly | Revisioned into `openshift-kube-apiserver/encryption-config-<revision>`, installed on every control-plane node at `/etc/kubernetes/static-pod-resources/kube-apiserver-pod-<revision>/secrets/encryption-config/encryption-config` |
  | openshift-apiserver | routes | deployment | through kube-apiserver aggregation | etcd, directly (`etcd-client` secret) | `openshift-apiserver/encryption-config` secret, mounted by kubelet as a regular secret volume |
  | oauth-apiserver | oauthaccesstokens, oauthauthorizetokens | deployment | through kube-apiserver aggregation | etcd, directly (`etcd-client` secret) | `openshift-oauth-apiserver/encryption-config` secret, mounted by kubelet as a regular secret volume |

  Only kube-apiserver has a config on disk that lives outside etcd. The other two are
  normal workloads: their config is a secret in etcd, which kube-apiserver encrypts like
  any other secret. Clients only reach them through kube-apiserver's API aggregation, but
  they read and write etcd themselves, not through kube-apiserver. They also delegate
  authentication and authorization (TokenReview, SubjectAccessReview) to kube-apiserver.

The invariants that matter for a restore:

1. **The read set shrinks over time.** `state.KeysWithPotentiallyPersistedDataAndNextReadKey`
   keeps keys only down to the newest key that has *all* resources migrated, plus one more.
   After the second rotation that finishes migrating, the oldest key leaves the config.
2. **Next key ID = highest known key ID + 1.** `planNextEncryptionKey` only looks at the
   key secrets in the cluster.
3. **Rotation is time-based.** A new key is created once the write key's
   `migrated-timestamp` is older than the rotation interval (weekly by default).
4. **The prune controller** deletes key secrets that aren't in the read set, keeping the
   10 newest unused ones.
5. **The deployer** finds the deployed config by reading the revision label of the running
   kube-apiserver pods and fetching `encryption-config-<revision>`. If that secret doesn't
   exist, it reports "converged, no encryption" (`deployer/revisionedpod.go`).

### What `cluster-restore.sh` does today

The script only replaces the etcd static pod and data dir on the restore node.
`disable-etcd.sh` stops etcd on the other control-plane nodes. kube-apiserver keeps
running everywhere with whatever it had on disk before the restore. This document uses
these terms:

| Term | Meaning | Where it lives |
|------|---------|----------------|
| **restore node** | The control-plane node where `cluster-restore.sh` runs. | |
| **running revision** | The kube-apiserver static pod revision the nodes run when the restore starts. | `/etc/kubernetes/manifests/kube-apiserver-pod.yaml` |
| **running config** | The encryption config of the running revision. | `/etc/kubernetes/static-pod-resources/kube-apiserver-pod-<running-revision>/secrets/encryption-config/encryption-config` |
| **backup revision** | The kube-apiserver revision at the time the backup was taken. The restored etcd matches this revision. | Recorded in the backup tarball |
| **backup config** | The encryption config of the backup revision. | `encryption_config_*.tar.gz`; in backups made before this change, `static_kuberesources_*.tar.gz` |

`encryption-config` is a revisioned secret of kube-apiserver (`RevisionSecrets` in
cluster-kube-apiserver-operator), so the installer writes it into
`kube-apiserver-pod-<revision>/secrets/encryption-config/encryption-config`.
`cluster-backup.sh` tars that whole revision directory into `static_kuberesources_*.tar.gz`.
So the backup config is already in every backup. For aescbc and aesgcm that means raw
keys sit in the backup directory right next to `snapshot_*.db`; with KMS the config only
references the plugin. [Changes to the backup](#changes-to-the-backup) moves it into a
tarball of its own.

## Problem

Example: at backup time the cluster knows key 1. Key 2 is created and migration
finishes. Then the cluster is restored to a backup that used key 1 and key 2 is unknown.

* etcd now has key secrets {1} and data encrypted with key 1.
* The kube-apiserver on disk has config [2 (write), 1 (read)].

### Failure 1: restored data becomes unreadable

With one rotation since the backup, the running config still contains key 1 and reads
work. After a second rotation the running config is [3, 2] and nothing encrypted with
key 1 can be read. Every operator that needs a secret fails, including the
kube-apiserver operator and installer pods that would roll out a fixed config. **The
cluster can't repair itself through the API.**

### Failure 2: key IDs reused with different key material

The cluster only knows key 1, and its `migrated-timestamp` is as old as the backup, so
the key controller creates key **2** right away with *new* material. The running
kube-apiserver already has a provider named `2` with the *old* material. Anything written
between the restore and the next rollout is encrypted with the old key 2 and becomes
unreadable once the new config arrives. The state controller also drops the old key 2,
because it builds the config only from key secrets in the cluster.

Without a new key the same happens with key 1 as the write key: the next revision knows
nothing about key 2 at all (OCPBUGS-142084). With a new key 2 the control plane splits:
a node that gets the new revision has the new key 2, the others keep the old key 2 on
disk, and neither side can read what the other writes (OCPBUGS-104617). kube-apiserver
then logs `cipher: message authentication failed` or
`no matching key was found for the provided AES transformer`.

### Failure 3: the encryption controllers plan from a wrong starting point, or not at all

The deployer looks at the revision labels of the running kube-apiserver pods:

* **All pods on the running revision:** `encryption-config-<running-revision>` doesn't
  exist in the restored etcd. The deployer returns "converged, no encryption", and the
  state machine plans from a false starting point.
* **Pods on different revisions**, for example once one node picked up a new revision
  and crashloops: the deployer reports not converged, and every encryption controller
  waits with `APIServerRevisionNotConverged`. The node that would have to become ready
  for convergence can't, because it can't decrypt. Nothing heals by itself
  (OCPBUGS-104617).

### Failure 4: the revision pruner deletes the running revision

After the restore, the operator's `currentRevision` status is the one from the backup
and stays stuck there while the rollout fails. The static pod revision pruner protects
`currentRevision`, but not the revisions the nodes actually run. In OCPBUGS-104617 it
deleted the running revision directories on the healthy nodes. Their kube-apiserver kept
running from memory, and sidecar containers crashed on missing files.

### openshift-apiserver and oauth-apiserver

Their data is encrypted in two layers, and the layers come from different places:

| Layer | Encrypted by | Key in the snapshot | Key that decrypts it after the restore |
|-------|--------------|---------------------|----------------------------------------|
| Their objects (routes, OAuth tokens) | openshift-apiserver / oauth-apiserver | backup-time openshift-apiserver / oauth-apiserver key | whatever config their pods load |
| Their encryption config and key secrets | kube-apiserver (they are secrets) | backup-time kube-apiserver key | the **running** kube-apiserver config |

Inside the snapshot both layers are consistent. But the outer layer is read by the
kube-apiserver that is running *after* the restore, which may be several keys further
along. So they inherit failure 1 from kube-apiserver: if the running kube-apiserver
config no longer has the backup-time key, it can't decrypt their config secrets, kubelet
can't refresh their secret volumes, and new or restarted pods can't mount their config.
Meanwhile the old pods keep running with the newer config in memory and write routes
and OAuth tokens with a key the restored cluster doesn't know, which is failure 2 again.

## Goals

* A restore never makes data that was readable before it unreadable.
* Problems are found *before* the existing data dir is moved away.
* Works without a reachable API (the normal disaster recovery case).
* No hand-edited state: library-go ends up with a consistent view and continues with
  its normal rotate, migrate and prune cycle.

## Non-goals

* KMS mode. Remote keys can't be checked offline. The preflight warns and requires an
  explicit override.
* Restoring across OpenShift upgrades, which is already unsupported.
* Two-node clusters with fencing (TNF). They have different guarantees and their own
  restore path (`cluster-restore-tnf.sh`, `disable-etcd-tnf.sh`), so they need a design
  of their own.

## Proposal

### Full roll back to the backup-time keys

The restored etcd only contains data that the backup config can read: the backup was
taken while kube-apiserver wasn't progressing (`cluster-backup.sh` checks this), and a
key is always readable everywhere before anyone writes with it. Keys created after the
backup are only needed for data written after the backup, and the restore throws that
data away.

So if **no API server that knows a post-backup key ever writes to the restored etcd**,
the post-backup keys are not needed at all. The rollback guarantees that by fencing:

1. Stop kube-apiserver on every control-plane node before the restored etcd serves.
2. Start exactly one kube-apiserver, on the restore node, with the backup config.
3. Let the operator roll out a new revision to all nodes, built from the key secrets in
   the restored etcd, which are the backup-time keys.

The key controller will then usually create the next key ID with fresh material
(the "key 2b" case from OCPBUGS-142084). That is harmless now: no data exists under the
old key 2, because nothing was allowed to write with it. This is a normal rotation.

**Post-backup keys must not be added to the single kube-apiserver's config, not even
for reading.** It would carry provider `2` with the old material. Once the operator
creates a new key 2 and rolls it to the other nodes, the single kube-apiserver couldn't
read what they write as `2`, which recreates the split from OCPBUGS-104617.

How this addresses each failure:

| Failure | Addressed by |
|---------|--------------|
| 1: restored data unreadable | The single kube-apiserver uses the backup config, which reads everything in the snapshot. |
| 2: key IDs reused | Fencing: nothing writes with a post-backup key, so a reused ID has no data under the old material. |
| 3: wrong starting point for the controllers | Phase 4 recreates `encryption-config-<running-revision>` with what the single kube-apiserver actually runs. |
| 4: pruner deletes the running revision | The single kube-apiserver runs from a copy the pruner doesn't manage, and the pruner never deletes revisions above `latestAvailableRevision` (see [Revision numbers after the restore](#revision-numbers-after-the-restore)). |
| openshift-apiserver, oauth-apiserver | Their config secrets can be decrypted again, oauth-apiserver is restarted to drop queued writes, and their old pods get no requests (see [openshift-apiserver and oauth-apiserver after the restore](#openshift-apiserver-and-oauth-apiserver-after-the-restore)). |

### Phases

The work is a set of subcommands under `cluster-etcd-operator restore-encryption`. The
logic is too involved for bash (bbolt reads, AES, building YAML), and the restore
scripts only call the subcommands. The operator image is already on every control-plane
node, because the etcd static pod uses it for its `etcd-readyz` and `etcd-rev`
containers.

| Phase | What | Where | Command | Destructive |
|-------|------|-------|---------|-------------|
| 1 | Locate the backup config | restore node | `restore-encryption backup-config` | no |
| 2 | Preflight: prove the snapshot can be read with it | restore node | `restore-encryption preflight` | no |
| 3 | Fence and restore: stop etcd and the API servers, restore etcd | all control-plane nodes | `disable-etcd-and-apiservers.sh` (today `disable-etcd.sh`), `cluster-restore.sh` | yes |
| 4 | API restoration: single kube-apiserver with the backup config, redeploy | restore node | `restore-encryption start-apiserver` | yes |

Phases 1 and 2 must finish before anything is changed. If phase 2 fails, the cluster is
left untouched.

### Phase 1: locate the backup config (`backup-config`)

The rollback needs exactly one piece of key material: the backup config. Every backup
already contains it, so no keys are gathered from the running cluster. Phase 1 finds it
in the backup directory and writes it out as an `EncryptionConfiguration` (mode `0600`,
written atomically) for phases 2 and 4:

| Source | Backups |
|--------|---------|
| `encryption_config_*.tar.gz` | made after [the backup change](#changes-to-the-backup) |
| `static_kuberesources_*.tar.gz` | made before the change; same file, at the same path inside |

It also records the backup revision, taken from the revision directory name in the
tarball.

Not every backup has an encryption config, and not every cluster that has one now had
it at backup time. Phase 1 looks at three things: whether the snapshot contains encrypted
values, whether a backup config was found, and whether the running revision on the
restore node has an encryption config. That gives four cases:

| Snapshot encrypted | Backup config found | Running config exists | Case | What phases 2 and 4 do |
|---|---|---|---|---|
| any | yes | any | Encryption was on at backup time (possibly with an `identity` write key). | The rollback as described. |
| yes | no | any | The admin stored the encryption tarball elsewhere. | Phase 1 fails with a message that names `encryption_config_<datetime>.tar.gz` and says to copy it next to the snapshot. |
| no | no | yes | Encryption was enabled after the backup. | The rollback with an `identity`-only config, see below. |
| no | no | no | Encryption was never enabled. | No encryption config at all. Phase 4 starts kube-apiserver unchanged and skips the bookkeeping. |

**Encryption enabled after the backup.** The restored data is all plaintext, but the
running config may no longer list `identity` for reading once the migration finished,
and it holds post-backup keys. So this is the same problem as an encrypted backup, and it
gets the same fix: phase 1 writes an `EncryptionConfiguration` that only has the
`identity` provider for the encrypted resources, and phase 4 swaps it in like a backup
config. Removing the file instead isn't an option: once encryption is on, the operator's
config observer sets `encryption-provider-config` in the kube-apiserver config and never
removes it (`library-go/pkg/operator/encryption/observer/observe_encryption_config.go`),
so the running revision expects the file. The bookkeeping step is skipped: the backup revision had no encryption config, so
the deployer's "converged, no encryption" for the missing `encryption-config-<running-revision>`
is correct this time. The restored `APIServer` resource says encryption is off, so the
redeployed revision has no encryption config either. The admin has to enable encryption
again after the restore.

### Phase 2: preflight (`preflight`)

This runs on the restore node before anything destructive.

1. **kube-apiserver coverage:** scan the snapshot offline (bbolt, read-only) under the
   prefixes kube-apiserver encrypts (`/kubernetes.io/secrets/`, `/kubernetes.io/configmaps/`)
   and collect the provider prefixes in use, such as `k8s:enc:aescbc:v1:<id>:`. Every ID
   in use must be in the backup config, and `identity` must be in it if any value is
   unprefixed.
2. **Decrypt dry run:** decrypt one value per key ID to prove that the material matches.
3. **openshift-apiserver and oauth-apiserver:** decrypt their `encryption-config` secrets
   from the snapshot with the backup config, then run the same coverage check and dry
   run for routes and OAuth tokens against those configs.
4. **Running revision present:** the restore node still has
   `kube-apiserver-pod-<running-revision>/` with its manifest. Phase 4 runs from it.
5. Print a report and exit non-zero if anything is missing. KMS mode is warned about and
   requires `--skip-kms-check`.

### Phase 3: fence and restore

1. **Fence kube-apiserver.** On every control-plane node other than the restore node,
   the disable script stops etcd as today, and also moves the kube-apiserver manifest
   into the stopped-manifests directory next to etcd's and waits for its containers to
   stop. `cluster-restore.sh` does the same on the restore node. From here until phase 4
   the API is down everywhere, as it is today during a restore. Most of the time spent
   here is kube-apiserver's graceful termination (160s): in the validation run, fencing
   took 2m45s of a 6m40s API outage. Since no request should be served anymore at this
   point, the scripts can stop the containers with a short `crictl stop --timeout`
   instead of waiting for the full graceful termination.
2. **Restart oauth-apiserver.** Once no kube-apiserver runs anywhere, the disable script
   (and `cluster-restore.sh` on the restore node) stops the oauth-apiserver containers on
   its node with `crictl stop`. Kubelet restarts them right away, which is the point: see
   [openshift-apiserver and oauth-apiserver](#openshift-apiserver-and-oauth-apiserver-after-the-restore)
   for why a fresh process is enough. openshift-apiserver needs nothing.
3. **Move post-backup revisions aside.** On every node, move the kube-apiserver revision
   directories above the backup revision from `/etc/kubernetes/static-pod-resources/` to
   `/home/core/assets/static-pod-resources-stopped/`. On the restore node, first copy the
   running revision to `/etc/kubernetes/static-pod-resources/restore-kube-apiserver-pod/`
   for phase 4. See [Revision numbers after the restore](#revision-numbers-after-the-restore)
   for why.
4. **Restore.** The existing etcd snapshot restore, unchanged.

**Renaming `disable-etcd.sh`.** The script no longer only disables etcd, so it should be
renamed to something like `disable-etcd-and-apiservers.sh`. It stops etcd and
kube-apiserver, restarts oauth-apiserver, and moves the post-backup revisions aside. It
is shipped through the `etcd-scripts` ConfigMap (`bindata/etcd/scripts-cm.yaml`), and the
restore documentation of every release tells admins to run `disable-etcd.sh`. So
`disable-etcd.sh` should stay as a thin wrapper that calls the new script, at least until
the documentation of all supported releases uses the new name.

Fencing is done on every cluster, with or without encryption. Without encryption it isn't
needed: the restore bumps the etcd revision and marks it compacted, which makes every
kube-apiserver drop its watch cache and relist. But it doesn't hurt either, and one
procedure for all clusters is simpler for the documentation and the scripts.

The kube-apiserver manifests and revision directories moved aside are **never put
back**. They belong to the running revision with the post-backup keys. Nodes get
kube-apiserver back only through the installer, with a revision built from the restored
cluster. The moved directories contain raw post-backup keys and are deleted once the
redeployment has finished.

### Phase 4: API restoration (`start-apiserver`)

On the restore node, once etcd runs from the snapshot:

1. **Swap the encryption config** in the copy:
   `restore-kube-apiserver-pod/secrets/encryption-config/encryption-config` becomes the
   backup config from phase 1 (or the `identity`-only config, see phase 1).
2. **Start kube-apiserver from the copy.** Write the running revision's pod manifest to
   `/etc/kubernetes/manifests/kube-apiserver-pod.yaml` with the resource-dir hostPath
   changed from `kube-apiserver-pod-<running-revision>` to `restore-kube-apiserver-pod`.
   Everything else, including the revision label, stays as it was.

   kube-apiserver runs with host networking (ports 6443, 6080 and 17697), so there must
   never be two of them on the node. There aren't: phase 3 moved the original manifest
   aside and waited for its containers to stop, and the copy is started under the same
   file name and pod name, so kubelet sees it as the same static pod. When the installer
   later writes a new revision to the same manifest path, kubelet treats it like any
   revision change: it doesn't start a static pod until the previous one with the same
   name has terminated (`allowStaticPodStart` in `pkg/kubelet/pod_workers.go`), and the
   pod's `setup` init container also waits for the old kube-apiserver to release its lock
   file. The copy must never be started under a different file or pod name.

   kube-apiserver uses
   the running revision for everything except the encryption config. See
   [Certificates](#certificates) for what that does and doesn't cover.
3. **Bookkeeping for the deployer (failure 3).** Once the API answers, create
   `openshift-kube-apiserver/encryption-config-<running-revision>` with the backup config,
   so the deployer sees what actually runs. The mirror pod still carries the running
   revision's label, so that's the name the deployer looks up. If the secret already
   exists, the running revision is the backup revision, and the content is identical, so
   nothing has to be done. In the `identity`-only case this step is skipped. The restored
   etcd already contains the backup config as `encryption-config-<backup-revision>`, so
   this step can simply copy that secret to the new name.
4. **Force a redeployment** of etcd, kube-apiserver, kube-controller-manager and
   kube-scheduler, as the restore procedure already does. The kube-apiserver operator
   builds the new revision from the key secrets in the restored etcd, which are the
   backup-time keys, and rolls it out to all nodes. Two gates delay this, and both exist
   in today's restore too:
   * The etcd operator doesn't create a new revision while only one member is healthy
     (`RevisionControllerDegraded: CheckSafeToScaleCluster found 1 healthy member(s) out
     of the 3 required by the HAScalingStrategy`). The procedure has to set
     `useUnsupportedUnsafeNonHANonProductionUnstableEtcd` for the duration of the
     restore, as the disaster recovery tests in openshift/origin (`test/extended/dr`) do,
     and remove it once etcd has all members again.
   * The kube-apiserver operator doesn't render a new configuration while etcd has fewer
     than two live endpoints (`apiServerArguments.etcd-servers has less than two live
     etcd endpoints`).

   So the single kube-apiserver runs from the copy until etcd has scaled up again, which
   took about 25 minutes in the validation run. The copy has to stay in place for that
   long. The installer then picked the fenced nodes first, one at a time, and the restore
   node last.
5. **Rotation continues normally.** With an old backup, the key controller creates the
   next key, the state controller rolls it out as a read key and then as the write key,
   and storage migration rewrites everything.
6. **Clean up** once every node runs a new revision: delete
   `restore-kube-apiserver-pod/` and `/home/core/assets/static-pod-resources-stopped/`.

### Revision numbers after the restore

The answers below come from library-go's static pod controllers
(`pkg/operator/staticpod/...`) and how cluster-kube-apiserver-operator wires them.

**The next revision collides with a directory on disk.** The revision controller only
looks at the revision configmaps in the API. After the restore it creates
`latestAvailableRevision + 1`, which is the backup revision + 1, no matter what runs on
the nodes. The installer doesn't refuse an existing `kube-apiserver-pod-<N>` directory:
it replaces each configmap and secret subdirectory, overwrites the pod manifest, and
leaves everything else in place. Without step 3 of phase 3, that would leave stale
optional resources from the post-backup revision in the new one. That includes
`secrets/encryption-config/` with post-backup keys when the new revision has no
encryption config. And the installer would write into `kube-apiserver-pod-<running-revision>`
when it installs that revision number. After a restore, five or so new revisions come
quickly (forced redeployment, certificate regeneration, key rotation), so this can happen
while the restore node's kube-apiserver still runs from that directory. Moving the
post-backup revisions aside and running from a copy avoids all of this.

**The name of the copy matters.** The pruner treats every directory that starts with
`kube-apiserver-pod` as a revision and parses the rest as a number. A directory called
`kube-apiserver-pod-restore` would make every prune run on that node fail. The copy
therefore doesn't start with that prefix.

**The pruner can't delete what phase 4 runs.** The prune pod gets
`--max-eligible-revision=<latestAvailableRevision>` and never deletes a directory above it,
and it ignores directories outside its prefix. Failure 4 from OCPBUGS-104617 happened
because the operator's revisions moved past the running ones while the rollout was stuck;
in the rollback, the single kube-apiserver doesn't run from a revision directory at all.

**Fenced nodes come back through the installer.** The installer tolerates a missing
manifest, never waits for the old pod, and only waits for other installer pods on the
node. The installer controller even picks nodes without a mirror pod first, then the
restore node, whose mirror pod label (running revision) differs from its
`currentRevision` (backup revision). Nodes are updated one at a time, and a node counts as
done only when its new mirror pod is Ready at the target revision. There is no timeout: a
fenced node whose new kube-apiserver never becomes Ready blocks the rollout, and the admin
has to look at that node, as with any stuck rollout today. The startup monitor isn't
involved, because it only runs on single-node clusters.

### openshift-apiserver and oauth-apiserver after the restore

In the validation run, both operators rolled out new pods shortly after the restore, and
the Endpoints only pointed at those. In general, once the single kube-apiserver can decrypt their config
secrets, kubelet replaces their
running pods with the restored ones. The running pods aren't in the restored etcd as
long as an encryption config change rolls out new pods, which the validation checks. Those mount the backup-time config, which
matches their restored data. Their post-backup keys are thrown away, like
kube-apiserver's.

Fencing kube-apiserver doesn't fence them, though: they talk to etcd directly, so their
old pods, with post-backup keys in memory, can reach the restored etcd as soon as it
serves. Whether they write to it depends on two things, both answered from code
(openshift-apiserver, oauth-apiserver and openshift-kubernetes):

1. **Writes without a request.**
   * openshift-apiserver doesn't write to its own storage on its own. Route host
     allocation runs inside create and update requests, and route status arrives through
     the `/status` endpoint. Its post-start hooks only start caches, or write cluster roles
     and a service account through kube-apiserver once at startup.
   * oauth-apiserver **does**. Its token timeout updater (post-start hook
     `openshift.io-StartTokenTimeoutUpdater`, `pkg/tokenvalidation/timeoutvalidator.go`)
     queues tokens in memory on token review and later flushes them with a Get and an
     Update of `OAuthAccessToken`, through its own loopback client. A flush can happen long
     after the request, after kube-apiserver is gone. It writes with the post-backup key
     whenever the Get succeeds, which is the case as long as its config still reads the
     backup-time key. One such token can't be decrypted after the rollback, which breaks
     listing `oauthaccesstokens` and the storage migration of the next rotation.
   * Token expiry is an etcd lease set at create time, not a controller, so it doesn't write.

   The queue only lives in memory and is only filled by token reviews, which need a
   request. That's why phase 3 restarts the oauth-apiserver containers after every
   kube-apiserver is down: the new process starts with an empty queue, and nothing can
   fill it again until phase 4.
2. **Requests reaching the old pods.** Clients only reach these servers through
   kube-apiserver aggregation, and they delegate token authentication and authorization
   to kube-apiserver, so with every kube-apiserver down no normal request gets through.
   After phase 4, kube-apiserver routes aggregated requests to endpoints
   (`enable-aggregator-routing: "true"` in the operator's default config) and reads them
   from v1 Endpoints in its own storage, which is the restored etcd. Those point at the
   backup-time pod IPs, not at the old pods, until kubelet has replaced them. The
   exceptions are clients that present a `system:masters` client certificate directly to
   a pod IP, and a pod IP that happens to be reused. Neither happens in a normal restore.

### Certificates

Fencing and the single kube-apiserver don't touch certificates, but a restore rolls back
the certificate secrets and CA bundles in etcd just as it rolls back the encryption keys.
kube-apiserver gets its certificates from two places, and they behave differently:

| Where | Examples | After the restore |
|-------|----------|-------------------|
| Revisioned resources in `kube-apiserver-pod-<revision>/` | the encryption config, kubeconfigs, `config.yaml` | Phase 4 runs a copy of the running revision, so these stay current. |
| `kube-apiserver-certs/`, kept up to date by the `kube-apiserver-cert-syncer` container | `client-ca`, `aggregator-client-ca`, the serving certs, `kubelet-client` (`CertConfigMaps` and `CertSecrets` in cluster-kube-apiserver-operator) | The cert syncer copies whatever is in etcd, so they go back to the backup-time versions. |

Using the running revision therefore does **not** keep CA bundles current. If a CA rotated
between backup and restore, the restored `client-ca` doesn't trust client certificates
signed by the new CA. That is the node authentication failure from OCPBUGS-142084, and
it would hit the rollback in the same way.

Expired certificates are possible on both sides:

* **The backup's certificates may have expired** by the time of the restore. Once the
  cert syncer copies them, kube-apiserver serves or trusts expired certificates until the
  operators and the `kube-apiserver-cert-regeneration-controller` container issue new
  ones.
* **The running revision's certificates may have expired** if the cluster was down for a
  long time before the restore. Phase 4 then starts kube-apiserver with expired
  certificates, which is the existing recovery-from-expired-certificates scenario.

None of this is specific to encryption, and today's restore has the same exposure. What
the rollback must not do is make it worse: the fencing window should be as short as
possible, and the preflight should warn when certificates on disk or in the snapshot are
expired or close to expiry. Fixing certificate rollback itself (for example by keeping
the current CA bundles across a restore) is a separate design.

### Changes to the backup

Only kube-apiserver's config lives outside etcd, so only kube-apiserver needs a change
here. The configs and keys of openshift-apiserver and oauth-apiserver are secrets inside
the snapshot already. They are encrypted by kube-apiserver, so the kube-apiserver backup
config is the root key for everything.

When the kube-apiserver revision being backed up has an encryption config, the backup
writes it into a separate tarball:

```
snapshot_<datetime>.db
static_kuberesources_<datetime>.tar.gz    # without secrets/encryption-config/
encryption_config_<datetime>.tar.gz       # only secrets/encryption-config/, mode 0600
```

* **Same paths inside.** The encryption tarball keeps the path
  `static-pod-resources/kube-apiserver-pod-<revision>/secrets/encryption-config/encryption-config`.
  Extracting both tarballs into `/etc/kubernetes` gives exactly what one tarball gives
  today. The resources tarball excludes that path.
* **The name must not start with `static_kuberesources`.** Both restore paths look the
  resources tarball up by that prefix: `cluster-restore.sh` with
  `ls -vd static_kuberesources*.tar.gz | tail -1`, and `findTheLatestRevision` in
  `pkg/cmd/backuprestore` by newest mtime. A file called `static_kuberesources_encryption_*`
  could be picked instead of the real one.
* **Only when there is a config.** Clusters without encryption get the same two files as
  today. With KMS the config has no raw keys, but it's split out too, so there is only
  one layout.
* **The admin can store it apart from the snapshot.** The snapshot and resources
  tarball can go to one place and the encryption tarball to another, with tighter
  access. For a restore, it has to be copied back next to the snapshot, otherwise phase 1
  fails.

Everything that writes, reads or prunes backups has to learn the new file:

| Where | Change |
|-------|--------|
| `bindata/etcd/cluster-backup.sh` | Write the second tarball, exclude the path from the first. |
| `pkg/cmd/backuprestore` (`backuputils.go`, used by `EtcdBackup` and periodic backups) | Same split. The termination log lists the new file with the existing asset type `StaticKubeResources`, see below. |
| `pkg/backuphelpers` (`backupFilePattern`) | Accept `encryption_config_<datetime>.tar.gz`. Otherwise `prune-backups` rejects the file in `deleteByName`, and `ListBackupFiles` never lists it, so old keys pile up on disk forever. |
| `bindata/etcd/cluster-restore.sh`, `pkg/cmd/backuprestore/restoreutils.go` | Extract the encryption tarball when it exists. The etcd restore itself doesn't need it, so it stays optional. |
| `restore-encryption backup-config` | Read the backup config from the encryption tarball, and fall back to the resources tarball for older backups. |
| `test/e2e/backup.go` | Expect the third file on clusters with encryption enabled. |

**Asset type.** `EtcdBackupAssetType` in openshift/api is used on both sides of an
`EtcdBackup`: `spec.assets` requests what to back up (enum `Snapshot;StaticKubeResources`),
and `status.files[].assetType` records what was written. The encryption tarball keeps the
type `StaticKubeResources`: it is part of the static pod resources and only split out
for storage, so requesting `StaticKubeResources` writes both tarballs, and the status
lists both files under that type. A new type would need an API change and a decision on
what requesting it on its own means, and nothing needs to tell the two files apart except
by name.


## Validation

The procedure was run by hand on a three-node OpenShift 5.1 nightly cluster (control-plane
nodes that are also workers), emulating the subcommands with shell scripts. The setup:

1. Enable aescbc encryption and wait until all three components report
   `EncryptionCompleted` with key 1.
2. Take a backup with `cluster-backup.sh` on the restore node, at kube-apiserver revision 8.
   The backup config in the tarball holds key 1 only.
3. Create a namespace, secret, configmap and route after the backup.
4. Force two key rotations for all three components (`unsupportedConfigOverrides`
   `encryption.reason` for kube-apiserver and openshift-apiserver,
   `oauthAPIServer.encryption.reason` for authentication). Afterwards every running
   config is `[3, 2, identity]` at revision 13: key 1 is gone everywhere, so a restore
   without the rollback runs into failures 1 and 2.

Then phases 3 and 4 as described. The node side ran from a privileged pod on the restore
node that reached the other control-plane nodes over ssh, using the same key setup as the
disaster recovery tests in openshift/origin. The pod was created after the backup, so
kubelet deletes it as soon as kube-apiserver serves the restored data. All node-side
steps therefore run as a host process started from the pod, and the steps that need the
API ran from outside the cluster.

Results:

| Check | Result |
|-------|--------|
| Restored data readable through the single kube-apiserver | Yes: all secrets, configmaps and routes could be listed without errors. The objects created after the backup are gone, as expected. |
| Values written with a post-backup key | None. After the restore and the full redeployment, every secret, configmap, route and OAuth access token value in etcd carries the `k8s:enc:aescbc:v1:1:` prefix. That includes an OAuth access token created after the restore. |
| Key IDs reused with different material | None. Each component kept only key 1, and the desired configs were `[1, identity]`. The backup was less than a day old, so no time-based rotation was due; with an older backup the next key is created right away, which is harmless after the rollback. |
| Mirror pods of the fenced nodes | Kubelet removed the ones restored from etcd. Until the redeployment, only the restore node's mirror pod existed, labeled with the running revision. |
| Fenced nodes coming back | The installer brought kube-apiserver back on both fenced nodes first, one at a time, as described in [Revision numbers after the restore](#revision-numbers-after-the-restore). |
| Restore node switching from the copy to a new revision | Clean. The `setup` container got the lock immediately (`flock: getting lock took 0.000006 seconds`), with no entries in `lock.log`, and the new revision runs from a fresh `kube-apiserver-pod-<revision>` directory. |
| openshift-apiserver and oauth-apiserver | Both rolled out new pods after the restore. The Endpoints only pointed at the new pods. |
| API outage | 6m40s, of which 2m45s was waiting for kube-apiserver's graceful termination during the fencing. |

Not covered by this run:

* The oauth-apiserver restart in phase 3 had nothing to drop, because no OAuth tokens
  existed before the restore. A test needs tokens with an inactivity timeout that are in
  use right before the fencing.
* The rotation after the restore, because no rotation was due.
* The certificate topics in [Certificates](#certificates), because no certificate
  rotated or expired between backup and restore.
* The `identity`-only case (encryption enabled after the backup).

## Alternatives

### Roll forward: teach the restored cluster the post-backup keys

The first version of this design. Keep every kube-apiserver running, and make the
restored cluster aware of the keys created after the backup:

1. Gather all current keys before the restore (API, node disk, etcd data dir).
2. On **every** control-plane node, merge the running config with the backup config:
   the newest key stays the write key, all keys are readable.
3. Once the API answers, recreate the missing key secrets in `openshift-config-managed`
   with the migration annotations removed, so library-go treats the newest key as not
   yet migrated, runs a storage migration to it, and only then retires the older keys.

| | Roll forward | Rollback (proposal) |
|---|---|---|
| Post-backup keys | made known to the cluster | thrown away |
| Needs | gathering all keys of all components, merging on every node, reconcile before the key controller acts | fencing every kube-apiserver before the restored etcd serves |
| Race with the key controller | yes: if it creates a new key before the reconcile, IDs collide | none |
| Where the config changes | every control-plane node | the restore node only |
| openshift-apiserver, oauth-apiserver | their post-backup keys have to be gathered from kubelet secret volumes or the etcd data dir, and reconciled too | discarded |

The race was the deciding factor. Step 3 has to happen before the key controller creates
the next key, and with an old backup it does that on its first sync.

### Restore the control plane static pods from the backup

Stop kube-apiserver (and kube-controller-manager and kube-scheduler) on every node and
start the versions from the backup tarball. The encryption config then matches the
restored etcd exactly, and the revision numbers match the operator status.
`cluster-restore.sh` still contains an unused `restore_static_pods` function from when
it worked this way.

This was the workaround in OCPBUGS-142084, and it broke as soon as a certificate rotated
between backup and restore: the backup's kube-apiserver didn't trust the new node client
CA, while kube-controller-manager kept signing with it. The rollback takes the useful
part of this, the backup-time encryption config, and keeps everything else from the
running revision.

### Have the kube-apiserver operator import keys from disk

The kube-apiserver operator (in library-go) would read the encryption config of the
running revision from the nodes and add unknown keys as read keys. That is a roll
forward done by the operator. It can't read node files directly, so it would need a new
path through the installer or a node agent, it only covers kube-apiserver keys, and it
has the same race with the key controller.

### Disable and re-enable encryption after the restore

This is what OCPBUGS-104617 ended up doing. It only works once all data is readable
again, so it needs the same key recovery first, and it rewrites all encrypted resources
twice. It may also never finish: kube-apiserver keeps serving while the migration runs,
and a controller that keeps rewriting a secret or configmap can keep producing data under
a key that the migration is trying to retire. In the rollback, kube-apiserver is down
during the fencing, so nothing can write.

