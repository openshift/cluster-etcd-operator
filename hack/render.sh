#!/bin/bash

# openshift-install --dir test/ create manifests
# openshift-install --dir test/ create aio-config

INSTALLER_ASSETS_DIR="$1"
IGNITION_CONFIG="${INSTALLER_ASSETS_DIR}/aio.ign"

CLUSTER_DOMAIN="markmc.devcluster.openshift.com"

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
CLUSTER_ETCD_OPERATOR_IMAGE=$(image_for cluster-etcd-operator)
MACHINE_CONFIG_OPERATOR_IMAGE=$(image_for machine-config-operator)
MACHINE_CONFIG_KUBE_CLIENT_AGENT_IMAGE=$(image_for kube-client-agent)

./cluster-etcd-operator render \
    --templates-input-dir=./bindata/bootkube \
    --etcd-ca=./assets/tls/etcd-ca-bundle.crt \
    --etcd-metric-ca=./assets/tls/etcd-metric-ca-bundle.crt \
    --manifest-etcd-image="${MACHINE_CONFIG_ETCD_IMAGE}" \
    --etcd-discovery-domain="${CLUSTER_DOMAIN}" \
    --manifest-cluster-etcd-operator-image="${CLUSTER_ETCD_OPERATOR_IMAGE}" \
    --manifest-setup-etcd-env-image="${MACHINE_CONFIG_OPERATOR_IMAGE}" \
    --manifest-kube-client-agent-image="${MACHINE_CONFIG_KUBE_CLIENT_AGENT_IMAGE}" \
    --asset-input-dir=./assets/tls \
    --asset-output-dir=./assets/etcd-bootstrap \
    --config-output-file=./assets/etcd-bootstrap/config \
    --cluster-config-file="${INSTALLER_ASSETS_DIR}/manifests/cluster-network-02-config.yml" \
    --cluster-configmap-file="${INSTALLER_ASSETS_DIR}/manifests/cluster-config.yaml" \
    --infra-config-file="${INSTALLER_ASSETS_DIR}/manifests/cluster-infrastructure-02-config.yml"
