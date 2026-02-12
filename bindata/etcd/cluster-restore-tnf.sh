#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# example
# ./cluster-restore.sh $path-to-backup
# ETCD_ADVERTISE_IP               - OPTIONAL: Override the IP address that etcd should advertise to cluster peers.
#                                   If not set, the script will auto-detect from etcd.env

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
  echo 'The backup directory is expected to contain the etcd snapshot'
  exit 1
}

# If the argument is not passed, or if it is not a directory, print usage and exit.
if [ "$1" == "" ] || [ ! -d "$1" ]; then
  usage
fi

function get_etcd_advertise_ip() {
  # Try to detect IP from etcd.env (NODE_<nodename>_IP variable)
  NODENAME_UNDERSCORE=$(echo "${NODENAME}" | tr '-' '_')
  NODE_IP_VAR="NODE_${NODENAME_UNDERSCORE}_IP"
  IP="${!NODE_IP_VAR}"
  echo "$IP"
}

function get_peer_node_name() {
  NODENAME_UNDERSCORE=$(echo "${NODENAME}" | tr '-' '_')

  # Find all NODE_*_ETCD_NAME environment variables, exclude current node, get the value
  env | grep -E '^NODE_.*_ETCD_NAME' | grep -v "${NODENAME_UNDERSCORE}" | cut -d= -f2
}

function wait_for_podman_etcd_start() {
  local start=$SECONDS
  local timeout=$((5*60))  # 5 minutes at most
  while [ $((SECONDS - start)) -lt "$timeout" ]; do
    local output
    if ! output=$(pcs status xml | grep 'id="etcd".*role="Started"'); then
      echo "could not detect if etcd is running. Retrying..."
    elif [ "$(echo "$output" | wc -l)" -eq 2 ]; then
      echo "podman-etcd is started"
      return 0
    fi

    sleep 5
  done

  echo "timed out waiting for etcd resources to start (timeout: $timeout seconds)"
  return 1
}

function cleanup_podman_etcd_attributes() {
  # Ensure that none of the podman-etcd's attributes is set (e.g. force_new_cluster, standalone_node, learner_node)
  local peer_node_name

  crm_attribute --delete --name "standalone_node" || true
  crm_attribute --delete --name "learner_node" || true
  crm_attribute --delete --name "force_new_cluster" --lifetime reboot --node "${NODENAME}" || true

  peer_node_name=$(get_peer_node_name)
  if [ "$(echo "$peer_node_name" | wc -w)" -eq 1 ]; then
    crm_attribute --delete --name "force_new_cluster" --lifetime reboot --node "${peer_node_name}" || true
  else
    echo "Warning: could not find peer node name. If restore fails, manually run on the peer node, and try again:" >&2
    echo "  crm_attribute --delete --name force_new_cluster --lifetime reboot --node \"\$(hostname)\"" >&2
  fi
}

function setup_pacemaker_restore() {
  PODMAN_ETCD_CONFIGURATION_FILES=(certs.hash config-previous.tar.gz config.yaml pod.yaml)

  NODENAME=${NODENAME:-$(hostname)}
  if [ -z "${NODENAME}" ] ; then
    echo "could not determine the node hostname. Please set NODENAME env variable and try again"
    exit 1
  fi

  IP=${ETCD_ADVERTISE_IP:-$(get_etcd_advertise_ip)}
  if [ -z "${IP}" ]; then
    echo "could not determine etcd advertise IP address from etcd.env. Please set ETCD_ADVERTISE_IP and try again"
    exit 1
  fi

  # Extend restore flags with Pacemaker-specific arguments
  ETCDCTL_RESTORE_FLAGS=(
    --data-dir="${ETCD_DATA_DIR}"
    --name="$NODENAME"
    --initial-cluster="$NODENAME=https://$IP:2380"
    --initial-advertise-peer-urls="https://$IP:2380"
  )

  if ! pcs resource disable etcd; then
    echo "failed to disable etcd"
    exit 1
  fi

  if ! wait_for_podman_etcd_to_stop; then
    echo "could not wait for podman-etcd to stop"
    exit 1
  fi

  # Clean up any stale CIB attributes
  echo "Clean up any stale CIB attributes"
  cleanup_podman_etcd_attributes

  echo "Clean up any resource agent stale error"
  pcs resource cleanup etcd || true

  # Move podman-etcd configuration files to BACKUP_DIR, to allow snapshot restore (ignore missing files).
  for file in "${PODMAN_ETCD_CONFIGURATION_FILES[@]}"; do
    if [ ! -f "${ETCD_DATA_DIR}/${file}" ]; then
      continue
    fi
    mv "${ETCD_DATA_DIR}/${file}" "${BACKUP_DIR}" || echo "Warning: Failed to move ${file}"
  done
}

BACKUP_DIR="$1"
if [ "$BACKUP_DIR" = "$ETCD_DATA_DIR" ]; then
  echo "The BACKUP_DIR ($BACKUP_DIR) and ETCD_DATA_DIR cannot be the same. Move the snapshot to another directory and try again."
  exit 1
fi

# shellcheck disable=SC2012
SNAPSHOT_FILE=$(ls -vd "${BACKUP_DIR}"/snapshot*.db | tail -1) || true

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

if [ ! -d "${ETCD_DATA_DIR_BACKUP}" ]; then
  mkdir -p "${ETCD_DATA_DIR_BACKUP}"
fi

# Setup TNF/Pacemaker restore
setup_pacemaker_restore

# backup old data-dir
if [ -d "${ETCD_DATA_DIR}/member" ]; then
  if [ -d "${ETCD_DATA_DIR_BACKUP}/member" ]; then
    echo "removing previous backup ${ETCD_DATA_DIR_BACKUP}/member"
    rm -rf "${ETCD_DATA_DIR_BACKUP}"/member
  fi
  echo "Moving etcd data-dir ${ETCD_DATA_DIR}/member to ${ETCD_DATA_DIR_BACKUP}"
  mv "${ETCD_DATA_DIR}"/member "${ETCD_DATA_DIR_BACKUP}"/
fi

echo "removing etcd data dir..."
rm -rf "${ETCD_DATA_DIR}"
mkdir -p "${ETCD_DATA_DIR}"

echo "starting snapshot restore through etcdctl..."
# We are never going to rev-bump here to ensure we don't cause a revision split between the
# remainder of the running cluster and this restore member. Imagine your non-restore quorum members run at rev 100,
# we would attempt to rev bump this with snapshot at rev 120, now this member is 20 revisions ahead and RAFT is confused.
if ! ${ETCD_CLIENT} snapshot restore "${SNAPSHOT_FILE}" "${ETCDCTL_RESTORE_FLAGS[@]}"; then
    echo "Snapshot restore failed. Aborting!"
    exit 1
fi
echo "Snapshot restore succeeded!"

echo "restarting etcd in a new cluster"
# start podman-etcd resource agent to force a new cluster
if ! crm_attribute --lifetime reboot --node "$NODENAME" --name "force_new_cluster" --update "$NODENAME"; then
  echo "could not setup etcd to force a new cluster on restart: crm_attribute error code $?"
  exit 1
fi

if ! pcs resource enable etcd; then
  echo "could not enable podman-etcd: error code $?"
  exit 1
fi
if ! wait_for_podman_etcd_start; then
  # Non-fatal: may fail due to timeout or temporary issues.
  # Continue to print completion message so admin receives troubleshooting instructions.
  echo "could not wait for etcd resources to restart"
fi

print_restore_completion_message
