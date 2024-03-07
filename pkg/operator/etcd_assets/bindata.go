// Code generated for package etcd_assets by go-bindata DO NOT EDIT. (@generated)
// sources:
// bindata/etcd/backups-cr.yaml
// bindata/etcd/backups-crb.yaml
// bindata/etcd/backups-sa.yaml
// bindata/etcd/cluster-backup-cronjob.yaml
// bindata/etcd/cluster-backup-job.yaml
// bindata/etcd/cluster-backup.sh
// bindata/etcd/cluster-restore.sh
// bindata/etcd/cm.yaml
// bindata/etcd/etcd-common-tools
// bindata/etcd/minimal-sm.yaml
// bindata/etcd/ns.yaml
// bindata/etcd/pod-cm.yaml
// bindata/etcd/pod.yaml
// bindata/etcd/prometheus-role.yaml
// bindata/etcd/prometheus-rolebinding.yaml
// bindata/etcd/restore-pod-cm.yaml
// bindata/etcd/restore-pod.yaml
// bindata/etcd/sa.yaml
// bindata/etcd/scripts-cm.yaml
// bindata/etcd/sm.yaml
// bindata/etcd/svc.yaml
package etcd_assets

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type asset struct {
	bytes []byte
	info  os.FileInfo
}

type bindataFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

// Name return file name
func (fi bindataFileInfo) Name() string {
	return fi.name
}

// Size return file size
func (fi bindataFileInfo) Size() int64 {
	return fi.size
}

// Mode return file mode
func (fi bindataFileInfo) Mode() os.FileMode {
	return fi.mode
}

// Mode return file modify time
func (fi bindataFileInfo) ModTime() time.Time {
	return fi.modTime
}

// IsDir return file whether a directory
func (fi bindataFileInfo) IsDir() bool {
	return fi.mode&os.ModeDir != 0
}

// Sys return file is sys mode
func (fi bindataFileInfo) Sys() interface{} {
	return nil
}

var _etcdBackupsCrYaml = []byte(`kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: system:openshift:operator:etcd-backup-role
rules:
  - apiGroups:
      - "operator.openshift.io"
    resources:
      - "etcdbackups"
      - "etcdbackups/status"
      - "etcdbackups/finalizers"
    verbs:
      - "create"
      - "update"
      - "delete"
  - apiGroups:
      - "batch"
    resources:
      - "jobs/finalizers"
    verbs:
      - "update"
`)

func etcdBackupsCrYamlBytes() ([]byte, error) {
	return _etcdBackupsCrYaml, nil
}

func etcdBackupsCrYaml() (*asset, error) {
	bytes, err := etcdBackupsCrYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/backups-cr.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdBackupsCrbYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: system:openshift:operator:etcd-backup-crb
  annotations:
    include.release.openshift.io/self-managed-high-availability: "true"
    include.release.openshift.io/single-node-developer: "true"
roleRef:
  kind: ClusterRole
  name: system:openshift:operator:etcd-backup-role
subjects:
  - kind: ServiceAccount
    namespace: openshift-etcd
    name: etcd-backup-sa
`)

func etcdBackupsCrbYamlBytes() ([]byte, error) {
	return _etcdBackupsCrbYaml, nil
}

func etcdBackupsCrbYaml() (*asset, error) {
	bytes, err := etcdBackupsCrbYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/backups-crb.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdBackupsSaYaml = []byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  namespace: openshift-etcd
  name: etcd-backup-sa
`)

func etcdBackupsSaYamlBytes() ([]byte, error) {
	return _etcdBackupsSaYaml, nil
}

func etcdBackupsSaYaml() (*asset, error) {
	bytes, err := etcdBackupsSaYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/backups-sa.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdClusterBackupCronjobYaml = []byte(`apiVersion: batch/v1
kind: CronJob
metadata:
  name: templated
  namespace: openshift-etcd
  labels:
    app: cluster-backup-cronjob
    backup-name: templated
spec:
  schedule: "templated"
  concurrencyPolicy: "Forbid"
  failedJobsHistoryLimit: 10
  successfulJobsHistoryLimit: 5
  jobTemplate:
    metadata:
      labels:
        app: cluster-backup-cronjob
    spec:
      template:
        metadata:
          labels:
            app: cluster-backup-cronjob
        spec:
          initContainers:
            - name: retention
              imagePullPolicy: IfNotPresent
              terminationMessagePolicy: FallbackToLogsOnError
              # since we can expect hostPath mounts, we need to run as privileged to access them
              securityContext:
                privileged: true
              command: [ "cluster-etcd-operator" ]
              args: [ "templated" ]
              volumeMounts:
                - mountPath: /etc/kubernetes/cluster-backup
                  name: etc-kubernetes-cluster-backup
          containers:
            - name: cluster-backup
              imagePullPolicy: IfNotPresent
              terminationMessagePolicy: FallbackToLogsOnError
              command: [ "cluster-etcd-operator" ]
              args: [ "templated" ]
              env:
                - name: MY_JOB_NAME
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.labels['batch.kubernetes.io/job-name']
                - name: MY_JOB_UID
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.labels['batch.kubernetes.io/controller-uid']
          serviceAccountName: etcd-backup-sa
          nodeSelector:
            node-role.kubernetes.io/master: ""
          tolerations:
            - operator: "Exists"
          restartPolicy: OnFailure
          volumes:
            - name: etc-kubernetes-cluster-backup
              persistentVolumeClaim:
                claimName: templated
`)

func etcdClusterBackupCronjobYamlBytes() ([]byte, error) {
	return _etcdClusterBackupCronjobYaml, nil
}

func etcdClusterBackupCronjobYaml() (*asset, error) {
	bytes, err := etcdClusterBackupCronjobYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/cluster-backup-cronjob.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdClusterBackupJobYaml = []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: cluster-backup-job
  namespace: openshift-etcd
  labels:
    app: cluster-backup-job
    backup-name: templated
spec:
  template:
    spec:
      initContainers:
        - name: verify-storage
          imagePullPolicy: IfNotPresent
          terminationMessagePolicy: FallbackToLogsOnError
          command: [ "cluster-etcd-operator", "verify", "backup-storage" ]
          securityContext:
            privileged: true
          resources:
            requests:
              memory: 50Mi
              cpu: 5m
          volumeMounts:
            - mountPath: /etc/kubernetes/cluster-backup
              name: etc-kubernetes-cluster-backup
            - mountPath: /var/run/secrets/etcd-client
              name: etcd-client
            - mountPath: /var/run/configmaps/etcd-ca
              name: etcd-ca
      containers:
        - name: cluster-backup
          imagePullPolicy: IfNotPresent
          terminationMessagePolicy: FallbackToLogsOnError
          command:
            - /bin/sh
            - -c
            - |
              #!/bin/sh
              set -exuo pipefail

              cluster-etcd-operator cluster-backup --backup-dir "${CLUSTER_BACKUP_PATH}"

          resources:
            requests:
              memory: 80Mi
              cpu: 10m
          securityContext:
            privileged: true
          volumeMounts:
            - mountPath: /usr/local/bin
              name: usr-local-bin
            - mountPath: /etc/kubernetes/static-pod-resources
              name: resources-dir
            - mountPath: /etc/kubernetes/static-pod-certs
              name: cert-dir
            - mountPath: /etc/kubernetes/manifests
              name: static-pod-dir
            - mountPath: /etc/kubernetes/cluster-backup
              name: etc-kubernetes-cluster-backup
            - mountPath: /var/run/secrets/etcd-client
              name: etcd-client
            - mountPath: /var/run/configmaps/etcd-ca
              name: etcd-ca
      priorityClassName: system-node-critical
      activeDeadlineSeconds: 900
      nodeSelector:
        node-role.kubernetes.io/master: ""
      restartPolicy: Never
      hostNetwork: true
      tolerations:
        - operator: "Exists"
      volumes:
        - hostPath:
            path: /usr/local/bin
          name: usr-local-bin
        - hostPath:
            path: /etc/kubernetes/manifests
          name: static-pod-dir
        - hostPath:
            path: /etc/kubernetes/static-pod-resources
          name: resources-dir
        - hostPath:
            path: /etc/kubernetes/static-pod-resources/etcd-certs
          name: cert-dir
        - name: etcd-client
          secret:
            secretName: etcd-client
        - name: etcd-ca
          configMap:
            name: etcd-ca-bundle
        - name: etc-kubernetes-cluster-backup
          persistentVolumeClaim:
            claimName: templated
`)

func etcdClusterBackupJobYamlBytes() ([]byte, error) {
	return _etcdClusterBackupJobYaml, nil
}

func etcdClusterBackupJobYaml() (*asset, error) {
	bytes, err := etcdClusterBackupJobYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/cluster-backup-job.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdClusterBackupSh = []byte(`#!/usr/bin/env bash

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
    latest_resource_dirs+=("${latest_resource#${CONFIG_FILE_DIR}/}")
  done

  # tar latest resources with the path relative to CONFIG_FILE_DIR
  tar -cpzf "$backup_tar_file" -C "${CONFIG_FILE_DIR}" "${latest_resource_dirs[@]}"
  chmod 600 "$backup_tar_file"
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

trap 'rm -f ${BACKUP_TAR_FILE} ${SNAPSHOT_FILE}' ERR

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

backup_latest_kube_static_resources "${BACKUP_TAR_FILE}"

# Download etcdctl and get the etcd snapshot
dl_etcdctl

# snapshot save will continue to stay in etcdctl
ETCDCTL_ENDPOINTS="https://${NODE_NODE_ENVVAR_NAME_IP}:2379" etcdctl snapshot save "${SNAPSHOT_FILE}"

# Check the integrity of the snapshot
check_snapshot_status "${SNAPSHOT_FILE}"
snapshot_failed=$?

# If check_snapshot_status returned 1 it failed, so exit with code 1
if [[ $snapshot_failed -eq 1 ]]; then
  echo "snapshot failed with exit code ${snapshot_failed}"
  exit 1
fi

echo "snapshot db and kube resources are successfully saved to ${BACKUP_DIR}"
`)

func etcdClusterBackupShBytes() ([]byte, error) {
	return _etcdClusterBackupSh, nil
}

func etcdClusterBackupSh() (*asset, error) {
	bytes, err := etcdClusterBackupShBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/cluster-backup.sh", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdClusterRestoreSh = []byte(`#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

# example
# ./cluster-restore.sh $path-to-backup
# There are several customization switches based on env variables:
# ETCD_RESTORE_SKIP_MOVE_CP_STATIC_PODS - when set this script will not move the other (non-etcd) static pod yamls
# ETCD_ETCDCTL_RESTORE - when set this script will use ` + "`" + `etcdctl snapshot restore` + "`" + ` instead of a restore pod yaml
# ETCD_ETCDCTL_RESTORE_ENABLE_BUMP - when set this script will spawn the restore pod with a large enough revision bump

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

function wait_for_containers_to_stop() {
  local containers=("$@")

  for container_name in "${containers[@]}"; do
    echo "Waiting for container ${container_name} to stop"
    while [[ -n $(crictl ps --label io.kubernetes.container.name="${container_name}" -q) ]]; do
      echo -n "."
      sleep 1
    done
    echo "complete"
  done
}

function mv_static_pods() {
  local containers=("$@")

  # Move manifests and stop static pods
  if [ ! -d "$MANIFEST_STOPPED_DIR" ]; then
    mkdir -p "$MANIFEST_STOPPED_DIR"
  fi

  for POD_FILE_NAME in "${containers[@]}"; do
    echo "...stopping ${POD_FILE_NAME}"
    [ ! -f "${MANIFEST_DIR}/${POD_FILE_NAME}" ] && continue
    mv "${MANIFEST_DIR}/${POD_FILE_NAME}" "${MANIFEST_STOPPED_DIR}"
  done
}

BACKUP_DIR="$1"
# shellcheck disable=SC2012
BACKUP_FILE=$(ls -vd "${BACKUP_DIR}"/static_kuberesources*.tar.gz | tail -1) || true
# shellcheck disable=SC2012
SNAPSHOT_FILE=$(ls -vd "${BACKUP_DIR}"/snapshot*.db | tail -1) || true

ETCD_STATIC_POD_LIST=("etcd-pod.yaml")
AUX_STATIC_POD_LIST=("kube-apiserver-pod.yaml" "kube-controller-manager-pod.yaml" "kube-scheduler-pod.yaml")

ETCD_STATIC_POD_CONTAINERS=("etcd" "etcdctl" "etcd-metrics" "etcd-readyz")
AUX_STATIC_POD_CONTAINERS=("kube-controller-manager" "kube-apiserver" "kube-scheduler")

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

# Move static pod manifests out of MANIFEST_DIR, if required
if [ -z "${ETCD_RESTORE_SKIP_MOVE_CP_STATIC_PODS}" ]; then
  mv_static_pods "${AUX_STATIC_POD_LIST[@]}"
  wait_for_containers_to_stop "${AUX_STATIC_POD_CONTAINERS[@]}"
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

  echo "starting restore-etcd static pod"
  # ideally this can be solved with jq and a real env var, but we don't have it available at this point
  if [ -n "${ETCD_ETCDCTL_RESTORE_ENABLE_BUMP}" ]; then
    sed "s/export ETCD_ETCDCTL_RESTORE_ENABLE_BUMP=\"false\"/export ETCD_ETCDCTL_RESTORE_ENABLE_BUMP=\"true\"/" "${RESTORE_ETCD_POD_YAML}" > "${MANIFEST_DIR}/etcd-pod.yaml"
  else
     cp -p "${RESTORE_ETCD_POD_YAML}" "${MANIFEST_DIR}/etcd-pod.yaml"
  fi

else
  echo "removing etcd data dir..."
  rm -rf "${ETCD_DATA_DIR}"
  mkdir -p "${ETCD_DATA_DIR}"

  echo "starting snapshot restore through etcdctl..."
  if ! ${ETCD_CLIENT} snapshot restore "${SNAPSHOT_FILE}" --data-dir="${ETCD_DATA_DIR}"; then
      echo "Snapshot restore failed. Aborting!"
      exit 1
  fi

  # start the original etcd static pod again through the new snapshot
  echo "restoring old etcd pod to start etcd again"
  mv "${MANIFEST_STOPPED_DIR}/etcd-pod.yaml" "${MANIFEST_DIR}/etcd-pod.yaml"
fi

# start remaining static pods
if [ -z "${ETCD_RESTORE_SKIP_MOVE_CP_STATIC_PODS}" ]; then
  restore_static_pods "${BACKUP_FILE}" "${AUX_STATIC_POD_LIST[@]}"
fi
`)

func etcdClusterRestoreShBytes() ([]byte, error) {
	return _etcdClusterRestoreSh, nil
}

func etcdClusterRestoreSh() (*asset, error) {
	bytes, err := etcdClusterRestoreShBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/cluster-restore.sh", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdCmYaml = []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: openshift-etcd
  name: config
data:
  config.yaml:
`)

func etcdCmYamlBytes() ([]byte, error) {
	return _etcdCmYaml, nil
}

func etcdCmYaml() (*asset, error) {
	bytes, err := etcdCmYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/cm.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdEtcdCommonTools = []byte(`# Common environment variables
ASSET_DIR="/home/core/assets"
CONFIG_FILE_DIR="/etc/kubernetes"
MANIFEST_DIR="${CONFIG_FILE_DIR}/manifests"
ETCD_DATA_DIR="/var/lib/etcd"
ETCD_DATA_DIR_BACKUP="/var/lib/etcd-backup"
MANIFEST_STOPPED_DIR="${ASSET_DIR}/manifests-stopped"
RESTORE_ETCD_POD_YAML="${CONFIG_FILE_DIR}/static-pod-resources/etcd-certs/configmaps/restore-etcd-pod/pod.yaml"
ETCDCTL_BIN_DIR="${CONFIG_FILE_DIR}/static-pod-resources/bin"
PATH=${PATH}:${ETCDCTL_BIN_DIR}
export KUBECONFIG="/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/localhost.kubeconfig"
export ETCD_ETCDCTL_BIN="etcdctl"

# download etcdctl from download release image
function dl_etcdctl {
  # Avoid caching the binary when podman exists, the etcd image is always available locally and we need a way to update etcdctl.
  # When we're running from an etcd image there's no podman and we can continue without a download.
  if ([ -n "$(command -v podman)" ]); then
     local etcdimg=${ETCD_IMAGE}
     local etcdctr=$(podman create --authfile=/var/lib/kubelet/config.json ${etcdimg})
     local etcdmnt=$(podman mount "${etcdctr}")
     [ ! -d ${ETCDCTL_BIN_DIR} ] && mkdir -p ${ETCDCTL_BIN_DIR}
     cp ${etcdmnt}/bin/etcdctl ${ETCDCTL_BIN_DIR}/
     if [ -f "${etcdmnt}/bin/etcdutl" ]; then
       cp ${etcdmnt}/bin/etcdutl ${ETCDCTL_BIN_DIR}/
       export ETCD_ETCDUTL_BIN=etcdutl
     fi

     umount "${etcdmnt}"
     podman rm "${etcdctr}"
     etcdctl version
     return
  fi

  if ([ -x "$(command -v etcdctl)" ]); then
    echo "etcdctl is already installed"
    if [ -x "$(command -v etcdutl)" ]; then
      echo "etcdutl is already installed"
      export ETCD_ETCDUTL_BIN=etcdutl
    fi

    return
  fi

  echo "Could neither pull etcdctl nor find it locally in cache. Aborting!"
  exit 1
}

function check_snapshot_status() {
  local snap_file="$1"

  ETCD_CLIENT="${ETCD_ETCDCTL_BIN}"
  if [ -n "${ETCD_ETCDUTL_BIN}" ]; then
    ETCD_CLIENT="${ETCD_ETCDUTL_BIN}"
  fi

  if ! ${ETCD_CLIENT} snapshot status "${snap_file}" -w json; then
    echo "Backup integrity verification failed. Backup appears corrupted. Aborting!"
    return 1
  fi
}

`)

func etcdEtcdCommonToolsBytes() ([]byte, error) {
	return _etcdEtcdCommonTools, nil
}

func etcdEtcdCommonTools() (*asset, error) {
	bytes, err := etcdEtcdCommonToolsBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/etcd-common-tools", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdMinimalSmYaml = []byte(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: etcd-minimal
  namespace: openshift-etcd-operator
  labels:
    app.kubernetes.io/name: etcd
    k8s-app: etcd
    monitoring.openshift.io/collection-profile: minimal
spec:
  endpoints:
    - interval: 30s
      metricRelabelings:
      - action: keep
        regex: (etcd_disk_backend_commit_duration_seconds_bucket|etcd_disk_wal_fsync_duration_seconds_bucket|etcd_mvcc_db_total_size_in_bytes|etcd_mvcc_db_total_size_in_use_in_bytes|etcd_network_peer_round_trip_time_seconds_bucket|etcd_network_peer_sent_failures_total|etcd_server_has_leader|etcd_server_is_leader|etcd_server_proposals_failed_total|etcd_server_quota_backend_bytes|grpc_server_handled_total|grpc_server_handling_seconds_bucket|grpc_server_started_total|process_start_time_seconds)
        sourceLabels:
        - __name__
      port: etcd-metrics
      scheme: https
      tlsConfig:
        ca:
          configMap:
            name: etcd-metric-serving-ca
            key: ca-bundle.crt
        cert:
          secret:
            name: etcd-metric-client
            key: tls.crt
        keySecret:
          name: etcd-metric-client
          key: tls.key
  jobLabel: k8s-app
  namespaceSelector:
    matchNames:
    - openshift-etcd
  selector:
    matchLabels:
      k8s-app: etcd
`)

func etcdMinimalSmYamlBytes() ([]byte, error) {
	return _etcdMinimalSmYaml, nil
}

func etcdMinimalSmYaml() (*asset, error) {
	bytes, err := etcdMinimalSmYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/minimal-sm.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdNsYaml = []byte(`apiVersion: v1
kind: Namespace
metadata:
  annotations:
    openshift.io/node-selector: ""
    workload.openshift.io/allowed: "management"
  name: openshift-etcd
  labels:
    openshift.io/run-level: "0"
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
`)

func etcdNsYamlBytes() ([]byte, error) {
	return _etcdNsYaml, nil
}

func etcdNsYaml() (*asset, error) {
	bytes, err := etcdNsYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/ns.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdPodCmYaml = []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: openshift-etcd
  name: etcd-pod
data:
  pod.yaml:
  forceRedeploymentReason:
  version:
`)

func etcdPodCmYamlBytes() ([]byte, error) {
	return _etcdPodCmYaml, nil
}

func etcdPodCmYaml() (*asset, error) {
	bytes, err := etcdPodCmYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/pod-cm.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdPodYaml = []byte(`apiVersion: v1
kind: Pod
metadata:
  name: etcd
  namespace: openshift-etcd
  annotations:
    kubectl.kubernetes.io/default-container: etcd
    target.workload.openshift.io/management: '{"effect": "PreferredDuringScheduling"}'
  labels:
    app: etcd
    k8s-app: etcd
    etcd: "true"
    revision: "REVISION"
spec:
  initContainers:
    - name: setup
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      terminationMessagePolicy: FallbackToLogsOnError
      command:
        - /bin/sh
        - -c
        - |
          #!/bin/sh
          echo -n "Fixing etcd log permissions."
          chmod 0700 /var/log/etcd && touch /var/log/etcd/etcd-health-probe.log && chmod 0600 /var/log/etcd/*
      securityContext:
        privileged: true
      resources:
        requests:
          memory: 50Mi
          cpu: 5m
      volumeMounts:
        - mountPath: /var/log/etcd
          name: log-dir
    - name: etcd-ensure-env-vars
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      terminationMessagePolicy: FallbackToLogsOnError
      command:
        - /bin/sh
        - -c
        - |
          #!/bin/sh
          set -euo pipefail

          : "${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST?not set}"
          : "${NODE_NODE_ENVVAR_NAME_ETCD_NAME?not set}"
          : "${NODE_NODE_ENVVAR_NAME_IP?not set}"

          # check for ipv4 addresses as well as ipv6 addresses with extra square brackets
          if [[ "${NODE_NODE_ENVVAR_NAME_IP}" != "${NODE_IP}" && "${NODE_NODE_ENVVAR_NAME_IP}" != "[${NODE_IP}]" ]]; then
            # echo the error message to stderr
            echo "Expected node IP to be ${NODE_IP} got ${NODE_NODE_ENVVAR_NAME_IP}" >&2
            exit 1
          fi

          # check for ipv4 addresses as well as ipv6 addresses with extra square brackets
          if [[ "${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}" != "${NODE_IP}" && "${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}" != "[${NODE_IP}]" ]]; then
            # echo the error message to stderr
            echo "Expected etcd url host to be ${NODE_IP} got ${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}" >&2
            exit 1
          fi

      resources:
        requests:
          memory: 60Mi
          cpu: 10m
      securityContext:
        privileged: true
      env:
${COMPUTED_ENV_VARS}
      - name: NODE_IP
        valueFrom:
          fieldRef:
            fieldPath: status.podIP
    - name: etcd-resources-copy
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      terminationMessagePolicy: FallbackToLogsOnError
      command:
        - /bin/sh
        - -c
        - |
          #!/bin/sh
          set -euo pipefail

          rm -f $(grep -l '^### Created by cluster-etcd-operator' /usr/local/bin/*)
          cp -p /etc/kubernetes/static-pod-certs/configmaps/etcd-scripts/*.sh /usr/local/bin

      resources:
        requests:
          memory: 60Mi
          cpu: 10m
      securityContext:
        privileged: true
      volumeMounts:
        - mountPath: /etc/kubernetes/static-pod-resources
          name: resource-dir
        - mountPath: /etc/kubernetes/static-pod-certs
          name: cert-dir
        - mountPath: /usr/local/bin
          name: usr-local-bin
  containers:
  # The etcdctl container should always be first. It is intended to be used
  # to open a remote shell via ` + "`" + `oc rsh` + "`" + ` that is ready to run ` + "`" + `etcdctl` + "`" + `.
  - name: etcdctl
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    terminationMessagePolicy: FallbackToLogsOnError
    command:
      - "/bin/bash"
      - "-c"
      - "trap TERM INT; sleep infinity & wait"
    resources:
      requests:
        memory: 60Mi
        cpu: 10m
    volumeMounts:
      - mountPath: /etc/kubernetes/manifests
        name: static-pod-dir
      - mountPath: /etc/kubernetes/static-pod-resources
        name: resource-dir
      - mountPath: /etc/kubernetes/static-pod-certs
        name: cert-dir
      - mountPath: /var/lib/etcd/
        name: data-dir
    env:
${COMPUTED_ENV_VARS}
      - name: "ETCD_STATIC_POD_VERSION"
        value: "REVISION"
  - name: etcd
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    terminationMessagePolicy: FallbackToLogsOnError
    command:
      - /bin/sh
      - -c
      - |
        #!/bin/sh
        set -euo pipefail

        etcdctl member list || true

        # this has a non-zero return code if the command is non-zero.  If you use an export first, it doesn't and you
        # will succeed when you should fail.
        ETCD_INITIAL_CLUSTER=$(discover-etcd-initial-cluster \
          --cacert=/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt \
          --cert=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt \
          --key=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key \
          --endpoints=${ALL_ETCD_ENDPOINTS} \
          --data-dir=/var/lib/etcd \
          --target-peer-url-host=${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST} \
          --target-name=NODE_NAME)
        export ETCD_INITIAL_CLUSTER

        # we cannot use the "normal" port conflict initcontainer because when we upgrade, the existing static pod will never yield,
        # so we do the detection in etcd container itself.
        echo -n "Waiting for ports 2379, 2380 and 9978 to be released."
        time while [ -n "$(ss -Htan '( sport = 2379 or sport = 2380 or sport = 9978 )')" ]; do
          echo -n "."
          sleep 1
        done

        export ETCD_NAME=${NODE_NODE_ENVVAR_NAME_ETCD_NAME}
        env | grep ETCD | grep -v NODE

        set -x
        # See https://etcd.io/docs/v3.4.0/tuning/ for why we use ionice
        exec nice -n -19 ionice -c2 -n0 etcd \
          --logger=zap \
          --log-level=${VERBOSITY} \
          --experimental-initial-corrupt-check=true \
          --snapshot-count=10000 \
          --initial-advertise-peer-urls=https://${NODE_NODE_ENVVAR_NAME_IP}:2380 \
          --cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-NODE_NAME.crt \
          --key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-NODE_NAME.key \
          --trusted-ca-file=/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt \
          --client-cert-auth=true \
          --peer-cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt \
          --peer-key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key \
          --peer-trusted-ca-file=/etc/kubernetes/static-pod-certs/configmaps/etcd-peer-client-ca/ca-bundle.crt \
          --peer-client-cert-auth=true \
          --advertise-client-urls=https://${NODE_NODE_ENVVAR_NAME_IP}:2379 \
          --listen-client-urls=https://${LISTEN_ON_ALL_IPS}:2379,unixs://${NODE_NODE_ENVVAR_NAME_IP}:0 \
          --listen-peer-urls=https://${LISTEN_ON_ALL_IPS}:2380 \
          --metrics=extensive \
          --listen-metrics-urls=https://${LISTEN_ON_ALL_IPS}:9978 ||  mv /etc/kubernetes/etcd-backup-dir/etcd-member.yaml /etc/kubernetes/manifests
    env:
${COMPUTED_ENV_VARS}
      - name: "ETCD_STATIC_POD_VERSION"
        value: "REVISION"
    resources:
      requests:
        memory: 600Mi
        cpu: 300m
    readinessProbe:
      httpGet:
        port: 9980
        path: readyz
        scheme: HTTPS
      timeoutSeconds: 30
      failureThreshold: 5
      periodSeconds: 5
      successThreshold: 1
    livenessProbe:
      httpGet:
        path: healthz
        port: 9980
        scheme: HTTPS
      timeoutSeconds: 30
      periodSeconds: 5
      successThreshold: 1
      failureThreshold: 5
    startupProbe:
      httpGet:
        port: 9980
        path: readyz
        scheme: HTTPS
      initialDelaySeconds: 10
      timeoutSeconds: 1
      periodSeconds: 10
      successThreshold: 1
      failureThreshold: 18
    securityContext:
      privileged: true
    volumeMounts:
      - mountPath: /etc/kubernetes/manifests
        name: static-pod-dir
      - mountPath: /etc/kubernetes/static-pod-resources
        name: resource-dir
      - mountPath: /etc/kubernetes/static-pod-certs
        name: cert-dir
      - mountPath: /var/lib/etcd/
        name: data-dir
  - name: etcd-metrics
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    terminationMessagePolicy: FallbackToLogsOnError
    command:
      - /bin/sh
      - -c
      - |
        #!/bin/sh
        set -euo pipefail

        export ETCD_NAME=${NODE_NODE_ENVVAR_NAME_ETCD_NAME}

        exec nice -n -18 etcd grpc-proxy start \
          --endpoints https://${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}:9978 \
          --metrics-addr https://${LISTEN_ON_ALL_IPS}:9979 \
          --listen-addr ${LOCALHOST_IP}:9977 \
          --advertise-client-url ""  \
          --key /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key \
          --key-file /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-metrics-NODE_NAME.key \
          --cert /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt \
          --cert-file /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-metrics-NODE_NAME.crt \
          --cacert /etc/kubernetes/static-pod-certs/configmaps/etcd-peer-client-ca/ca-bundle.crt \
          --trusted-ca-file /etc/kubernetes/static-pod-certs/configmaps/etcd-metrics-proxy-serving-ca/ca-bundle.crt \
          --listen-cipher-suites TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256
    env:
${COMPUTED_ENV_VARS}
      - name: "ETCD_STATIC_POD_VERSION"
        value: "REVISION"
    resources:
      requests:
        memory: 200Mi
        cpu: 40m
    securityContext:
      privileged: true
    volumeMounts:
      - mountPath: /etc/kubernetes/static-pod-resources
        name: resource-dir
      - mountPath: /etc/kubernetes/static-pod-certs
        name: cert-dir
      - mountPath: /var/lib/etcd/
        name: data-dir
  - name: etcd-readyz
    image: ${OPERATOR_IMAGE}
    imagePullPolicy: IfNotPresent
    terminationMessagePolicy: FallbackToLogsOnError
    command:
      - /bin/sh
      - -c
      - |
        #!/bin/sh
        set -euo pipefail
        
        exec nice -n -18 cluster-etcd-operator readyz \
          --target=https://localhost:2379 \
          --listen-port=9980 \
          --serving-cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-NODE_NAME.crt \
          --serving-key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-NODE_NAME.key \
          --client-cert-file=$(ETCDCTL_CERT) \
          --client-key-file=$(ETCDCTL_KEY) \
          --client-cacert-file=$(ETCDCTL_CACERT)
    securityContext:
      privileged: true
    ports:
    - containerPort: 9980
      name: readyz
      protocol: TCP
    resources:
      requests:
        memory: 50Mi
        cpu: 10m
    env:
${COMPUTED_ENV_VARS}
    volumeMounts:
      - mountPath: /var/log/etcd/
        name: log-dir
      - mountPath: /etc/kubernetes/static-pod-certs
        name: cert-dir
  hostNetwork: true
  priorityClassName: system-node-critical
  tolerations:
  - operator: "Exists"
  volumes:
    - hostPath:
        path: /etc/kubernetes/manifests
      name: static-pod-dir
    - hostPath:
        path: /etc/kubernetes/static-pod-resources/etcd-pod-REVISION
      name: resource-dir
    - hostPath:
        path: /etc/kubernetes/static-pod-resources/etcd-certs
      name: cert-dir
    - hostPath:
        path: /var/lib/etcd
        type: ""
      name: data-dir
    - hostPath:
        path: /usr/local/bin
      name: usr-local-bin
    - hostPath:
        path: /var/log/etcd
      name: log-dir
`)

func etcdPodYamlBytes() ([]byte, error) {
	return _etcdPodYaml, nil
}

func etcdPodYaml() (*asset, error) {
	bytes, err := etcdPodYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/pod.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdPrometheusRoleYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: prometheus-k8s
  namespace: openshift-etcd
rules:
- apiGroups:
  - ""
  resources:
  - services
  - endpoints
  - pods
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - extensions
  resources:
  - ingresses
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - networking.k8s.io
  resources:
  - ingresses
  verbs:
  - get
  - list
  - watch
`)

func etcdPrometheusRoleYamlBytes() ([]byte, error) {
	return _etcdPrometheusRoleYaml, nil
}

func etcdPrometheusRoleYaml() (*asset, error) {
	bytes, err := etcdPrometheusRoleYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/prometheus-role.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdPrometheusRolebindingYaml = []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: prometheus-k8s
  namespace: openshift-etcd
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: prometheus-k8s
subjects:
- kind: ServiceAccount
  name: prometheus-k8s
  namespace: openshift-monitoring
`)

func etcdPrometheusRolebindingYamlBytes() ([]byte, error) {
	return _etcdPrometheusRolebindingYaml, nil
}

func etcdPrometheusRolebindingYaml() (*asset, error) {
	bytes, err := etcdPrometheusRolebindingYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/prometheus-rolebinding.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdRestorePodCmYaml = []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: openshift-etcd
  name: restore-etcd-pod
data:
  pod.yaml:
`)

func etcdRestorePodCmYamlBytes() ([]byte, error) {
	return _etcdRestorePodCmYaml, nil
}

func etcdRestorePodCmYaml() (*asset, error) {
	bytes, err := etcdRestorePodCmYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/restore-pod-cm.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdRestorePodYaml = []byte(`apiVersion: v1
kind: Pod
metadata:
  name: etcd
  namespace: openshift-etcd
  labels:
    app: etcd
    k8s-app: etcd
    etcd: "true"
    revision: "REVISION"
spec:
  containers:
  - name: etcd
    image: ${IMAGE}
    imagePullPolicy: IfNotPresent
    terminationMessagePolicy: FallbackToLogsOnError
    command:
      - /bin/sh
      - -c
      - |
        #!/bin/sh
        set -euo pipefail
        
        # this can be controlled by cluster-restore.sh, which will replace this line entirely when enabled at runtime
        export ETCD_ETCDCTL_RESTORE_ENABLE_BUMP="false"
        
        export ETCD_NAME=${NODE_NODE_ENVVAR_NAME_ETCD_NAME}
        export ETCD_INITIAL_CLUSTER="${ETCD_NAME}=https://${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}:2380"
        env | grep ETCD | grep -v NODE
        export ETCD_NODE_PEER_URL=https://${NODE_NODE_ENVVAR_NAME_ETCD_URL_HOST}:2380

        # checking if there are any fio perf file left behind that could be deleted without problems
        if [ ! -z $(ls -A "/var/lib/etcd/etcd_perf*") ]; then
          rm -f /var/lib/etcd/etcd_perf*
        fi

        # checking if data directory is empty, if not etcdctl restore will fail
        if [ ! -z $(ls -A "/var/lib/etcd") ]; then
          echo "please delete the contents of data directory before restoring, running the restore script will do this for you"
          exit 1
        fi
        
        ETCD_ETCDCTL_BIN="etcdctl"
        if [ -x "$(command -v etcdutl)" ]; then
          echo "found newer etcdutl, using that instead of etcdctl"
          ETCD_ETCDCTL_BIN="etcdutl"
        fi      

        # check if we have backup file to be restored
        # if the file exist, check if it has not changed size in last 5 seconds
        if [ ! -f /var/lib/etcd-backup/snapshot.db ]; then
          echo "please make a copy of the snapshot db file, then move that copy to /var/lib/etcd-backup/snapshot.db"
          exit 1
        else
          filesize=$(stat --format=%s "/var/lib/etcd-backup/snapshot.db")
          sleep 5
          newfilesize=$(stat --format=%s "/var/lib/etcd-backup/snapshot.db")
          if [ "$filesize" != "$newfilesize" ]; then
            echo "file size has changed since last 5 seconds, retry sometime after copying is complete"
            exit 1
          fi
        fi
        
        BUMP_ARGS=""
        if [[ "${ETCD_ETCDCTL_RESTORE_ENABLE_BUMP}" == "true" ]]; then
          echo "enabling restore bump"
          BUMP_ARGS="--bump-revision 1000000000 --mark-compacted"
        fi
                
        UUID=$(uuidgen)
        echo "restoring to a single node cluster"
        ${ETCD_ETCDCTL_BIN} snapshot restore /var/lib/etcd-backup/snapshot.db \
         --name  $ETCD_NAME \
         --initial-cluster=$ETCD_INITIAL_CLUSTER \
         --initial-cluster-token "openshift-etcd-${UUID}" \
         --initial-advertise-peer-urls $ETCD_NODE_PEER_URL \
         --data-dir="/var/lib/etcd/restore-${UUID}" \
         ${BUMP_ARGS}

        mv /var/lib/etcd/restore-${UUID}/* /var/lib/etcd/

        rmdir /var/lib/etcd/restore-${UUID}
        rm /var/lib/etcd-backup/snapshot.db

        set -x
        exec etcd \
          --logger=zap \
          --log-level=${VERBOSITY} \
          --initial-advertise-peer-urls=https://${NODE_NODE_ENVVAR_NAME_IP}:2380 \
          --cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-NODE_NAME.crt \
          --key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-NODE_NAME.key \
          --trusted-ca-file=/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt \
          --client-cert-auth=true \
          --peer-cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.crt \
          --peer-key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-NODE_NAME.key \
          --peer-trusted-ca-file=/etc/kubernetes/static-pod-certs/configmaps/etcd-peer-client-ca/ca-bundle.crt \
          --peer-client-cert-auth=true \
          --advertise-client-urls=https://${NODE_NODE_ENVVAR_NAME_IP}:2379 \
          --listen-client-urls=https://${LISTEN_ON_ALL_IPS}:2379 \
          --listen-peer-urls=https://${LISTEN_ON_ALL_IPS}:2380 \
          --metrics=extensive \
          --listen-metrics-urls=https://${LISTEN_ON_ALL_IPS}:9978
    env:
${COMPUTED_ENV_VARS}
      - name: "ETCD_STATIC_POD_REV"
        value: "REVISION"
    resources:
      requests:
        memory: 600Mi
        cpu: 300m
    readinessProbe:
      tcpSocket:
        port: 2380
      failureThreshold: 3
      initialDelaySeconds: 3
      periodSeconds: 5
      successThreshold: 1
      timeoutSeconds: 5
    securityContext:
      privileged: true
    volumeMounts:
      - mountPath: /etc/kubernetes/manifests
        name: static-pod-dir
      - mountPath: /etc/kubernetes/static-pod-certs
        name: cert-dir
      - mountPath: /var/lib/etcd/
        name: data-dir
      - mountPath: /var/lib/etcd-backup/
        name: backup-dir
  hostNetwork: true
  priorityClassName: system-node-critical
  tolerations:
  - operator: "Exists"
  volumes:
    - hostPath:
        path: /etc/kubernetes/manifests
      name: static-pod-dir
    - hostPath:
        path: /etc/kubernetes/static-pod-resources/etcd-certs
      name: cert-dir
    - hostPath:
        path: /var/lib/etcd
        type: ""
      name: data-dir
    - hostPath:
        path: /var/lib/etcd-backup
        type: ""
      name: backup-dir
`)

func etcdRestorePodYamlBytes() ([]byte, error) {
	return _etcdRestorePodYaml, nil
}

func etcdRestorePodYaml() (*asset, error) {
	bytes, err := etcdRestorePodYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/restore-pod.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdSaYaml = []byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  namespace: openshift-etcd
  name: etcd-sa
`)

func etcdSaYamlBytes() ([]byte, error) {
	return _etcdSaYaml, nil
}

func etcdSaYaml() (*asset, error) {
	bytes, err := etcdSaYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/sa.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdScriptsCmYaml = []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: openshift-etcd
  name: etcd-scripts
data:
  etcd.env:
  cluster-restore.sh:
  cluster-backup.sh:
  etcd-common-tools:
`)

func etcdScriptsCmYamlBytes() ([]byte, error) {
	return _etcdScriptsCmYaml, nil
}

func etcdScriptsCmYaml() (*asset, error) {
	bytes, err := etcdScriptsCmYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/scripts-cm.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdSmYaml = []byte(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: etcd
  namespace: openshift-etcd-operator
  labels:
    app.kubernetes.io/name: etcd
    k8s-app: etcd
    monitoring.openshift.io/collection-profile: full
spec:
  endpoints:
    - interval: 30s
      port: etcd-metrics
      scheme: https
      tlsConfig:
        ca:
          configMap:
            name: etcd-metric-serving-ca
            key: ca-bundle.crt
        cert:
          secret:
            name: etcd-metric-client
            key: tls.crt
        keySecret:
          name: etcd-metric-client
          key: tls.key
  jobLabel: k8s-app
  namespaceSelector:
    matchNames:
    - openshift-etcd
  selector:
    matchLabels:
      k8s-app: etcd
`)

func etcdSmYamlBytes() ([]byte, error) {
	return _etcdSmYaml, nil
}

func etcdSmYaml() (*asset, error) {
	bytes, err := etcdSmYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/sm.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

var _etcdSvcYaml = []byte(`apiVersion: v1
kind: Service
metadata:
  namespace: openshift-etcd
  name: etcd
  annotations:
    service.alpha.openshift.io/serving-cert-secret-name: serving-cert
    prometheus.io/scrape: "true"
    prometheus.io/scheme: https
  labels:
    # this label is used to indicate that it should be scraped by prometheus
    k8s-app: etcd
spec:
  selector:
    etcd: "true"
  ports:
    - name: etcd
      port: 2379
      protocol: TCP
    - name: etcd-metrics
      port: 9979
      protocol: TCP
`)

func etcdSvcYamlBytes() ([]byte, error) {
	return _etcdSvcYaml, nil
}

func etcdSvcYaml() (*asset, error) {
	bytes, err := etcdSvcYamlBytes()
	if err != nil {
		return nil, err
	}

	info := bindataFileInfo{name: "etcd/svc.yaml", size: 0, mode: os.FileMode(0), modTime: time.Unix(0, 0)}
	a := &asset{bytes: bytes, info: info}
	return a, nil
}

// Asset loads and returns the asset for the given name.
// It returns an error if the asset could not be found or
// could not be loaded.
func Asset(name string) ([]byte, error) {
	cannonicalName := strings.Replace(name, "\\", "/", -1)
	if f, ok := _bindata[cannonicalName]; ok {
		a, err := f()
		if err != nil {
			return nil, fmt.Errorf("Asset %s can't read by error: %v", name, err)
		}
		return a.bytes, nil
	}
	return nil, fmt.Errorf("Asset %s not found", name)
}

// MustAsset is like Asset but panics when Asset would return an error.
// It simplifies safe initialization of global variables.
func MustAsset(name string) []byte {
	a, err := Asset(name)
	if err != nil {
		panic("asset: Asset(" + name + "): " + err.Error())
	}

	return a
}

// AssetInfo loads and returns the asset info for the given name.
// It returns an error if the asset could not be found or
// could not be loaded.
func AssetInfo(name string) (os.FileInfo, error) {
	cannonicalName := strings.Replace(name, "\\", "/", -1)
	if f, ok := _bindata[cannonicalName]; ok {
		a, err := f()
		if err != nil {
			return nil, fmt.Errorf("AssetInfo %s can't read by error: %v", name, err)
		}
		return a.info, nil
	}
	return nil, fmt.Errorf("AssetInfo %s not found", name)
}

// AssetNames returns the names of the assets.
func AssetNames() []string {
	names := make([]string, 0, len(_bindata))
	for name := range _bindata {
		names = append(names, name)
	}
	return names
}

// _bindata is a table, holding each asset generator, mapped to its name.
var _bindata = map[string]func() (*asset, error){
	"etcd/backups-cr.yaml":             etcdBackupsCrYaml,
	"etcd/backups-crb.yaml":            etcdBackupsCrbYaml,
	"etcd/backups-sa.yaml":             etcdBackupsSaYaml,
	"etcd/cluster-backup-cronjob.yaml": etcdClusterBackupCronjobYaml,
	"etcd/cluster-backup-job.yaml":     etcdClusterBackupJobYaml,
	"etcd/cluster-backup.sh":           etcdClusterBackupSh,
	"etcd/cluster-restore.sh":          etcdClusterRestoreSh,
	"etcd/cm.yaml":                     etcdCmYaml,
	"etcd/etcd-common-tools":           etcdEtcdCommonTools,
	"etcd/minimal-sm.yaml":             etcdMinimalSmYaml,
	"etcd/ns.yaml":                     etcdNsYaml,
	"etcd/pod-cm.yaml":                 etcdPodCmYaml,
	"etcd/pod.yaml":                    etcdPodYaml,
	"etcd/prometheus-role.yaml":        etcdPrometheusRoleYaml,
	"etcd/prometheus-rolebinding.yaml": etcdPrometheusRolebindingYaml,
	"etcd/restore-pod-cm.yaml":         etcdRestorePodCmYaml,
	"etcd/restore-pod.yaml":            etcdRestorePodYaml,
	"etcd/sa.yaml":                     etcdSaYaml,
	"etcd/scripts-cm.yaml":             etcdScriptsCmYaml,
	"etcd/sm.yaml":                     etcdSmYaml,
	"etcd/svc.yaml":                    etcdSvcYaml,
}

// AssetDir returns the file names below a certain
// directory embedded in the file by go-bindata.
// For example if you run go-bindata on data/... and data contains the
// following hierarchy:
//
//	data/
//	  foo.txt
//	  img/
//	    a.png
//	    b.png
//
// then AssetDir("data") would return []string{"foo.txt", "img"}
// AssetDir("data/img") would return []string{"a.png", "b.png"}
// AssetDir("foo.txt") and AssetDir("notexist") would return an error
// AssetDir("") will return []string{"data"}.
func AssetDir(name string) ([]string, error) {
	node := _bintree
	if len(name) != 0 {
		cannonicalName := strings.Replace(name, "\\", "/", -1)
		pathList := strings.Split(cannonicalName, "/")
		for _, p := range pathList {
			node = node.Children[p]
			if node == nil {
				return nil, fmt.Errorf("Asset %s not found", name)
			}
		}
	}
	if node.Func != nil {
		return nil, fmt.Errorf("Asset %s not found", name)
	}
	rv := make([]string, 0, len(node.Children))
	for childName := range node.Children {
		rv = append(rv, childName)
	}
	return rv, nil
}

type bintree struct {
	Func     func() (*asset, error)
	Children map[string]*bintree
}

var _bintree = &bintree{nil, map[string]*bintree{
	"etcd": {nil, map[string]*bintree{
		"backups-cr.yaml":             {etcdBackupsCrYaml, map[string]*bintree{}},
		"backups-crb.yaml":            {etcdBackupsCrbYaml, map[string]*bintree{}},
		"backups-sa.yaml":             {etcdBackupsSaYaml, map[string]*bintree{}},
		"cluster-backup-cronjob.yaml": {etcdClusterBackupCronjobYaml, map[string]*bintree{}},
		"cluster-backup-job.yaml":     {etcdClusterBackupJobYaml, map[string]*bintree{}},
		"cluster-backup.sh":           {etcdClusterBackupSh, map[string]*bintree{}},
		"cluster-restore.sh":          {etcdClusterRestoreSh, map[string]*bintree{}},
		"cm.yaml":                     {etcdCmYaml, map[string]*bintree{}},
		"etcd-common-tools":           {etcdEtcdCommonTools, map[string]*bintree{}},
		"minimal-sm.yaml":             {etcdMinimalSmYaml, map[string]*bintree{}},
		"ns.yaml":                     {etcdNsYaml, map[string]*bintree{}},
		"pod-cm.yaml":                 {etcdPodCmYaml, map[string]*bintree{}},
		"pod.yaml":                    {etcdPodYaml, map[string]*bintree{}},
		"prometheus-role.yaml":        {etcdPrometheusRoleYaml, map[string]*bintree{}},
		"prometheus-rolebinding.yaml": {etcdPrometheusRolebindingYaml, map[string]*bintree{}},
		"restore-pod-cm.yaml":         {etcdRestorePodCmYaml, map[string]*bintree{}},
		"restore-pod.yaml":            {etcdRestorePodYaml, map[string]*bintree{}},
		"sa.yaml":                     {etcdSaYaml, map[string]*bintree{}},
		"scripts-cm.yaml":             {etcdScriptsCmYaml, map[string]*bintree{}},
		"sm.yaml":                     {etcdSmYaml, map[string]*bintree{}},
		"svc.yaml":                    {etcdSvcYaml, map[string]*bintree{}},
	}},
}}

// RestoreAsset restores an asset under the given directory
func RestoreAsset(dir, name string) error {
	data, err := Asset(name)
	if err != nil {
		return err
	}
	info, err := AssetInfo(name)
	if err != nil {
		return err
	}
	err = os.MkdirAll(_filePath(dir, filepath.Dir(name)), os.FileMode(0755))
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(_filePath(dir, name), data, info.Mode())
	if err != nil {
		return err
	}
	err = os.Chtimes(_filePath(dir, name), info.ModTime(), info.ModTime())
	if err != nil {
		return err
	}
	return nil
}

// RestoreAssets restores an asset under the given directory recursively
func RestoreAssets(dir, name string) error {
	children, err := AssetDir(name)
	// File
	if err != nil {
		return RestoreAsset(dir, name)
	}
	// Dir
	for _, child := range children {
		err = RestoreAssets(dir, filepath.Join(name, child))
		if err != nil {
			return err
		}
	}
	return nil
}

func _filePath(dir, name string) string {
	cannonicalName := strings.Replace(name, "\\", "/", -1)
	return filepath.Join(append([]string{dir}, strings.Split(cannonicalName, "/")...)...)
}
