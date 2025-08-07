local etcdMixin = (import 'github.com/etcd-io/etcd/contrib/mixin/mixin.libsonnet');
local openshiftRules = (import 'custom.libsonnet');

local alertingRules = if std.objectHasAll(etcdMixin, 'prometheusAlerts') then etcdMixin.prometheusAlerts.groups else [];
local promRules = if std.objectHasAll(etcdMixin, 'prometheusRules') then etcdMixin.prometheusRules.groups else [];

// Exclude rules that are either OpenShift specific or do not work for OpenShift.
// List should be ordered!
local excludedAlerts = ['etcdGRPCRequestsSlow', 'etcdHighNumberOfFailedGRPCRequests', 'etcdHighNumberOfLeaderChanges', 'etcdInsufficientMembers'];
local excludeRules = std.map(
  function(group) group {
    rules: std.filter(
      function(rule) !std.setMember(rule.alert, excludedAlerts), super.rules
    ),
  }, alertingRules + promRules
);

// modifiedRules injects runbook_url to all critical alerts on all rules and overrides specific alert expressions.
local modifiedRules = std.map(function(group) group {
  rules: std.map(function(rule)
    if 'alert' in rule && rule.alert == 'etcdHighCommitDurations' then rule {
      expr: |||
        histogram_quantile(0.99, rate(etcd_disk_backend_commit_duration_seconds_bucket{job=~".*etcd.*"}[5m]))
        > on () group_left (type)
        bottomk(0.25,
        0.5 * group by (type) (cluster_infrastructure_provider{type="Azure"})
        or
        0.25 * group by (type) (cluster_infrastructure_provider)
        )
      |||,
    }
    else if 'alert' in rule && !('runbook_url' in rule.annotations) && (rule.labels.severity == 'critical') then rule {
      annotations+: {
        runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/%s.md' % rule.alert,
      },
    } else rule,
    super.rules),
}, excludeRules + openshiftRules.prometheusRules.groups);

{
  apiVersion: 'monitoring.coreos.com/v1',
  kind: 'PrometheusRule',
  metadata: {
    name: 'etcd-prometheus-rules',
    namespace: 'openshift-etcd-operator',
    annotations:
      {
        'include.release.openshift.io/self-managed-high-availability': 'true',
        'include.release.openshift.io/single-node-developer': 'true',
      },
  },
  spec: {
    groups: modifiedRules,
  },
}
