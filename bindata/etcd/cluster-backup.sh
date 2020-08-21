#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# example
# cluster-backup.sh $path-to-snapshot

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
fi

# If the first argument is missing, or it is an existing file, then print usage and exit
if [ -z "$1" ] || [ -f "$1" ]; then
  echo 'Path to backup dir required: ./cluster-backup.sh <path-to-backup-dir>'
  exit 1
fi

BACKUP_DIR=$(realpath "$1")
if [ ! -d "$BACKUP_DIR" ]; then
  mkdir -p "$BACKUP_DIR"
fi

function source_required_dependency {
  local path="$1"
  if [ ! -f "${path}" ]; then
    echo "required dependencies not found, please ensure this script is run on a node with a functional etcd static pod"
    exit 1
  fi
  # shellcheck disable=SC1090
  source "${path}"
}


trap 'podman rm --force cluster-backup' ERR

source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env

podman run  \
  --name cluster-backup \
  --volume /etc/kubernetes/static-pod-resources:/etc/kubernetes/static-pod-resources:ro,z \
  --volume /etc/kubernetes/static-pod-resources/etcd-certs:/etc/kubernetes/static-pod-certs:ro,z \
  --volume "$BACKUP_DIR":/backupdir:rw,z \
  --authfile=/var/lib/kubelet/config.json \
  "${ETCD_OPERATOR_IMAGE}" \
  cluster-etcd-operator \
  cluster-backup \
  --cert "${ETCDCTL_CERT}" \
  --key "${ETCDCTL_KEY}" \
  --cacert "${ETCDCTL_CACERT}" \
  --endpoints https://"${NODE_NODE_ENVVAR_NAME_IP}":2379 \
  --backup-dir /backupdir

podman rm --force cluster-backup

echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}"

