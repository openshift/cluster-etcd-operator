#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# disable-etcd.sh
# This script will move the etcd static pod into the home/core/assets/manifests-stopped folder and wait for all containers to exit.

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
ETCD_STATIC_POD_CONTAINERS=("etcd" "etcdctl" "etcd-metrics" "etcd-readyz" "etcd-rev")

# always move etcd pod and wait for all containers to exit
mv_static_pods "${ETCD_STATIC_POD_LIST[@]}"
wait_for_containers_to_stop "${ETCD_STATIC_POD_CONTAINERS[@]}"
