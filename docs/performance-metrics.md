# OpenShift ETCD Performance Metrics

This document is intended to help users mitigate performance issues using Prometheus Metrics.

## Common Bottlenecks

### Memory

ETCD uses bbolt, which stores everything as a single memory mapped file. In OpenShift this is up to 8 GiB big, this file is organized with a B+Tree and chunked into discrete 4k pages which can be flagged in-use or not. On top of that, several in-memory caches/indices on both ETCD and API server are being used to facilitate fast multi-version access and label/field selection respectively.

Due to the indices and caches employed, it is fairly easy to exhaust memory on the control plane even though the actual used size of the etcd database file is tiny. One particular scenario where is this common is when listing all pods in a large cluster or listing many large configmaps/secrets.

Check these metrics:

> process_resident_memory_bytes{job="etcd"}
> 
> process_resident_memory_bytes{job="apiserver"}

It is perfectly normal for etcd to use a multiple of its "in use DB size" as resident memory.

Try to look through the audit log and find service accounts that might be listing pods or configmaps across namespaces.
Another frequent offender are events from chatty operators, which can be found in the audit log or directly from [etcd using KCS6970015](https://access.redhat.com/solutions/6970015).

#### Defrag and Compaction

One way to reduce memory usage is to defrag bbolt and compact the database to the latest revision. It is important to run the defrag after the compaction, so one would execute:

> etctctl compact $(etctctl get / -w json | jq .header.revision)
> 
> etctctl defrag

In more recent OpenShift versions this is done automatically -  compacting from the API server and defrag from the etcd-operator.

Sometimes etcd is quite reluctant to free memory, due to the laziness of the Golang GC. So it is perfectly normal for the resident memory to stay up until memory pressure forces it to go down again.

### CPU

While ETCD is not very heavy user of CPU in general, similar to memory, ETCD is competing for CPU usage with other components on the same hosts.

This query will give you our most offending candidates (API server, OVN and ETCD) in a single chart. 

> sum(node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate{namespace=~"openshift-etcd|openshift-ovn-kubernetes|openshift-apiserver"}) by (namespace)

Especially when dealing with OVN, you might want to drill deeper into what component is using the CPU:

> rate(container_cpu_usage_seconds_total{namespace=~"openshift-etcd|openshift-ovn-kubernetes", container=~"etcd|ovnkube-master|nbdb|northd|sbdb"}[2m])

Keep in mind that this is not exhaustive, there might be something else that runs on the host that consumes lots of CPU and thus starves etcd. It also makes sense to look at the whole picture with:

> sum(node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate{cluster="", node=~"!!! NODE NAME !!!"}) by (pod)
 

### Disk Latency

ETCD is extremely sensitive to high latency disk latency. ETCD utilizes the disk to persist bbolt's memory mapped file, but more importantly, for writing to the write-ahead-log (WAL). The latter is the most important path with writing and executes fsync to ensure the data is durably synchronized to the disk.

From ETCD, you can look at the 99th percentile of the WAL append with:

> histogram_quantile(0.99, sum(rate(etcd_disk_wal_fsync_duration_seconds_bucket{job="etcd"}[5m])) by (instance, le))

To ensure the best performance, this metric should not exceed 10ms. It's thus incredibly important to run OpenShift ETCD on SSD or better NVME drives that offer less than a 1ms.

The backend disk commit can be tracked on 99th percentile with: 

> histogram_quantile(0.99, sum(rate(etcd_disk_backend_commit_duration_seconds_bucket{job="etcd"}[5m])) by (instance, le))

This is used primarily to continuously write the snapshots and rotate old WAL files. High latencies here indicate faulty disk or bandwidth starvation issues (both on read and write).

To check for noisy neighbours on the instance's disk (OVN, logs, etc.), you can check the average write time on the master nodes:

> rate(node_disk_write_time_seconds_total{instance=~".+-master-.+"}[5m]) / rate(node_disk_writes_completed_total{instance=~".+-master-.+"}[5m])

That should reveal whether ETCD is behaving worse or better than what else is running on that host.

### Disk Bandwidth

A common reason for latency to go up is that bandwidth on the disk is exhausted. 

> sum by (namespace, node) (rate(container_fs_writes_bytes_total{namespace=~"(openshift-etcd|openshift-ovn-kubernetes)", node=~".+-master-.+"}[2m]))

On OpenShift this can vary a lot depending on how many and what operators are installed, a rule of thumb is 5-10mb/s for a 100 node cluster.


### Network Latency

OpenShift ETCD communicates primarily with control plane components and via host network. To persist a single write it needs to be acknowledged by at least a second ETCD instance using the Raft consensus protocol. This also warrants to have a low latency host network between the control plane nodes.

To see the pure network latency between the nodes, you can check the round trip time with:

> histogram_quantile(0.99, sum by (instance, le) (rate(etcd_network_peer_round_trip_time_seconds_bucket{job="etcd"}[5m])))

Couple layers up top, you can check the reliability of GRPC. Most common you would see timeouts, failures or unavailability from the GRPC exit codes of the server using:

> (100 * sum(rate(grpc_server_handled_total{job=~".*etcd.*", grpc_code=~"Unknown|FailedPrecondition|ResourceExhausted|Internal|Unavailable|DataLoss|DeadlineExceeded"}[5m])) without (grpc_type, grpc_code)
/ sum(rate(grpc_server_handled_total{job=~".*etcd.*"}[5m])) without (grpc_type, grpc_code)) > 0 

This gives you a percentage (0-100) of failures that helps to track down what instance and what grpc_method was involved. Keep in mind that some methods (like Defragment, Snapshot or Compact) usually take longer than the more common Txn or Range requests. 

