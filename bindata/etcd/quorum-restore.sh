#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# ./quorum-restore.sh
# This script attempts to restore quorum by spawning a revision-bumped etcd without membership information.

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
fi

function source_required_dependency {
  local src_path="$1"
  if [ ! -f "${src_path}" ]; then
    echo "required dependencies not found, please ensure this script is run on a node with a functional etcd static pod"
    exit 1
  fi
  # shellcheck disable=SC1090
  source "${src_path}"
}

source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

ETCD_STATIC_POD_LIST=("etcd-pod.yaml")
ETCD_STATIC_POD_CONTAINERS=("etcd" "etcdctl" "etcd-metrics" "etcd-readyz" "etcd-rev" "etcd-backup-server")

# always move etcd pod and wait for all containers to exit
mv_static_pods "${ETCD_STATIC_POD_LIST[@]}"
stop_containers "${ETCD_STATIC_POD_CONTAINERS[@]}"
wait_for_containers_to_stop "${ETCD_STATIC_POD_CONTAINERS[@]}"
await_mirror_pod_removal

echo "starting restore-etcd static pod"
cp "${QUORUM_RESTORE_ETCD_POD_YAML}" "${MANIFEST_DIR}/etcd-pod.yaml"
