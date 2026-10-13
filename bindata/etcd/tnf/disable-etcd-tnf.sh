#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# disable-etcd.sh (TNF version)
# This script will stop the Pacemaker-managed etcd resource and wait for the container to exit.

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

# Stop Pacemaker etcd resource and wait for container to exit
if ! pcs resource disable etcd; then
  echo "failed to disable etcd"
  exit 1
fi

if ! wait_for_podman_etcd_to_stop; then
  echo "failed to stop etcd"
  exit 1
fi
