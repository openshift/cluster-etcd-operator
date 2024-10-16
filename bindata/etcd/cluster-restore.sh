#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# example
# ./cluster-restore.sh $path-to-backup
# ETCD_ETCDCTL_RESTORE - when set this script will use `etcdctl snapshot restore` instead of a restore pod yaml,
#                        which can be used when restoring a single member (e.g. on single node OCP).
#                        Syncing very big snapshots (>8GiB) from the leader might also be expensive, this aids in
#                        keeping the amount of data pulled to a minimum. This option will neither rev-bump nor mark-compact.

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

function usage() {
  echo 'Path to the directory containing backup files is required: ./cluster-restore.sh <path-to-backup>'
  echo 'The backup directory is expected to be contain two files:'
  echo '        1. etcd snapshot'
  echo '        2. A copy of the Static POD resources at the time of backup'
  exit 1
}

# If the argument is not passed, or if it is not a directory, print usage and exit.
if [ "$1" == "" ] || [ ! -d "$1" ]; then
  usage
fi

function restore_static_pods() {
  local backup_file="$1"
  shift
  local static_pods=("$@")

  for pod_file_name in "${static_pods[@]}"; do
    backup_pod_path=$(tar -tvf "${backup_file}" "*${pod_file_name}" | awk '{ print $6 }') || true
    if [ -z "${backup_pod_path}" ]; then
      echo "${pod_file_name} does not exist in ${backup_file}"
      exit 1
    fi

    echo "starting ${pod_file_name}"
    tar -xvf "${backup_file}" --strip-components=2 -C "${MANIFEST_DIR}"/ "${backup_pod_path}"
  done
}

BACKUP_DIR="$1"
# shellcheck disable=SC2012
BACKUP_FILE=$(ls -vd "${BACKUP_DIR}"/static_kuberesources*.tar.gz | tail -1) || true
# shellcheck disable=SC2012
SNAPSHOT_FILE=$(ls -vd "${BACKUP_DIR}"/snapshot*.db | tail -1) || true

ETCD_STATIC_POD_LIST=("etcd-pod.yaml")
ETCD_STATIC_POD_CONTAINERS=("etcd" "etcdctl" "etcd-metrics" "etcd-readyz" "etcd-rev" "etcd-backup-server")

if [ ! -f "${SNAPSHOT_FILE}" ]; then
  echo "etcd snapshot ${SNAPSHOT_FILE} does not exist"
  exit 1
fi

# Download etcdctl and check the snapshot status
dl_etcdctl
check_snapshot_status "${SNAPSHOT_FILE}"

ETCD_CLIENT="${ETCD_ETCDCTL_BIN+etcdctl}"
if [ -n "${ETCD_ETCDUTL_BIN}" ]; then
  ETCD_CLIENT="${ETCD_ETCDUTL_BIN}"
fi

# always move etcd pod and wait for all containers to exit
mv_static_pods "${ETCD_STATIC_POD_LIST[@]}"
wait_for_containers_to_stop "${ETCD_STATIC_POD_CONTAINERS[@]}"

if [ ! -d "${ETCD_DATA_DIR_BACKUP}" ]; then
  mkdir -p "${ETCD_DATA_DIR_BACKUP}"
fi

# backup old data-dir
if [ -d "${ETCD_DATA_DIR}/member" ]; then
  if [ -d "${ETCD_DATA_DIR_BACKUP}/member" ]; then
    echo "removing previous backup ${ETCD_DATA_DIR_BACKUP}/member"
    rm -rf "${ETCD_DATA_DIR_BACKUP}"/member
  fi
  echo "Moving etcd data-dir ${ETCD_DATA_DIR}/member to ${ETCD_DATA_DIR_BACKUP}"
  mv "${ETCD_DATA_DIR}"/member "${ETCD_DATA_DIR_BACKUP}"/
fi

if [ -z "${ETCD_ETCDCTL_RESTORE}" ]; then
  # Restore static pod resources
  tar -C "${CONFIG_FILE_DIR}" -xzf "${BACKUP_FILE}" static-pod-resources

  # Copy snapshot to backupdir
  cp -p "${SNAPSHOT_FILE}" "${ETCD_DATA_DIR_BACKUP}"/snapshot.db
  # Move the revision.json when it exists
  [ ! -f "${ETCD_REV_JSON}" ] ||  mv -f "${ETCD_REV_JSON}" "${ETCD_DATA_DIR_BACKUP}"/revision.json
  # removing any fio perf files left behind that could be deleted without problems
  rm -f "${ETCD_DATA_DIR}"/etcd_perf*

  # ensure the folder is really empty, otherwise the restore pod will crash loop
  if [ -n "$(ls -A "${ETCD_DATA_DIR}")" ]; then
      echo "folder ${ETCD_DATA_DIR} is not empty, please review and remove all files in it"
      exit 1
  fi

  echo "starting restore-etcd static pod"
  cp -p "${RESTORE_ETCD_POD_YAML}" "${MANIFEST_DIR}/etcd-restore-pod.yaml"
else
  echo "removing etcd data dir..."
  rm -rf "${ETCD_DATA_DIR}"
  mkdir -p "${ETCD_DATA_DIR}"

  echo "starting snapshot restore through etcdctl..."
  # We are never going to rev-bump here to ensure we don't cause a revision split between the
  # remainder of the running cluster and this restore member. Imagine your non-restore quorum members run at rev 100,
  # we would attempt to rev bump this with snapshot at rev 120, now this member is 20 revisions ahead and RAFT is confused.
  if ! ${ETCD_CLIENT} snapshot restore "${SNAPSHOT_FILE}" --data-dir="${ETCD_DATA_DIR}"; then
      echo "Snapshot restore failed. Aborting!"
      exit 1
  fi

  # start the original etcd static pod again through the new snapshot
  echo "restoring old etcd pod to start etcd again"
  mv "${MANIFEST_STOPPED_DIR}/etcd-pod.yaml" "${MANIFEST_DIR}/etcd-restore-pod.yaml"
fi

# This ensures kubelet does not get stuck on reporting status of the static pod, see OCPBUGS-42133
systemctl restart kubelet
