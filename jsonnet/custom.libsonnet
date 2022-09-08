{
  prometheusRules:: {
    groups: [
      {
        name: 'openshift-etcd.rules',
        rules: [
          {
            alert: 'etcdHighNumberOfFailedGRPCRequests',
            expr: |||
              (sum(rate(grpc_server_handled_total{job="etcd", grpc_code=~"Unknown|FailedPrecondition|ResourceExhausted|Internal|Unavailable|DataLoss|DeadlineExceeded"}[5m])) without (grpc_type, grpc_code)
                /
              (sum(rate(grpc_server_handled_total{job="etcd"}[5m])) without (grpc_type, grpc_code)
                > 2 and on ()(sum(cluster_infrastructure_provider{type!~"ipi|BareMetal"} == bool 1)))) * 100 > 10
            |||,
            'for': '10m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": {{ $value }}% of requests for {{ $labels.grpc_method }} failed on etcd instance {{ $labels.instance }}.',
              summary: 'etcd cluster has high number of failed grpc requests.',
            },
          },
          {
            alert: 'etcdHighNumberOfFailedGRPCRequests',
            expr: |||
              (sum(rate(grpc_server_handled_total{job="etcd", grpc_code=~"Unknown|FailedPrecondition|ResourceExhausted|Internal|Unavailable|DataLoss|DeadlineExceeded"}[5m])) without (grpc_type, grpc_code)
                /
              (sum(rate(grpc_server_handled_total{job="etcd"}[5m])) without (grpc_type, grpc_code)
                > 2 and on ()(sum(cluster_infrastructure_provider{type!~"ipi|BareMetal"} == bool 1)))) * 100 > 50
            |||,
            'for': '10m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": {{ $value }}% of requests for {{ $labels.grpc_method }} failed on etcd instance {{ $labels.instance }}.',
              summary: 'etcd cluster has high number of failed grpc requests.',
            },
          },
          {
            alert: 'etcdHighNumberOfLeaderChanges',
            expr: |||
              avg(changes(etcd_server_is_leader[10m])) > 5
            |||,
            'for': '5m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": {{ $value }} average leader changes within the last 10 minutes. Frequent elections may be a sign of insufficient resources, high network latency, or disruptions by other components and should be investigated.',
              summary: 'etcd cluster has high number of leader changes.',
            },
          },
          {
            expr: 'sum(up{job="etcd"} == bool 1 and etcd_server_has_leader{job="etcd"} == bool 1) without (instance,pod) < ((count(up{job="etcd"}) without (instance,pod) + 1) / 2)',
            alert: 'etcdInsufficientMembers',
            'for': '3m',
            annotations: {
              description: 'etcd is reporting fewer instances are available than are needed ({{ $value }}). When etcd does not have a majority of instances available the Kubernetes and OpenShift APIs will reject read and write requests and operations that preserve the health of workloads cannot be performed. This can occur when multiple control plane nodes are powered off or are unable to connect to each other via the network. Check that all control plane nodes are powered on and that network connections between each machine are functional.',
              summary: 'etcd is reporting that a majority of instances are unavailable.',
            },
            labels: {
              severity: 'critical',
            },
          },
        ],
      },
    ],
  },
}
