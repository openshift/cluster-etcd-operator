#!/usr/bin/env bash

set -o errexit
set -o pipefail
set -o errtrace

# example
# ./etcd-snapshot-restore.sh $path-to-backup 

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
fi

source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

function usage {
  echo 'Path to the directory containing backup files is required: ./etcd-snapshot-restore.sh <path-to-backup>'
  echo 'The backup directory is expected to be contain two files:'
  echo '        1. etcd snapshot'
  echo '        2. A copy of the Static POD resources at the time of backup'
  exit 1
}

# If the argument is not passed, or if it is not a directory, print usage and exit.
if [ "$1" == "" ] || [ ! -d "$1" ]; then
  usage
fi

BACKUP_DIR="$1"
BACKUP_FILE=$(ls -vd "${BACKUP_DIR}"/static_kuberesources*.tar.gz | tail -1) || true
SNAPSHOT_FILE=$(ls -vd "${BACKUP_DIR}"/snapshot*.db | tail -1) || true

if [ ! -f "${SNAPSHOT_FILE}" ]; then
  echo "etcd snapshot ${SNAPSHOT_FILE} does not exist."
  exit 1
fi

# Move manifests and stop static pods
if [ ! -d "$MANIFEST_STOPPED_DIR" ]; then
  mkdir -p $MANIFEST_STOPPED_DIR
fi

# Move static pod manifests out of MANIFEST_DIR
find ${MANIFEST_DIR} \
  -maxdepth 1 \
  -type f \
  -printf '...stopping %P\n' \
  -exec mv {} ${MANIFEST_STOPPED_DIR} \;

# Wait for pods to stop
sleep 30

# //TO DO: verify using crictl that etcd and other pods stopped.

# Remove data dir
echo "Moving etcd data-dir ${ETCD_DATA_DIR}/member to ${ETCD_DATA_DIR_BACKUP}"
[ ! -d ${ETCD_DATA_DIR_BACKUP} ]  && mkdir -p ${ETCD_DATA_DIR_BACKUP}
mv ${ETCD_DATA_DIR}/member ${ETCD_DATA_DIR_BACKUP}/member

# Copy snapshot to backupdir
if [ ! -d ${ETCD_DATA_DIR_BACKUP} ]; then
  mkdir -p ${ETCD_DATA_DIR_BACKUP}
fi
cp -p ${SNAPSHOT_FILE} ${ETCD_DATA_DIR_BACKUP}/snapshot.db

# Copy etcd restore pod yaml
cp -p ${RESTORE_ETCD_POD_YAML} ${MANIFEST_DIR}/etcd-pod.yaml

# Restore static pod resources
tar -C ${CONFIG_FILE_DIR} -xzf ${BACKUP_FILE} static-pod-resources
