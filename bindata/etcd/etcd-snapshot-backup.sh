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

#backup latest static pod resources for kube-apiserver
function backup_latest_kube_static_resources {
  echo "Trying to backup latest static pod resources.."
  LATEST_STATIC_POD_DIR=$(ls -vd "${CONFIG_FILE_DIR}"/static-pod-resources/kube-apiserver-pod-[0-9]* | tail -1) || true
  if [ -z "$LATEST_STATIC_POD_DIR" ]; then
      echo "error finding static-pod-resources"
      exit 1
  fi

  LATEST_ETCD_STATIC_POD_DIR=$(ls -vd "${CONFIG_FILE_DIR}"/static-pod-resources/etcd-pod-[0-9]* | tail -1) || true
  if [ -z "$LATEST_ETCD_STATIC_POD_DIR" ]; then
      echo "error finding static-pod-resources"
      exit 1
  fi

  # tar up the static kube resources, with the path relative to CONFIG_FILE_DIR
  tar -cpzf $BACKUP_TAR_FILE -C ${CONFIG_FILE_DIR} ${LATEST_STATIC_POD_DIR#$CONFIG_FILE_DIR/} ${LATEST_ETCD_STATIC_POD_DIR#$CONFIG_FILE_DIR/}
}


# main
# If the first argument is missing, or it is an existing file, then print usage and exit
if [ -z "$1" ] || [ -f "$1" ]; then
  usage
fi

if [ ! -d "$1" ]; then
  mkdir -p $1
fi

BACKUP_DIR="$1"
DATESTRING=$(date "+%F_%H%M%S")
BACKUP_TAR_FILE=${BACKUP_DIR}/static_kuberesources_${DATESTRING}.tar.gz
SNAPSHOT_FILE="${BACKUP_DIR}/snapshot_${DATESTRING}.db"

trap "rm -f ${BACKUP_TAR_FILE} ${SNAPSHOT_FILE}" ERR

source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

dl_etcdctl
backup_latest_kube_static_resources
etcdctl snapshot save ${SNAPSHOT_FILE}
echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}!"
