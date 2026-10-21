#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# ./quorum-restore.sh
# This script attempts to restore quorum by spawning a revision-bumped etcd without membership information.

function usage() {
  echo "Usage: $0 [--wait [SECONDS]]" >&2
  echo "--wait waits for local etcd readiness at https://localhost:9980/readyz, not full cluster recovery (default: 300 seconds)" >&2
}

function parse_arguments() {
  WAIT_FOR_ETCD=false
  WAIT_TIMEOUT=300
  while (( $# > 0 )); do
    if [[ "$1" != --wait ]]; then
      usage
      return 2
    fi
    WAIT_FOR_ETCD=true
    shift
    if (( $# > 0 )) && [[ "$1" != --wait ]]; then
      local value="$1"
      if [[ ! "${value}" =~ ^[0-9]+$ ]]; then
        usage
        return 2
      fi
      value="${value#"${value%%[!0]*}"}"
      if [[ -z "${value}" ]] || (( ${#value} > 10 )) || (( value > 2147483647 )); then
        echo "Wait seconds must be a positive decimal integer no greater than 2147483647" >&2
        usage
        return 2
      fi
      WAIT_TIMEOUT="${value}"
      shift
    fi
  done
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

function restore_quorum() {
  local ca_cert
  if "${WAIT_FOR_ETCD}"; then
    if ! command -v curl >/dev/null 2>&1; then
      echo "curl is required for --wait" >&2
      return 1
    fi
    ca_cert="${ETCDCTL_CACERT:-}"
    if ! test -f "${ca_cert}" || ! test -r "${ca_cert}"; then
      case "${ca_cert}" in
        /etc/kubernetes/static-pod-certs/*)
          ca_cert="/etc/kubernetes/static-pod-resources/etcd-certs/${ca_cert#/etc/kubernetes/static-pod-certs/}"
          ;;
      esac
    fi
    if ! test -f "${ca_cert}" || ! test -r "${ca_cert}"; then
      echo "A readable ETCDCTL_CACERT CA file is required for --wait: ${ca_cert}" >&2
      return 1
    fi
    if ! command -v timeout >/dev/null 2>&1 || ! timeout --signal=KILL 1s true >/dev/null 2>&1; then
      echo "GNU coreutils timeout with --signal=KILL is required for --wait" >&2
      return 1
    fi
  fi

  local static_pods=("etcd-pod.yaml")
  local containers=("etcd" "etcdctl" "etcd-metrics" "etcd-readyz" "etcd-rev")

  mv_static_pods "${static_pods[@]}"
  wait_for_containers_to_stop "${containers[@]}"

  echo "starting restore-etcd static pod"
  cp "${QUORUM_RESTORE_ETCD_POD_YAML}" "${MANIFEST_DIR}/etcd-pod.yaml"
  if "${WAIT_FOR_ETCD}"; then
    wait_for_restored_etcd_to_start "${WAIT_TIMEOUT}" "${ca_cert}"
  fi
}

function main() {
  parse_arguments "$@"
  if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root"
    exit 1
  fi
  source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
  source_required_dependency /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools
  restore_quorum
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
