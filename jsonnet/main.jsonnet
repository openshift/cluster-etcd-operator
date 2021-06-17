local excludedRules = [
  {
    name: 'etcd',
    rules: [
      { alert: 'etcdHighNumberOfFailedGRPCRequests' },
      { alert: 'etcdInsufficientMembers' },
    ],
  },
];

local etcdMixin = (import 'github.com/etcd-io/etcd/contrib/mixin/mixin.libsonnet');
local openshiftRules = (import 'custom.libsonnet');
local a = if std.objectHasAll(etcdMixin, 'prometheusAlerts') then etcdMixin.prometheusAlerts.groups else [];
local r = if std.objectHasAll(etcdMixin, 'prometheusRules') then etcdMixin.prometheusRules.groups else [];
local o = if std.objectHasAll(openshiftRules, 'prometheusRules') then openshiftRules.prometheusRules.groups else [];
local rules = std.map(function(group) group { rules: std.filter(function(rule) !std.setMember(rule.alert, ['etcdHighNumberOfFailedGRPCRequests', 'etcdInsufficientMembers']), super.rules) }
                      , a + r);

{
  apiVersion: 'monitoring.coreos.com/v1',
  kind: 'PrometheusRule',
  metadata: {
    labels: 'blah',  //cfg.commonLabels + cfg.mixin.ruleLabels,
    name: 'etcd-prometheus-rules',
    namespace: 'openshift-etcd',  //cfg.namespace,
  },
  spec: {
    groups: rules + o,
  },
}
