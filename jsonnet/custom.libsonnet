// EtcdBackup and EtcdBackupPolicy are cluster-scoped, and the backup collectors
// export no namespace label, so any namespace on their series comes from the
// scrape target. Following the alerting style guide for cluster-scoped components
// (https://github.com/openshift/enhancements/blob/master/enhancements/monitoring/alerting-consistency.md#style-guide),
// the backup alerts carry a static namespace label: the namespace of this
// PrometheusRule (see main.jsonnet), where the operator that reconciles these
// resources and exports their metrics runs. The backup Jobs, CronJobs and PVCs
// live in openshift-etcd, but these alerts report on the cluster-scoped
// EtcdBackup and EtcdBackupPolicy objects.
local backupAlertNamespace = 'openshift-etcd-operator';

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
            alert: 'etcdGRPCReadRequestsSlow',
            expr: 'histogram_quantile(0.99, sum(rate(grpc_server_handling_seconds_bucket{job="etcd", grpc_method="Range", grpc_type="unary"}[10m])) without(grpc_type)) > 3',
            'for': '10m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": 99th percentile of gRPC read requests is {{ $value }}s on etcd instance {{ $labels.instance }}.',
              summary: 'etcd grpc read requests are slow',
            },
          },
          {
            alert: 'etcdGRPCWriteRequestsSlow',
            expr: 'histogram_quantile(0.99, sum(rate(grpc_server_handling_seconds_bucket{job="etcd", grpc_method="Txn", grpc_type="unary"}[10m])) without(grpc_type)) > 5',
            'for': '10m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": 99th percentile of gRPC write requests is {{ $value }}s on etcd instance {{ $labels.instance }}.',
              summary: 'etcd grpc write requests are slow',
            },
          },
          {
            alert: 'etcdHighCommitDurations',
            expr: 'histogram_quantile(0.99, rate(etcd_disk_backend_commit_duration_seconds_bucket{job=~".*etcd.*"}[5m])) > 0.5',
            'for': '10m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              description: 'etcd cluster "{{ $labels.job }}": 99th percentile commit durations {{ $value }}s on etcd instance {{ $labels.instance }}.',
              summary: 'etcd cluster 99th percentile commit durations are too high.',
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
              description: 'etcd is reporting the signer ca "{{ $labels.name }}" to have less than two years ({{ printf "%.f" $value }} days) of validity left.',
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
              description: 'etcd is reporting the signer ca "{{ $labels.name }}" to have less than one year ({{ printf "%.f" $value }} days) of validity left.',
              summary: 'etcd has critical signer ca expiration',
            },
            labels: {
              severity: 'critical',
            },
          },
          {
            alert: 'etcdBackupFailed',
            expr: 'etcd_backup_status{status="Failed"} == 1',
            'for': '5m',
            labels: {
              severity: 'warning',
              namespace: backupAlertNamespace,
            },
            annotations: {
              description: 'etcd backup "{{ $labels.etcd_backup }}" has failed. Check the EtcdBackup resource status for details.',
              summary: 'etcd backup has failed.',
            },
          },
          {
            alert: 'etcdBackupHighFailureRate',
            expr: |||
              (
                count(
                  (etcd_backup_status{status="Failed"} == 1)
                  and on(etcd_backup, uid)
                  ((time() - etcd_backup_completion_timestamp_seconds) < 86400)
                )
                /
                count(
                  (
                    (etcd_backup_status{status="Failed"} == 1)
                    or
                    (etcd_backup_status{status="Completed"} == 1)
                  )
                  and on(etcd_backup, uid)
                  ((time() - etcd_backup_completion_timestamp_seconds) < 86400)
                )
              ) > 0.5
            |||,
            'for': '30m',
            labels: {
              severity: 'warning',
              namespace: backupAlertNamespace,
            },
            annotations: {
              description: 'More than 50% of etcd backups finished in the last 24 hours have failed. This indicates a persistent issue with backup configuration or storage that requires investigation.',
              summary: 'etcd backup failure rate is high.',
            },
          },
          {
            alert: 'etcdBackupHighFailureRate',
            expr: |||
              (
                count(
                  (etcd_backup_status{status="Failed"} == 1)
                  and on(etcd_backup, uid)
                  ((time() - etcd_backup_completion_timestamp_seconds) < 86400)
                )
                /
                count(
                  (
                    (etcd_backup_status{status="Failed"} == 1)
                    or
                    (etcd_backup_status{status="Completed"} == 1)
                  )
                  and on(etcd_backup, uid)
                  ((time() - etcd_backup_completion_timestamp_seconds) < 86400)
                )
              ) >= 0.75
            |||,
            'for': '30m',
            labels: {
              severity: 'critical',
              namespace: backupAlertNamespace,
            },
            annotations: {
              description: 'More than 75% of etcd backups finished in the last 24 hours have failed. This is a critical issue that may prevent disaster recovery and requires immediate investigation.',
              summary: 'etcd backup failure rate is critically high.',
            },
          },
          // etcdBackupNoPolicyConfigured deferred until AutomatedEtcdBackup GA:
          // it needs a feature-gate presence signal (e.g. etcd_automated_backup_enabled)
          // so absent(etcd_backup_policy_info) does not fire on Default clusters.
          {
            alert: 'etcdBackupPolicyNoRecentSuccess',
            // Last success is derived from live Completed EtcdBackup metrics (no
            // ConfigMap tracker). Threshold is 2*schedule interval so weekly/monthly
            // policies do not permanently fire. Interval series absence (unparseable
            // schedule) suppresses this alert. Backups are associated with policies
            // by name on the shared etcd_backup_policy label.
            //
            // More than one scrape target can export the same object, e.g. an
            // outgoing and an incoming operator pod whose series are both within
            // the lookback window. Their series differ only in target labels
            // (instance, pod), so every operand that must be unique on its side of
            // a join is first reduced with max: policy series by policy name+uid,
            // backup timestamps by backup name+uid, and the last success by policy
            // name. Creation timestamps are identical across targets; max of the
            // schedule interval picks the larger threshold if targets briefly
            // disagree after a schedule change. Raw etcd_backup_info is only used on
            // the "many" side of group_left or inside set operators.
            // The alert keeps etcd_backup_policy and uid, carried on the "many" side of
            // group_left/group_right; namespace comes from the static backup alert
            // label. The last success is compared
            // per policy object, so two UIDs sharing a name (a recreated policy seen
            // through an outgoing pod) are evaluated against their own interval.
            expr: |||
              (
                (
                  (time() - max by (etcd_backup_policy, uid) (etcd_backup_policy_creation_timestamp_seconds))
                  > on(etcd_backup_policy, uid) group_left()
                  (2 * max by (etcd_backup_policy, uid) (etcd_backup_policy_schedule_interval_seconds))
                )
                unless on(etcd_backup_policy)
                (
                  etcd_backup_info
                  and on(etcd_backup, uid)
                  (etcd_backup_status{status="Completed"} == 1)
                )
              )
              or
              (
                (
                  time()
                  -
                  max by (etcd_backup_policy) (
                    (
                      etcd_backup_info
                      * on(etcd_backup, uid) group_left()
                      max by (etcd_backup, uid) (etcd_backup_completion_timestamp_seconds)
                    )
                    and on(etcd_backup, uid)
                    (etcd_backup_status{status="Completed"} == 1)
                  )
                )
                > on(etcd_backup_policy) group_right()
                (2 * max by (etcd_backup_policy, uid) (etcd_backup_policy_schedule_interval_seconds))
              )
            |||,
            // 5m hold before firing (a policy choice, matching etcdBackupFailed and
            // etcdBackupDurationHigh).
            'for': '5m',
            labels: {
              severity: 'warning',
              namespace: backupAlertNamespace,
            },
            annotations: {
              description: 'Backup policy "{{ $labels.etcd_backup_policy }}" has not completed a successful backup within two schedule intervals. This may prevent disaster recovery.',
              summary: 'Backup policy has no successful backup within two schedule intervals.',
            },
          },
          {
            alert: 'etcdBackupDurationHigh',
            // Fires only while a backup is actively Running and more than 30 minutes
            // have passed since the EtcdBackup was created (as before, this includes
            // any time spent queued before the job started). Completed and Failed
            // backups do not keep this alert firing: success clears it, and failures
            // are reported by etcdBackupFailed / etcdBackupHighFailureRate.
            //
            // The collector exports etcd_backup_status for Running, Completed and
            // Failed on every scrape, with value 1 only for the current lifecycle
            // condition, so == 1 is required and a zero-valued Running series is not
            // active. Every scraped operator target exports the same series for a
            // backup, differing only in target labels (instance, pod, ...). Status and
            // the creation timestamp are reduced to one series per (etcd_backup, uid)
            // before joining, so the alert has one stable identity across operator pod
            // changes; namespace comes from the static backup alert label.
            //
            // If exporters disagree while their series overlap (one cache still shows
            // Running after the backup finished), any exporter reporting Completed or
            // Failed suppresses the alert, since those statuses replace Running.
            // Resolution is bounded by what the metrics show: once no exporter reports
            // the terminal status, an exporter that still reports Running raises the
            // alert again after a new 5m hold, until it observes the terminal status
            // itself or its series go stale. With a single operator replica a restart
            // can leave a gap instead of an overlap; while no exporter reports the
            // backup the alert resolves, and it fires again after a new 5m hold.
            //
            // The age comparison is the left operand of the intersection, so the alert
            // value is the time in seconds since the EtcdBackup was created, as shown
            // in the description.
            expr: |||
              (time() - max by (etcd_backup, uid) (etcd_backup_creation_timestamp_seconds) > 1800)
              and on(etcd_backup, uid)
              (
                max by (etcd_backup, uid) (etcd_backup_status{status="Running"} == 1)
                unless on(etcd_backup, uid)
                (etcd_backup_status{status=~"Completed|Failed"} == 1)
              )
            |||,
            'for': '5m',
            labels: {
              severity: 'warning',
              namespace: backupAlertNamespace,
            },
            annotations: {
              description: 'etcd backup "{{ $labels.etcd_backup }}" is still running {{ $value | humanizeDuration }} after it was created, longer than expected (30 minutes). This time is measured from the creation of the EtcdBackup object and includes any time the backup spent queued before it started running. This may indicate storage performance issues or an issue with the backup process.',
              summary: 'etcd backup duration is high.',
            },
          },
        ],
      },
    ],
  },
}
