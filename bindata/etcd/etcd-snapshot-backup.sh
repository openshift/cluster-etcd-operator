#!/usr/bin/env bash

set -o errexit
set -o pipefail
set -o errtrace

# example
# etcd-snapshot-backup.sh $path-to-snapshot

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
fi

function usage {
  echo 'Path to backup dir required: ./etcd-snapshot-backup.sh <path-to-backup-dir>'
  exit 1
}

# If the first argument is missing, or it is an existing file, then print usage and exit
if [ -z "$1" ] || [ -f "$1" ]; then
  usage
fi

if [ ! -d "$1" ]; then
  mkdir -p $1
fi

# backup latest static pod resources
function backup_latest_kube_static_resources {
  RESOURCES=("$@")

  LATEST_RESOURCE_DIRS=()
  for RESOURCE in "${RESOURCES[@]}"; do
    LATEST_RESOURCE=$(ls -trd "${CONFIG_FILE_DIR}"/static-pod-resources/${RESOURCE}-[0-9]* | tail -1) || true
    if [ -z "$LATEST_RESOURCE" ]; then
      echo "error finding static-pod-resource ${RESOURCE}"
      exit 1
    fi

    echo "found latest ${RESOURCE}: ${LATEST_RESOURCE}"
    LATEST_RESOURCE_DIRS+=("${LATEST_RESOURCE#${CONFIG_FILE_DIR}/}")
  done

  # tar latest resources with the path relative to CONFIG_FILE_DIR
  tar -cpzf $BACKUP_TAR_FILE -C ${CONFIG_FILE_DIR} "${LATEST_RESOURCE_DIRS[@]}"
}

BACKUP_DIR="$1"
DATESTRING=$(date "+%F_%H%M%S")
BACKUP_TAR_FILE=${BACKUP_DIR}/static_kuberesources_${DATESTRING}.tar.gz
SNAPSHOT_FILE="${BACKUP_DIR}/snapshot_${DATESTRING}.db"
BACKUP_RESOURCE_LIST=("kube-apiserver-pod" "kube-controller-manager-pod" "kube-scheduler-pod" "etcd-pod")

trap "rm -f ${BACKUP_TAR_FILE} ${SNAPSHOT_FILE}" ERR

source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

# TODO handle properly
if [ ! -f "$ETCDCTL_CACERT" ] && [ ! -d "${CONFIG_FILE_DIR}/static-pod-certs" ]; then
  ln -s ${CONFIG_FILE_DIR}/static-pod-resources/etcd-certs ${CONFIG_FILE_DIR}/static-pod-certs
fi

dl_etcdctl
backup_latest_kube_static_resources "${BACKUP_RESOURCE_LIST[@]}"
etcdctl snapshot save ${SNAPSHOT_FILE}
echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}"
