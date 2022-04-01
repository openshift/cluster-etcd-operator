# etcd Corruption Detection and Recovery

Currently, in etcd 3.5.0-3.5.2, there is a low probability risk of data corruption across the etcd members [Upstream Issue](https://github.com/etcd-io/etcd/issues/13766), [Openshift Documentation](https://access.redhat.com/solutions/6849521). There is a mitigation coming to the Openshift Cluster etcd Operator that enables the etcd 'experimental-initial-corrupt-check' which should help detect corruption and stop the bad member from joining the cluster.

This document will outline some ways to verify if an etcd member is corrupted so that you can be more confident that the corruption recovery steps are necessary and should be taken.

# Corruption Diagnosis
Unfortunately, there's no single command that will give a precise 'corrupt/not corrupt' at this time; however, if most or all of the following return that there might be corruption, then it's likely that there is corruption and recovery steps should be taken. Please note, that a single failure in the following commands does not necessarily mean that corruption has occurred.

If all of the following report no issues, then you do not need to follow the recovery steps.

---

## Check Prometheus for etcd revision divergence
Revision divergence is a natural happening of etcd since it takes time to sync a revision to all members; however, continuing divergence is an indication of corruption. If you have access to run Prometheus queries, you can check the following to graph the current and compact revisions to visualize any divergence.

`max by (pod,job) (etcd_debugging_mvcc_current_revision)`

`max by (pod, job) (etcd_debugging_mvcc_compact_revision)`

The former shows instantaneous revision differences where there can be normal divergence that happens in healthy clusters and could resolve itself quickly. If you're seeing divergence here, check the compact revision as well.

The latter shows a delayed history (0-5 minutes) of revision differences and can show true divergence, where the nodes are not going to resolve quickly and could put the cluster at risk to hit this corruption issue.

---

## Get revision differences
One indicator that there might be corruption is a large difference in revision number. It is normal for some etcd members to lag behind the revision by a few revisions (fewer than 10); if one gets behind, it should catch up to the leader. If a member's revision is largely different from the leader/other members, then it could be a sign that divergence is starting.

1. List the etcd pods

`oc get pods -n openshift-etcd | grep -v etcd-quorum-guard | grep etcd`

2. Log into one of the nodes (choose a healthy node).

`oc rsh -n openshift-etcd [etcd-pod]`

3. Show revisions, these should be all the same or very similar

`etcdctl endpoint status -w fields --cluster`

---

## Check leader elections
Frequent and failing leader elections are a symptom of the etcd cluster having issues.

1. List the etcd pods

`oc get pods -n openshift-etcd | grep -v etcd-quorum-guard | grep etcd`

2. Check the logs for any failures of leader elections

`oc logs -n openshfit-etcd [etcd-pod] | grep "failed to renew lease"`

---

## Check the output of the startup corruption check
The mitigation mentioned above will cause a startup corruption check to run when a member attempts to join the cluster. If this etcd flag is enabled and there's recent corruption, the bad etcd member will crashloop with a 'found data inconsistency with peers' error.

**Important:** This mitigation will be included in Openshift soon, but is currently not in any release. You can follow the steps below to manually enable it.

**Important:** There is a bug with the current implemention of the initial check, and if there's significant delay between the corruption taking place and the check running, the check will essentially no-op and allow the member to join the cluster as if it was non-corrupted; an upstream fix is in the works for this as well.

1. List the etcd pods

`oc get pods -n openshift-etcd | grep -v etcd-quorum-guard | grep etcd`

2. Look in the logs for data inconsistency messages

`oc logs -n openshift-etcd [etcd-pod] | grep "found data inconsistency with peers"`

If the above returns any results, then you have data corruption; if it doesn't, then you likely don't.

## Manually Enabling the Check
If you believe you have corruption, but the corruption check flag isn't enabled for your cluster, you can manually enable it on a specific node and the check will automatically run when the member restarts. This is a completely reversable change.

1. Disable the Cluster Version Operator

`oc scale --replicas 0 -n openshift-cluster-version deployments/cluster-version-operator`

2. Disable the Cluster etcd Operator

`oc scale --replicas 0 -n openshift-etcd-operator deployments/etcd-operator`

3. Login to the bad node

`oc debug node/<node-name>`

4. Allow running host binaries

`chroot /host`

5. Copy /etc/kubernetes/manifests/etcd-pod.yaml to a safe place.
6. Edit /etc/kubernetes/manifests/etcd-pod.yaml, adding the following to the etcd invocation (in the etcd container). Exit the node.

`--experimental-initial-corrupt-check=true`

7. The member should shortly restart with the changes, you can see the pod restart:

`oc get pods -n openshift-etcd | grep -v etcd-quorum-guard | grep etcd`

8. Check the logs of the bad node with the above 'Check the output of the startup corruption check' steps.

# Recovery

If a member is corrupt and you're ready to attempt recovery, please use the following document to remove and re-add the unhealthy member: [Replacing an Openshift etcd member](https://docs.openshift.com/container-platform/4.10/backup_and_restore/control_plane_backup_and_restore/replacing-unhealthy-etcd-member.html#restore-replace-crashlooping-etcd-member_replacing-unhealthy-etcd-member)
