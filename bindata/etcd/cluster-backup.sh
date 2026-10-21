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
  echo 'Usage: ./cluster-backup.sh [--force] [--available-static-resources] <path-to-backup-dir>'
  echo '--available-static-resources: save partial local resources, NOT a full control-plane recovery backup.'
  echo '--force: skip operator rollout checks; the backup may be inconsistent.'
  exit 1
}

IS_DIRTY=""
AVAILABLE_STATIC_RESOURCES=false
BACKUP_DIR=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --force) IS_DIRTY="__POSSIBLY_DIRTY__" ;;
    --available-static-resources) AVAILABLE_STATIC_RESOURCES=true ;;
    --)
      shift
      if [ "$#" -ne 1 ] || [ -n "${BACKUP_DIR}" ]; then
        usage
      fi
      BACKUP_DIR="$1"
      shift
      break
      ;;
    -*) usage ;;
    *) [ -z "${BACKUP_DIR}" ] || usage; BACKUP_DIR="$1" ;;
  esac
  shift
done

# If the first argument is missing, or it is an existing file, then print usage and exit
if [ -z "${BACKUP_DIR}" ] || [ -f "${BACKUP_DIR}" ]; then
  usage
fi

if [ ! -d "${BACKUP_DIR}" ]; then
  mkdir -p "${BACKUP_DIR}"
fi
BACKUP_DIR=$(cd -- "${BACKUP_DIR}" && pwd -P)

function check_if_operator_is_progressing {
   local operator="$1"

   if [ ! -f "${KUBECONFIG}" ]; then
      echo "Valid kubeconfig is not found in kube-apiserver-certs. Exiting! Pass --force only to explicitly skip rollout checks."
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
  local backup_tar_file="$1"
  local backup_resource_list=("kube-apiserver" "kube-controller-manager" "kube-scheduler" "etcd")
  local latest_resource_dirs=()

  for resource in "${backup_resource_list[@]}"; do
    if [ ! -f "/etc/kubernetes/manifests/${resource}-pod.yaml" ]; then
      echo "error finding manifests for the ${resource} pod. please check if it is running."
      exit 1
    fi

    local latest_resource
    latest_resource=$(grep -o -m 1 "/etc/kubernetes/static-pod-resources/${resource}-pod-[0-9]*" "/etc/kubernetes/manifests/${resource}-pod.yaml") || true

    if [ -z "${latest_resource}" ]; then
      echo "error finding static-pod-resources for the ${resource} pod. please check if it is running."
      exit 1
    fi
    if [ "${IS_DIRTY}" == "" ]; then
      check_if_operator_is_progressing "${resource}"
    fi

    echo "found latest ${resource}: ${latest_resource}"
    latest_resource_dirs+=("${latest_resource#"${CONFIG_FILE_DIR}/"}")
  done

  # tar latest resources with the path relative to CONFIG_FILE_DIR
  tar -cpzf "$backup_tar_file" -C "${CONFIG_FILE_DIR}" "${latest_resource_dirs[@]}"
  chmod 600 "$backup_tar_file"
}

function manifest_resource_revision {
  local manifest="$1" resource="$2" pod_paths host_path relative_path revision_number reference references=""
  if ! pod_paths=$(oc --kubeconfig=/dev/null --server=https://127.0.0.1:1 set env --local -f "${manifest}" \
    -o 'jsonpath={.apiVersion}{"\n"}{.kind}{"\n"}{range .spec.volumes[*]}{.hostPath.path}{"\n"}{end}'); then
    echo "Invalid or unreadable Pod manifest: ${manifest}" >&2
    return 1
  fi
  if [[ "${pod_paths}" != $'v1\nPod\n'* ]]; then
    echo "Invalid Pod manifest or missing hostPath volumes: ${manifest}" >&2
    return 1
  fi
  while IFS= read -r host_path; do
    case "${host_path}" in
      "${CONFIG_FILE_DIR}/static-pod-resources/${resource}-pod-"*)
        relative_path="${host_path#"${CONFIG_FILE_DIR}/static-pod-resources/${resource}-pod-"}"
        revision_number="${relative_path%%/*}"
        if [[ ! "${revision_number}" =~ ^[0-9]+$ ]] || \
           [[ "/${relative_path}/" == *'/../'* ]] || [[ "/${relative_path}/" == *'/./'* ]]; then
          echo "Invalid or unreadable revision reference in ${manifest}" >&2
          return 1
        fi
        reference="${CONFIG_FILE_DIR}/static-pod-resources/${resource}-pod-${revision_number}"
        if [ -n "${references}" ] && [ "${references}" != "${reference}" ]; then
          echo "Invalid or unreadable revision reference in ${manifest}" >&2
          return 1
        fi
        references="${reference}"
        ;;
    esac
  done <<< "${pod_paths#$'v1\nPod\n'}"
  if [ -z "${references}" ]; then
    echo "Invalid or unreadable revision reference in ${manifest}" >&2
    return 1
  fi
  printf '%s\n' "${references}"
}

function backup_available_kube_static_resources {
  local resource manifest revision archived_revision
  local resource_dirs=()
  local metadata="${WORK_DIR}/PARTIAL_BACKUP.txt"
  printf '%s\n' 'PARTIAL local backup: NOT qualified for full control-plane recovery.' \
    "Rollout checks skipped: ${IS_DIRTY:-no}" > "${metadata}"
  if [ ! -d "${MANIFEST_DIR}" ] || [ ! -r "${MANIFEST_DIR}" ] || [ ! -x "${MANIFEST_DIR}" ] || \
     [ ! -d "${CONFIG_FILE_DIR}/static-pod-resources" ] || \
     [ ! -r "${CONFIG_FILE_DIR}/static-pod-resources" ] || [ ! -x "${CONFIG_FILE_DIR}/static-pod-resources" ]; then
    echo 'Required manifest/resource parent directories are missing or unreadable' >&2
    exit 1
  fi

  for resource in etcd kube-apiserver kube-controller-manager kube-scheduler; do
    if [ "${IS_DIRTY}" == "" ]; then
      check_if_operator_is_progressing "${resource}"
    fi
    manifest="${MANIFEST_DIR}/${resource}-pod.yaml"
    if [ ! -e "${manifest}" ] && [ ! -L "${manifest}" ]; then
      if [ "${resource}" == "etcd" ]; then
        echo "Required etcd manifest is missing: ${manifest}" >&2
        exit 1
      fi
      printf 'OMITTED %s: local manifest is absent\n' "${resource}" | tee -a "${metadata}"
      continue
    fi
    if [ ! -f "${manifest}" ] || [ ! -r "${manifest}" ]; then
      echo "Invalid or unreadable manifest: ${manifest}" >&2
      exit 1
    fi
    revision=$(manifest_resource_revision "${manifest}" "${resource}")
    if [ ! -e "${revision}" ] && [ ! -L "${revision}" ]; then
      if [ "${resource}" == "etcd" ]; then
        echo "Required etcd revision is missing: ${revision}" >&2
        exit 1
      fi
      printf 'OMITTED %s: referenced local revision is absent: %s\n' "${resource}" "${revision}" | tee -a "${metadata}"
      continue
    fi
    if [ -L "${revision}" ] || [ ! -d "${revision}" ] || [ ! -r "${revision}" ] || [ ! -x "${revision}" ] || \
       [ ! -f "${revision}/${resource}-pod.yaml" ] || [ ! -r "${revision}/${resource}-pod.yaml" ]; then
      echo "Invalid or unreadable revision resources: ${revision}" >&2
      exit 1
    fi
    archived_revision=$(manifest_resource_revision "${revision}/${resource}-pod.yaml" "${resource}")
    if [ "${archived_revision}" != "${revision}" ]; then
      echo "Installed and revision manifests disagree: ${manifest}" >&2
      exit 1
    fi
    printf 'INCLUDED %s: %s\n' "${resource}" "${revision}" >> "${metadata}"
    resource_dirs+=("${revision#"${CONFIG_FILE_DIR}/"}")
  done

  tar -cpzf "${BACKUP_TAR_FILE}" -C "${CONFIG_FILE_DIR}" "${resource_dirs[@]}" -C "${WORK_DIR}" PARTIAL_BACKUP.txt
  [ -s "${BACKUP_TAR_FILE}" ]
  chmod 600 "${BACKUP_TAR_FILE}"
}

function source_required_dependency {
  local src_path="$1"
  if [ ! -f "${src_path}" ]; then
    echo "required dependencies not found, please ensure this script is run on a node with a functional etcd static pod"
    exit 1
  fi
  # shellcheck disable=SC1090
  source "${src_path}"
}

DATESTRING=$(date "+%F_%H%M%S")
WORK_DIR=$(mktemp -d "${BACKUP_DIR}/.cluster-backup.XXXXXXXX")
FINAL_BACKUP_TAR_FILE="${BACKUP_DIR}/static_kuberesources_${DATESTRING}${IS_DIRTY}.tar.gz"
FINAL_SNAPSHOT_FILE="${BACKUP_DIR}/snapshot_${DATESTRING}${IS_DIRTY}.db"
if [ "${AVAILABLE_STATIC_RESOURCES}" == "true" ]; then
  echo 'WARNING: PARTIAL local backup requested. This is NOT a full control-plane recovery backup.' >&2
  FINAL_BACKUP_TAR_FILE="${BACKUP_DIR}/partial_static_kuberesources_${DATESTRING}${IS_DIRTY}_${WORK_DIR##*.}.tar.gz"
  FINAL_SNAPSHOT_FILE="${BACKUP_DIR}/partial_snapshot_${DATESTRING}${IS_DIRTY}_${WORK_DIR##*.}.db"
fi
BACKUP_TAR_FILE="${WORK_DIR}/resources.tar.gz"
SNAPSHOT_FILE="${WORK_DIR}/snapshot.db"
PUBLISHED_ARCHIVE=false
PUBLISHED_SNAPSHOT=false

function cleanup {
  local status=$?
  if [ "${status}" -ne 0 ]; then
    if [ "${PUBLISHED_ARCHIVE}" == "true" ]; then
      rm -f -- "${FINAL_BACKUP_TAR_FILE}"
    fi
    if [ "${PUBLISHED_SNAPSHOT}" == "true" ]; then
      rm -f -- "${FINAL_SNAPSHOT_FILE}"
    fi
  fi
  if [ -n "${WORK_DIR}" ]; then
    rm -rf -- "${WORK_DIR}"
  fi
}
trap cleanup EXIT

source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

# replacing the value of variables sourced form etcd.env to use the local node folders if the script is not running into the cluster-backup pod
if [ ! -f "${ETCDCTL_CACERT}" ]; then
  echo "Certificate ${ETCDCTL_CACERT} is missing. Checking in different directory"
  export ETCDCTL_CACERT=$(echo ${ETCDCTL_CACERT} | sed -e "s|static-pod-certs|static-pod-resources/etcd-certs|")
  export ETCDCTL_CERT=$(echo ${ETCDCTL_CERT} | sed -e "s|static-pod-certs|static-pod-resources/etcd-certs|")
  export ETCDCTL_KEY=$(echo ${ETCDCTL_KEY} | sed -e "s|static-pod-certs|static-pod-resources/etcd-certs|")
  if [ ! -f "${ETCDCTL_CACERT}" ]; then
    echo "Certificate ${ETCDCTL_CACERT} is also missing in the second directory. Exiting!"
    exit 1
  else
    echo "Certificate ${ETCDCTL_CACERT} found!"
  fi
fi

if [ "${AVAILABLE_STATIC_RESOURCES}" == "true" ]; then
  backup_available_kube_static_resources
else
  backup_latest_kube_static_resources "${BACKUP_TAR_FILE}"
fi

# Download etcdctl and get the etcd snapshot
dl_etcdctl

# snapshot save will continue to stay in etcdctl
ETCDCTL_ENDPOINTS="https://${NODE_NODE_ENVVAR_NAME_IP}:2379" etcdctl snapshot save "${SNAPSHOT_FILE}"
[ -s "${SNAPSHOT_FILE}" ]
chmod 600 "${SNAPSHOT_FILE}"

# Check the integrity of the snapshot
check_snapshot_status "${SNAPSHOT_FILE}"
snapshot_failed=$?

# If check_snapshot_status returned 1 it failed, so exit with code 1
if [[ $snapshot_failed -eq 1 ]]; then
  echo "snapshot failed with exit code ${snapshot_failed}"
  exit 1
fi

if [ "${AVAILABLE_STATIC_RESOURCES}" == "true" ]; then
  ln -T -- "${BACKUP_TAR_FILE}" "${FINAL_BACKUP_TAR_FILE}"
  PUBLISHED_ARCHIVE=true
  ln -T -- "${SNAPSHOT_FILE}" "${FINAL_SNAPSHOT_FILE}"
  PUBLISHED_SNAPSHOT=true
else
  mv -fT -- "${BACKUP_TAR_FILE}" "${FINAL_BACKUP_TAR_FILE}"
  PUBLISHED_ARCHIVE=true
  mv -fT -- "${SNAPSHOT_FILE}" "${FINAL_SNAPSHOT_FILE}"
  PUBLISHED_SNAPSHOT=true
fi
rm -rf -- "${WORK_DIR}"
WORK_DIR=""

if [ "${AVAILABLE_STATIC_RESOURCES}" == "true" ]; then
  echo "WARNING: PARTIAL snapshot and available local resources saved to ${BACKUP_DIR}; NOT qualified for full control-plane recovery."
  echo "${FINAL_SNAPSHOT_FILE}"
  echo "${FINAL_BACKUP_TAR_FILE}"
else
  echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}"
fi
