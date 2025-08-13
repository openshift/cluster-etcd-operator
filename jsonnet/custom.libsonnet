{
  prometheusRules:: {
    groups: [
      {
        name: 'openshift-etcd.rules',
        rules: [
          {
            alert: 'etcdDatabaseQuotaLowSpace',
            expr: '(last_over_time(etcd_mvcc_db_total_size_in_bytes{job=~".*etcd.*"}[5m]) / last_over_time(etcd_server_quota_backend_bytes{job=~".*etcd.*"}[5m]))*100 > 65',
            'for': '10m',
            labels: {
              severity: 'info',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": database size is 65% of the defined quota on etcd instance {{ $labels.instance }}, please defrag or increase the quota as the writes to etcd will be disabled when it is full.',
              summary: 'etcd cluster database is using >= 65% of the defined quota.',
            },
          },
          {
            alert: 'etcdDatabaseQuotaLowSpace',
            expr: '(last_over_time(etcd_mvcc_db_total_size_in_bytes{job=~".*etcd.*"}[5m]) / last_over_time(etcd_server_quota_backend_bytes{job=~".*etcd.*"}[5m]))*100 > 75',
            'for': '10m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": database size is 75% of the defined quota on etcd instance {{ $labels.instance }}, please defrag or increase the quota as the writes to etcd will be disabled when it is full.',
              summary: 'etcd cluster database is using >= 75% of the defined quota.',
            },
          },
          {
            alert: 'etcdDatabaseQuotaLowSpace',
            expr: '(last_over_time(etcd_mvcc_db_total_size_in_bytes{job=~".*etcd.*"}[5m]) / last_over_time(etcd_server_quota_backend_bytes{job=~".*etcd.*"}[5m]))*100 > 85',
            'for': '10m',
            labels: {
                severity: 'critical',
            },
            annotations: {
                description: 'etcd cluster "{{ $labels.job }}": database size is 85% of the defined quota on etcd instance {{ $labels.instance }}, please defrag or increase the quota as the writes to etcd will be disabled when it is full.',
                summary: 'etcd cluster database is running full.',
            },
          },
          {
            alert: 'etcdGRPCRequestsSlow',
            expr: |||
              histogram_quantile(0.99, sum(rate(grpc_server_handling_seconds_bucket{job="etcd", grpc_method!="Defragment", grpc_type="unary"}[10m])) without(grpc_type))
              > on () group_left (type)
              bottomk(1,
                1.5 * group by (type) (cluster_infrastructure_provider{type="Azure"})
                or
                1 * group by (type) (cluster_infrastructure_provider))
            |||,
            'for': '30m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": 99th percentile of gRPC requests is {{ $value }}s on etcd instance {{ $labels.instance }} for {{ $labels.grpc_method }} method.',
              summary: 'etcd grpc requests are slow',
            },
          },
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
          {
            alert: 'etcdMembersDown',
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": members are down ({{ $value }}).',
              summary: 'etcd cluster members are down.',
            },
            expr: |||
              max without (endpoint) (
                  sum without (instance) (up{job=~".*etcd.*"} == bool 0)
              or
                count without (To) (
                  sum without (instance) (rate(etcd_network_peer_sent_failures_total{job=~".*etcd.*"}[120s])) > 0.01
                )
              )
              > 0
            |||,
            'for': '20m',
            labels: {
              severity: 'critical',
            },
          },
          {
            expr: 'avg(openshift_etcd_operator_signer_expiration_days) by (name) < 730',
            alert: 'etcdSignerCAExpirationWarning',
            'for': '1h',
            annotations: {
              description: 'etcd is reporting the signer ca "{{ $labels.name }}" to have less than two years (({{ printf "%.f" $value }} days) of validity left.',
              summary: 'etcd signer ca is about to expire',
            },
            labels: {
              severity: 'warning',
            },
          },
          {
            expr: 'avg(openshift_etcd_operator_signer_expiration_days) by (name) < 365',
            alert: 'etcdSignerCAExpirationCritical',
            'for': '1h',
            annotations: {
              description: 'etcd is reporting the signer ca "{{ $labels.name }}" to have less than year  (({{ printf "%.f" $value }} days) of validity left.',
              summary: 'etcd has critical signer ca expiration',
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
