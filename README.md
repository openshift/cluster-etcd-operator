# cluster-etcd-operator

cluster-etcd-operator (CEO) is an operator that handles the scaling of etcd during cluster bootstrap and regular operation. The operator also manages provisioning etcd dependencies such as TLS certificates.

## Developing the CEO 

See [HACKING.md](docs/HACKING.md).

## Frequently Asked Questions

See [FAQ.md](docs/FAQ.md).

## Security Response

If you've found a security issue that you'd like to disclose confidentially
please contact Red Hat's Product Security team. Details at
https://access.redhat.com/security/team/contact

## Telemetry queries

This operator sends some telemetry data from each OpenShift cluster, this data is used to help us engineers observe the clusters and how they behave. For full list of data that is being sent, see [this file](https://github.com/openshift/cluster-monitoring-operator/blob/master/manifests/0000_50_cluster-monitoring-operator_04-config.yaml).

To query this data make sure you have access to telemetry, primary to https://infogw-proxy.api.openshift.com/. If you do not, follow https://help.datahub.redhat.com/docs/interacting-with-telemetry-data this guide.

### Example queries

1. Number of etcd alerts firing.

This gives a good insight if we compared with existing alerts which alerts do not fire and could be adjusted. Or which fire too often and we should look into why.

```console
count by (alertname) (alerts{alertname=~"etcd.+"})
```

2. Number of etcd slow requests by gRPC service and method.

Again this is good to adjust the new etcdGRPCRequestsSlow alert, if one fires too often on clusters, it should be looked into.

```console
count by (grpc_method, grpc_service) (alerts{alertname="etcdGRPCRequestsSlow"})
```

3. Median fsync disk duration by provider.

This gives an insight which provider has slowest disks.
(None is often metal.)

```console
sum without (_id) (quantile by (_id)(0.5, instance:etcd_disk_wal_fsync_duration_seconds:histogram_quantile{quantile="0.99"}) + on(_id) group_left(provider) (topk by (_id) (1, id_provider*0)))
```

4. Median network peer latency by OpenShift version.

```console
sum without (_id) (quantile by (_id)(0.5, instance:etcd_network_peer_round_trip_time_seconds:histogram_quantile{quantile="0.99"}) + on(_id) group_left(version) (topk by (_id) (1, id_version*0)))
```

5. Top 100 clusters by db size.

```console
topk(100, instance:etcd_mvcc_db_total_size_in_use_in_bytes:sum)
```

6. Median disk commit duration by provider.

(None is often metal.)

```console
sum without (_id) (quantile by (_id)(0.5, instance:etcd_disk_backend_commit_duration_seconds:histogram_quantile{quantile="0.99"}) + on(_id) group_left(provider) (topk by (_id) (1, id_provider*0)))
```
