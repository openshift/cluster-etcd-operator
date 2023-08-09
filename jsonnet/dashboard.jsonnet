local etcdMixin = (import 'github.com/etcd-io/etcd/contrib/mixin/mixin.libsonnet');

{
  apiVersion: 'v1',
  kind: 'ConfigMap',
  metadata: {
    annotations: {
      'include.release.openshift.io/self-managed-high-availability': 'true',
      'include.release.openshift.io/single-node-developer': 'true',
    },
    labels: {
      'console.openshift.io/dashboard': 'true',
    },
    name: 'etcd-dashboard',
    namespace: 'openshift-config-managed',
  },
  data: {
    'etcd.json': std.manifestJsonEx(etcdMixin.grafanaDashboards['etcd.json'] + {rows+: [ (import 'diskjitter_dashboard.json') ]}, ' '),
  },
}
