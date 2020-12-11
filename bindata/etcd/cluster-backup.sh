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

function usage {
  echo 'Path to backup dir required: ./cluster-backup.sh [--force] <path-to-backup-dir>'
  exit 1
}

IS_DIRTY=""
if [ "$1" == "--force" ]; then
  IS_DIRTY="__POSSIBLY_DIRTY__"
  shift
fi

# If the first argument is missing, or it is an existing file, then print usage and exit
if [ -z "$1" ] || [ -f "$1" ]; then
  usage
fi

if [ ! -d "$1" ]; then
  mkdir -p "$1"
fi

function check_if_operator_is_progressing {
   operator="$1"

   export KUBECONFIG="/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/localhost.kubeconfig"
   if [ ! -f "${KUBECONFIG}" ]; then
      echo "Valid kubeconfig is not found in kube-apiserver-certs. Exiting!"
      exit 1
   fi

   progressing=$(oc get co "${operator}" -o jsonpath='{.status.conditions[?(@.type=="Progressing")].status}') || true
   if [ "$progressing" == "" ]; then
      echo "Could not find the status of the $operator. Check if the API server is running. Pass the --force flag to skip checks."
      exit 1
   elif [ "$progressing" != "False" ]; then
      echo "Currently the $operator operator is progressing. A reliable backup requires that a rollout is not in progress.  Aborting!"
      exit 1
   fi
}

# backup latest static pod resources
function backup_latest_kube_static_resources {
  RESOURCES=("$@")

  LATEST_RESOURCE_DIRS=()
  for RESOURCE in "${RESOURCES[@]}"; do
    if [ ! -f "/etc/kubernetes/manifests/${RESOURCE}-pod.yaml" ]; then
      echo "error finding manifests for the ${RESOURCE} pod. please check if it is running."
      exit 1
    fi

    LATEST_RESOURCE=$(grep -o -m 1 "/etc/kubernetes/static-pod-resources/${RESOURCE}-pod-[0-9]*" "/etc/kubernetes/manifests/${RESOURCE}-pod.yaml") || true

    if [ -z "$LATEST_RESOURCE" ]; then
      echo "error finding static-pod-resources for the ${RESOURCE} pod. please check if it is running."
      exit 1
    fi
    if [ "${IS_DIRTY}" == "" ]; then
      check_if_operator_is_progressing "${RESOURCE}"
    fi

    echo "found latest ${RESOURCE}: ${LATEST_RESOURCE}"
    LATEST_RESOURCE_DIRS+=("${LATEST_RESOURCE#${CONFIG_FILE_DIR}/}")
  done

  # tar latest resources with the path relative to CONFIG_FILE_DIR
  tar -cpzf "$BACKUP_TAR_FILE" -C "${CONFIG_FILE_DIR}" "${LATEST_RESOURCE_DIRS[@]}"
  chmod 600 "$BACKUP_TAR_FILE"
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

BACKUP_DIR="$1"
DATESTRING=$(date "+%F_%H%M%S")
BACKUP_TAR_FILE=${BACKUP_DIR}/static_kuberesources_${DATESTRING}${IS_DIRTY}.tar.gz
SNAPSHOT_FILE="${BACKUP_DIR}/snapshot_${DATESTRING}${IS_DIRTY}.db"
BACKUP_RESOURCE_LIST=("kube-apiserver" "kube-controller-manager" "kube-scheduler" "etcd")

trap 'rm -f ${BACKUP_TAR_FILE} ${SNAPSHOT_FILE}' ERR

source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

# TODO handle properly
if [ ! -f "$ETCDCTL_CACERT" ] && [ ! -d "${CONFIG_FILE_DIR}/static-pod-certs" ]; then
  ln -s "${CONFIG_FILE_DIR}"/static-pod-resources/etcd-certs "${CONFIG_FILE_DIR}"/static-pod-certs
fi

dl_etcdctl
backup_latest_kube_static_resources "${BACKUP_RESOURCE_LIST[@]}"
ETCDCTL_ENDPOINTS="https://${NODE_NODE_ENVVAR_NAME_IP}:2379" etcdctl snapshot save "${SNAPSHOT_FILE}"
echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}"
