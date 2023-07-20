#!/bin/bash

set +x

oc patch clusterversion/version --type='merge' -p "$(cat <<- EOF
spec:
  overrides:
  - group: apps
    kind: Deployment
    name: etcd-operator
    namespace: openshift-etcd-operator
    unmanaged: true
EOF
)"

oc scale deployment -n openshift-etcd-operator --replicas=0 etcd-operator
