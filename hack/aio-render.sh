#!/bin/bash

# openshift-install --dir test/ create aio-config

INSTALLER_ASSETS_DIR="$1"
IGNITION_CONFIG="${INSTALLER_ASSETS_DIR}/aio.ign"

# oc adm release info registry.svc.ci.openshift.org/ocp/release:4.6.0-0.ci-2020-07-21-114552 -o json > release:4.6.0-0.ci-2020-07-21-114552-info.json
RELEASE_INFO="release:4.6.0-0.ci-2020-07-21-114552-info.json"

mkdir -p ./assets/tls

# Unpack the TLS assets from the ignition file
jq  -c '.storage.files[] | {p:.path,c:.contents.source}' "${IGNITION_CONFIG}" | while read f t; do
    p=$(echo $f | jq -r .p)
    c=$(echo $f | jq -r .c)

    [[ "$p" != /opt/openshift/tls/* ]] && continue

    echo "${c#data:text/plain;charset=utf-8;base64,}" | base64 -d > "./assets/tls/$(basename $p)"
done

image_for() {
    jq -r '.references.spec.tags[] | select(.name =="tools") | .from.name' "${RELEASE_INFO}"
}

MACHINE_CONFIG_ETCD_IMAGE=$(image_for etcd)

./cluster-etcd-operator aio \
    --etcd-ca-cert=./assets/tls/etcd-signer.crt \
    --etcd-ca-key=./assets/tls/etcd-signer.key \
    --etcd-metric-ca-cert=./assets/tls/etcd-metric-signer.crt \
    --etcd-metric-ca-key=./assets/tls/etcd-metric-signer.key \
    --asset-output-dir=./assets/etcd-aio \
    --manifest-etcd-image="${MACHINE_CONFIG_ETCD_IMAGE}"
