#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# example
# ./cluster-restore.sh $path-to-backup

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
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
  STATIC_PODS=("$@")

  for POD_FILE_NAME in "${STATIC_PODS[@]}"; do
    BACKUP_POD_PATH=$(tar -tvf "${BACKUP_FILE}" "*${POD_FILE_NAME}" | awk '{ print $6 }') || true
    if [ -z "${BACKUP_POD_PATH}" ]; then
      echo "${POD_FILE_NAME} does not exist in ${BACKUP_FILE}"
      exit 1
    fi

    echo "starting ${POD_FILE_NAME}"
    tar -xvf "${BACKUP_FILE}" --strip-components=2 -C "${MANIFEST_DIR}"/ "${BACKUP_POD_PATH}"
  done
}
function replace_node_specific_parameters() {

  pod_file="$1"
  declare -A SUBS_MAP
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['export ETCD_NAME=.*$']='export ETCD_NAME=${NODE_NODE_ENVVAR_NAME_ETCD_NAME}'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['export ETCD_INITIAL_CLUSTER=.*$']='export ETCD_INITIAL_CLUSTER="${ETCD_NAME}=https://${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}:2380"'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['export ETCD_NODE_PEER_URL=.*$']='export ETCD_NODE_PEER_URL=https://${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}:2380'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--initial-advertise-peer-urls .*$']='--initial-advertise-peer-urls $ETCD_NODE_PEER_URL \\'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--initial-advertise-peer-urls=.*$']='--initial-advertise-peer-urls=https://${NODE_NODE_ENVVAR_NAME_IP}:2380 \\'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--cert-file=.*$']='--cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-serving/etcd-serving-NODE_NAME.crt \\'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--key-file=.*$']='--key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-serving/etcd-serving-NODE_NAME.key \\'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--peer-cert-file=.*$']='--peer-cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-peer/etcd-peer-NODE_NAME.crt \\'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--peer-key-file=.*$']='--peer-key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-peer/etcd-peer-NODE_NAME.key \\'
# shellcheck disable=SC2016,SC1003
  SUBS_MAP['--advertise-client-urls=.*$']='--advertise-client-urls=https://${NODE_NODE_ENVVAR_NAME_IP}:2379 \\'

  for pattern in "${!SUBS_MAP[@]}"; do
    # check if the pattern exists, and return with error if it doesn't
    if ! grep -- "$pattern" "$pod_file" > /dev/null 2>&1; then
      echo "Warning: restore pod extraction failed: expected pattern \"$pattern\" not found."
      return 1
    fi
    # replace the pattern
    sed -i "s,$pattern,${SUBS_MAP[$pattern]},g"  "$pod_file"
  done
}


function extract_and_start_restore_etcd_pod() {
  POD_FILE_NAME="restore-etcd-pod/pod.yaml"
   BACKUP_POD_PATH=$(tar -tvf "${BACKUP_FILE}" "*${POD_FILE_NAME}" | awk '{ print $6 }') || true
   if [ -z "${BACKUP_POD_PATH}" ]; then
     echo "${POD_FILE_NAME} does not exist in ${BACKUP_FILE}"
     return 1
   fi

   echo "starting restored etcd-pod.yaml"
   tar -O -xvf "${BACKUP_FILE}" -C "${MANIFEST_DIR}"/ "${BACKUP_POD_PATH}" > "${MANIFEST_DIR}"/etcd-pod.yaml

   # Since the backup could have been taken on any node, we need to replace the node specific values in the restore pod.
   echo "replacing node specific parameters in restore etcd-pod.yaml"
   if ! replace_node_specific_parameters "${MANIFEST_DIR}"/etcd-pod.yaml; then
     return 1
   fi

   return 0
}


function wait_for_containers_to_stop() {
  CONTAINERS=("$@")

  for NAME in "${CONTAINERS[@]}"; do
    echo "Waiting for container ${NAME} to stop"
    while [[ -n $(crictl ps --label io.kubernetes.container.name="${NAME}" -q) ]]; do
      echo -n "."
      sleep 1
    done
    echo "complete"
  done
}

BACKUP_DIR="$1"
# shellcheck disable=SC2012
BACKUP_FILE=$(ls -vd "${BACKUP_DIR}"/static_kuberesources*.tar.gz | tail -1) || true
# shellcheck disable=SC2012
SNAPSHOT_FILE=$(ls -vd "${BACKUP_DIR}"/snapshot*.db | tail -1) || true
STATIC_POD_LIST=("kube-apiserver-pod.yaml" "kube-controller-manager-pod.yaml" "kube-scheduler-pod.yaml")
STATIC_POD_CONTAINERS=("etcd" "etcdctl" "etcd-metrics" "kube-controller-manager" "kube-apiserver" "kube-scheduler")

if [ ! -f "${SNAPSHOT_FILE}" ]; then
  echo "etcd snapshot ${SNAPSHOT_FILE} does not exist"
  exit 1
fi

# Move manifests and stop static pods
if [ ! -d "$MANIFEST_STOPPED_DIR" ]; then
  mkdir -p "$MANIFEST_STOPPED_DIR"
fi

# Move static pod manifests out of MANIFEST_DIR
for POD_FILE_NAME in "${STATIC_POD_LIST[@]}" etcd-pod.yaml; do
  echo "...stopping ${POD_FILE_NAME}"
  [ ! -f "${MANIFEST_DIR}/${POD_FILE_NAME}" ] && continue
  mv "${MANIFEST_DIR}/${POD_FILE_NAME}" "${MANIFEST_STOPPED_DIR}"
done

# wait for every static pod container to stop
wait_for_containers_to_stop "${STATIC_POD_CONTAINERS[@]}"

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

# Restore static pod resources
tar -C "${CONFIG_FILE_DIR}" -xzf "${BACKUP_FILE}" static-pod-resources

# Copy snapshot to backupdir
cp -p "${SNAPSHOT_FILE}" "${ETCD_DATA_DIR_BACKUP}"/snapshot.db

# Extract and start restore-etcd pod.yaml
if ! extract_and_start_restore_etcd_pod; then
  # If backup doesn't have restore-pod.yaml, copy it from local disk
  echo "Warning: could not extract restore pod. Using the restore pod manifest on the disk."
  if [ ! -f "${RESTORE_ETCD_POD_YAML}" ]; then
    echo "Error: failed to create restore pod yaml. Exiting!"
    exit 1
  fi

  cp -p "${RESTORE_ETCD_POD_YAML}" "${MANIFEST_DIR}/etcd-pod.yaml"
fi

# start remaining static pods
restore_static_pods "${STATIC_POD_LIST[@]}"
