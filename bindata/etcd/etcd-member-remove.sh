#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.
set -o errexit
set -o pipefail
set -o errtrace

# example
# sudo ./etcd-member-remove.sh $etcd_name

if [[ $EUID -ne 0 ]]; then
  echo "This script must be run as root"
  exit 1
fi

function usage {
    echo 'The name of the etcd member to remove is required: ./etcd-member-remove.sh $etcd_name'
    exit 1
}

### main
if [ "$1" == "" ]; then
    usage
fi

NAME="$1"

source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd.env
source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

# TODO handle properly
if [ ! -f "$ETCDCTL_CACERT" ] && [ ! -d "${CONFIG_FILE_DIR}/static-pod-certs" ]; then
  ln -s ${CONFIG_FILE_DIR}/static-pod-resources/etcd-certs ${CONFIG_FILE_DIR}/static-pod-certs
fi

# Download etcdctl binary
dl_etcdctl

# If the 1st field or the 3rd field of the member list exactly matches with the name, then get its ID. Note 3rd field has extra space to match.
ID=$(etcdctl member list | awk -F,  "\$1 ~ /^${NAME}$/ || \$3 ~ /^\s${NAME}$/ { print \$1 }")
if [ "$?" -ne 0 ] || [ -z "$ID" ]; then
    echo "could not find etcd member $NAME to remove."
    exit 1
fi

# Remove the member using ID
etcdctl member remove $ID
