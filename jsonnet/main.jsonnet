local etcdMixin = (import 'github.com/etcd-io/etcd/contrib/mixin/mixin.libsonnet');
local openshiftRules = (import 'custom.libsonnet');

local alertingRules = if std.objectHasAll(etcdMixin, 'prometheusAlerts') then etcdMixin.prometheusAlerts.groups else [];
local promRules = if std.objectHasAll(etcdMixin, 'prometheusRules') then etcdMixin.prometheusRules.groups else [];

// Exclude rules that are either OpenShift specific or do not work for OpenShift.
// List should be ordered!
local excludedAlerts = ['etcdDatabaseQuotaLowSpace', 'etcdGRPCRequestsSlow', 'etcdHighCommitDurations', 'etcdHighNumberOfFailedGRPCRequests', 'etcdHighNumberOfLeaderChanges', 'etcdInsufficientMembers', 'etcdMembersDown'];
local excludeRules = std.map(
  function(group) group {
    rules: std.filter(
      function(rule) !std.setMember(rule.alert, excludedAlerts), super.rules
    ),
  }, alertingRules + promRules
);

// Collect alert names for runbook_url annotations.
// By definition, an alerting rule with a critical severity label must have a
// runbook URL.
local alertingRulesWithRunbooks = std.flattenArrays(std.map(
  function(group)
    std.map(
      function(rule)
        rule.alert,
      std.filter(
        function(rule)
          std.objectHas(rule, 'alert') && rule.labels.severity == 'critical',
        group.rules
      )
    ),
  excludeRules + openshiftRules.prometheusRules.groups
));

local modifiedRules = std.map(function(group) group {
  rules: std.map(function(rule) if 'alert' in rule && !('runbook_url' in rule.annotations) && std.member(alertingRulesWithRunbooks, rule.alert) then rule {
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
