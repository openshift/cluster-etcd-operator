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
   local operator="$1"

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

# backup latest static pod resources
function backup_latest_kube_static_resources {
  local backup_resource_list=("kube-apiserver" "kube-controller-manager" "kube-scheduler" "etcd")
  local latest_resource_dirs=()
  local resource manifest latest_resource incomplete=""

  if [ ! -r "${MANIFEST_DIR}" ] || [ ! -x "${MANIFEST_DIR}" ] || \
     [ ! -r "${CONFIG_FILE_DIR}/static-pod-resources" ] || [ ! -x "${CONFIG_FILE_DIR}/static-pod-resources" ]; then
    echo "Required manifest/resource directories are missing or unreadable" >&2
    exit 1
  fi

  for resource in "${backup_resource_list[@]}"; do
    manifest="${MANIFEST_DIR}/${resource}-pod.yaml"
    if [ ! -e "${manifest}" ] && [ ! -L "${manifest}" ]; then
      if [ "${resource}" == "etcd" ]; then
        echo "Required etcd manifest is missing: ${manifest}" >&2
        exit 1
      fi
      echo "WARNING: omitting ${resource}: local Pod manifest is absent" >&2
      incomplete="partial_"
      continue
    fi
    if [ ! -f "${manifest}" ] || [ ! -r "${manifest}" ]; then
      echo "Invalid or unreadable Pod manifest: ${manifest}" >&2
      exit 1
    fi
    latest_resource=$(manifest_resource_revision "${manifest}" "${resource}")
    if [ ! -e "${latest_resource}" ] && [ ! -L "${latest_resource}" ] && [ "${resource}" != "etcd" ]; then
      echo "WARNING: omitting ${resource}: installed static resources are absent (${latest_resource})" >&2
      incomplete="partial_"
      continue
    fi
    if [ ! -d "${latest_resource}" ] || [ ! -r "${latest_resource}" ] || [ ! -x "${latest_resource}" ]; then
      echo "Installed static resources are missing or unreadable: ${latest_resource}" >&2
      exit 1
    fi
    if [ "${IS_DIRTY}" == "" ]; then
      check_if_operator_is_progressing "${resource}"
    fi

    echo "found latest ${resource}: ${latest_resource}"
    latest_resource_dirs+=("${latest_resource#"${CONFIG_FILE_DIR}/"}")
  done

  if [ -n "${incomplete}" ]; then
    BACKUP_TAR_FILE="${BACKUP_DIR}/partial_static_kuberesources_${DATESTRING}${IS_DIRTY}.tar.gz"
    SNAPSHOT_FILE="${BACKUP_DIR}/partial_snapshot_${DATESTRING}${IS_DIRTY}.db"
    echo "WARNING: partial local backup; NOT qualified for full control-plane restore. Do not rename or mix these artifacts with full backups." >&2
  fi

  # tar latest resources with the path relative to CONFIG_FILE_DIR
  tar -cpzf "${BACKUP_TAR_FILE}" -C "${CONFIG_FILE_DIR}" "${latest_resource_dirs[@]}"
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

BACKUP_DIR="$1"
DATESTRING=$(date "+%F_%H%M%S")
BACKUP_TAR_FILE=${BACKUP_DIR}/static_kuberesources_${DATESTRING}${IS_DIRTY}.tar.gz
SNAPSHOT_FILE="${BACKUP_DIR}/snapshot_${DATESTRING}${IS_DIRTY}.db"

trap 'rm -f -- "${BACKUP_TAR_FILE}" "${SNAPSHOT_FILE}"' ERR

source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

# replacing the value of variables sourced form etcd.env to use the local node folders if the script is not running into the cluster-backup pod
if [ ! -f "${ETCDCTL_CACERT}" ]; then
  echo "Certificate ${ETCDCTL_CACERT} is missing. Checking in different directory"
  export ETCDCTL_CACERT="${ETCDCTL_CACERT/static-pod-certs/static-pod-resources\/etcd-certs}"
  export ETCDCTL_CERT="${ETCDCTL_CERT/static-pod-certs/static-pod-resources\/etcd-certs}"
  export ETCDCTL_KEY="${ETCDCTL_KEY/static-pod-certs/static-pod-resources\/etcd-certs}"
  if [ ! -f "${ETCDCTL_CACERT}" ]; then
    echo "Certificate ${ETCDCTL_CACERT} is also missing in the second directory. Exiting!"
    exit 1
  else
    echo "Certificate ${ETCDCTL_CACERT} found!"
  fi
fi

for certificate in "${ETCDCTL_CACERT}" "${ETCDCTL_CERT}" "${ETCDCTL_KEY}"; do
  if [ ! -s "${certificate}" ] || [ ! -r "${certificate}" ]; then
    echo "Required etcd certificate/key is missing, empty or unreadable: ${certificate}" >&2
    exit 1
  fi
done

backup_latest_kube_static_resources

# Download etcdctl and get the etcd snapshot
dl_etcdctl

# snapshot save will continue to stay in etcdctl
ETCDCTL_ENDPOINTS="https://${NODE_NODE_ENVVAR_NAME_IP}:2379" etcdctl snapshot save "${SNAPSHOT_FILE}"

if [ ! -s "${SNAPSHOT_FILE}" ]; then
  echo "Snapshot is missing or empty" >&2
  false
fi

# Check the integrity of the snapshot
check_snapshot_status "${SNAPSHOT_FILE}"
snapshot_failed=$?

# If check_snapshot_status returned 1 it failed, so exit with code 1
if [[ $snapshot_failed -eq 1 ]]; then
  echo "snapshot failed with exit code ${snapshot_failed}"
  exit 1
fi

echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}"
