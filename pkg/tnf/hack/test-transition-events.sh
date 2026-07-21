#!/usr/bin/bash
# Test script to validate transition event format on a live cluster.
# Creates mock events identical to what our code produces, then verifies
# they appear correctly in `oc get events`.
#
# Usage: ./hack/test-transition-events.sh
# Requires: oc/kubectl with cluster-admin access

set -euo pipefail

NAMESPACE="openshift-etcd"
TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

events=(
  "EtcdTransitionAuthCompleted|PCS authentication completed on all nodes"
  "EtcdTransitionClusterConfigured|Pacemaker cluster configured successfully"
  "EtcdTransitionFencingConfigured|STONITH fencing configured successfully"
  "EtcdTransitionEtcdResourceCreated|Pacemaker etcd resource agent (podman-etcd) configured"
  "EtcdTransitionConstraintsConfigured|Pacemaker ordering and colocation constraints configured"
  "EtcdTransitionStarted|Etcd transition from CEO-controlled to pacemaker-controlled has started"
  "EtcdTransitionWaitingForRemoval|Waiting for CEO to remove static etcd container from all nodes"
  "EtcdTransitionStaticContainerRemoved|Static etcd container removed from all nodes, revision is stable"
  "EtcdTransitionCompleted|Etcd transition to pacemaker-controlled has completed"
)

echo "Creating ${#events[@]} test transition events in ${NAMESPACE}..."

for entry in "${events[@]}"; do
  reason="${entry%%|*}"
  message="${entry#*|}"
  name="test-${reason,,}-$(date +%s%N)"

  oc apply -f - <<EOF
apiVersion: v1
kind: Event
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    tnf-test: "true"
involvedObject:
  kind: Job
  name: tnf-setup-job
  namespace: ${NAMESPACE}
  apiVersion: batch/v1
reason: ${reason}
message: "${message}"
type: Normal
source:
  component: tnf-setup-runner
firstTimestamp: "${TIMESTAMP}"
lastTimestamp: "${TIMESTAMP}"
count: 1
EOF

  echo "  Created: ${reason}"
done

echo ""
echo "Verifying events..."
echo ""
oc get events -n "${NAMESPACE}" --field-selector reason!=="" --sort-by='.lastTimestamp' | grep -i "EtcdTransition" || echo "No EtcdTransition events found!"

echo ""
echo "Cleanup: oc delete events -n ${NAMESPACE} -l tnf-test=true"
echo "  (events auto-expire after ~1 hour)"
