#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# example
# ./cluster-restore.sh $path-to-backup

if [[ "$EUID" -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
fi

function usage {
  echo 'Path to the directory containing backup files is required: ./cluster-restore.sh <path-to-backup>'
  echo 'The backup directory is expected to be contain two files:'
  echo '        1. etcd snapshot'
  echo '        2. A copy of the Static POD resources at the time of backup'
  exit 1
}

function source_required_dependency {
  local path="$1"
  if [ ! -f "${path}" ]; then
    echo "required dependencies not found, please ensure this script is run on a node with a functional etcd static pod"
    exit 1
  fi
  # shellcheck disable=SC1090
  source "${path}"
}

function dl_ceo {
  local ceoimg ceoctr ceomnt
  ceoimg="${ETCD_OPERATOR_IMAGE}"
  podman image pull "${ceoimg}" --authfile=/var/lib/kubelet/config.json
  ceoctr=$(podman create "${ceoimg}" --authfile=/var/lib/kubelet/config.json)
  ceomnt=$(podman mount "${ceoctr}")
  cp "${ceomnt}"/bin/cluster-etcd-operator /tmp
  umount "${ceomnt}"
  podman rm "${ceoctr}"
}

# If the argument is not passed, or if it is not a directory, print usage and exit.
if [ "$1" == "" ] || [ ! -d "$1" ]; then
  usage
fi

BACKUP_DIR="$1"
source_required_dependency "/etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env"
# Download CEO and run it on master for crio to work properly.
dl_ceo
trap 'rm -f /tmp/cluster-etcd-operator' ERR
/tmp/cluster-etcd-operator cluster-restore --backup-dir "$BACKUP_DIR"
rm -f /tmp/cluster-etcd-operator
