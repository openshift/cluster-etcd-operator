{
  prometheusRules:: {
    groups: [
      {
        name: 'tnf-pacemaker.rules',
        rules: [
          // Cluster-level alerts
          {
            alert: 'TNFNodeCountMismatch',
            expr: 'tnf_cluster_node_count_as_expected == 0',
            'for': '5m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              summary: 'TNF cluster node count does not match expected topology.',
              description: 'The TNF cluster reports an unexpected number of nodes. This may indicate a node was added or removed outside the expected two-node topology, potentially breaking quorum assumptions.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeCountMismatch.md',
            },
          },
          {
            alert: 'TNFClusterInMaintenance',
            expr: 'tnf_cluster_in_service == 0',
            'for': '2m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              summary: 'TNF cluster is in maintenance mode.',
              description: 'The TNF Pacemaker cluster has been placed into maintenance mode. Automated failover and fencing are disabled. The cluster will not recover from failures until maintenance mode is cleared.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFClusterInMaintenance.md',
            },
          },
          // Node-level alerts
          {
            alert: 'TNFNodeOffline',
            expr: 'tnf_node_online == 0',
            'for': '2m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              summary: 'TNF node {{ $labels.node }} is offline.',
              description: 'TNF node {{ $labels.node }} has been offline for more than 2 minutes. This may indicate a node failure, reboot, or network partition. The cluster is operating in a degraded single-node state.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeOffline.md',
            },
          },
          {
            alert: 'TNFNodeFencingUnavailable',
            expr: 'tnf_node_fencing_available == 0',
            'for': '5m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              summary: 'Fencing is unavailable for TNF node {{ $labels.node }}.',
              description: 'No fencing devices are available for TNF node {{ $labels.node }}. Without fencing, the cluster cannot safely recover from a split-brain scenario. Check BMC reachability and credentials.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeFencingUnavailable.md',
            },
          },
          {
            alert: 'TNFNodeFencingDegraded',
            expr: 'tnf_node_fencing_healthy == 0 and tnf_node_fencing_available == 1',
            'for': '10m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              summary: 'Fencing is degraded for TNF node {{ $labels.node }}.',
              description: 'Fencing devices for TNF node {{ $labels.node }} are available but not fully healthy. Fencing can still operate but with reduced redundancy. Investigate fence device health.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeFencingDegraded.md',
            },
          },
          {
            alert: 'TNFNodeUnclean',
            expr: 'tnf_node_clean == 0',
            'for': '5m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              summary: 'TNF node {{ $labels.node }} is in an unclean state.',
              description: 'TNF node {{ $labels.node }} is marked unclean by Pacemaker. This indicates a fencing, communication, or configuration issue that must be resolved before the node can rejoin the cluster.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeUnclean.md',
            },
          },
          {
            alert: 'TNFNodeInMaintenance',
            expr: 'tnf_node_in_service == 0',
            'for': '2m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              summary: 'TNF node {{ $labels.node }} is in maintenance mode.',
              description: 'TNF node {{ $labels.node }} has been placed into maintenance mode. Resources on this node will not be managed by Pacemaker until maintenance mode is cleared.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeInMaintenance.md',
            },
          },
          {
            alert: 'TNFNodeStandby',
            expr: 'tnf_node_active == 0',
            'for': '5m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              summary: 'TNF node {{ $labels.node }} is in standby mode.',
              description: 'TNF node {{ $labels.node }} is in standby mode and not actively running resources. This reduces cluster capacity. Remove standby mode to restore full capacity.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFNodeStandby.md',
            },
          },
          // Resource-level alerts
          {
            alert: 'TNFResourceStopped',
            expr: 'tnf_resource_started == 0 and on(node) tnf_node_active == 1',
            'for': '5m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              summary: 'TNF resource {{ $labels.resource }} is stopped on node {{ $labels.node }}.',
              description: 'TNF resource {{ $labels.resource }} on node {{ $labels.node }} has been stopped for more than 5 minutes. This may indicate quorum loss or a failed resource action.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFResourceStopped.md',
            },
          },
          {
            alert: 'TNFResourceFailed',
            expr: 'tnf_resource_operational == 0 and on(node) tnf_node_active == 1',
            'for': '2m',
            labels: {
              severity: 'critical',
            },
            annotations: {
              summary: 'TNF resource {{ $labels.resource }} has failed on node {{ $labels.node }}.',
              description: 'TNF resource {{ $labels.resource }} on node {{ $labels.node }} is not operational. This may indicate a resource agent failure or configuration error requiring immediate investigation.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFResourceFailed.md',
            },
          },
          {
            alert: 'TNFResourceUnmanaged',
            expr: 'tnf_resource_managed == 0 and on(node) tnf_node_in_service == 1 and on(node) tnf_node_active == 1',
            'for': '5m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              summary: 'TNF resource {{ $labels.resource }} is unmanaged on node {{ $labels.node }}.',
              description: 'TNF resource {{ $labels.resource }} on node {{ $labels.node }} has been placed in unmanaged mode. Pacemaker will not monitor or recover this resource until it is re-managed.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFResourceUnmanaged.md',
            },
          },
          {
            alert: 'TNFResourceDisabled',
            expr: 'tnf_resource_enabled == 0 and on(node) tnf_node_active == 1',
            'for': '5m',
            labels: {
              severity: 'warning',
            },
            annotations: {
              summary: 'TNF resource {{ $labels.resource }} is disabled on node {{ $labels.node }}.',
              description: 'TNF resource {{ $labels.resource }} on node {{ $labels.node }} has been disabled. The resource will not be started by Pacemaker until it is re-enabled.',
              runbook_url: 'https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/TNFResourceDisabled.md',
            },
          },
        ],
      },
    ],
  },
}
