#!/usr/bin/env bash

### Created by cluster-etcd-operator. DO NOT edit.

set -o errexit
set -o pipefail
set -o errtrace

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

function usage {
    echo "Usage: update-fencing-credentials.sh --node <name> --username <user> --password <password> --address <redfish-url> [--ssl-insecure]"
    exit 1
}

# mirrors hashMAC in pkg/tnf/pkg/tools/mac.go
function normalize_and_hash_mac {
    local mac="$1"
    local normalized
    normalized=$(echo "${mac}" | tr '[:upper:]' '[:lower:]' | tr -d ':-')
    if ! [[ "${normalized}" =~ ^[0-9a-f]{12}$ ]]; then
        echo "Invalid MAC address: ${mac}" >&2
        return 1
    fi
    echo -n "${normalized}" | sha256sum | awk '{print $1}'
}

# mirrors Phases 1-2 of GetFencingSecrets in pkg/tnf/pkg/tools/secrets.go;
# Phase 3 uses stonith device address matching instead of Redfish UUID query
function detect_fencing_secret {
    local node="$1"
    local namespace="openshift-etcd"
    local mac_annotation_key="tnf.openshift.io/mac-addresses"

    if ! oc get namespace "${namespace}" &>/dev/null; then
        echo "ERROR: Cannot reach Kubernetes API server" >&2
        return 1
    fi

    # Phase 1: hostname-based lookup
    local hostname_secret="fencing-credentials-${node}"
    if oc get secret "${hostname_secret}" --namespace="${namespace}" &>/dev/null; then
        echo "${hostname_secret}"
        return 0
    fi

    # Phase 2: MAC hash lookup from node annotation
    local mac_annotation
    mac_annotation=$(oc get node "${node}" -o jsonpath="{.metadata.annotations.${mac_annotation_key//\./\\.}}" 2>/dev/null) || true
    if [ -n "${mac_annotation}" ]; then
        IFS=',' read -ra macs <<< "${mac_annotation}"
        for mac in "${macs[@]}"; do
            mac=$(echo "${mac}" | tr -d '[:space:]')
            [ -z "${mac}" ] && continue
            local hash
            hash=$(normalize_and_hash_mac "${mac}") || continue
            local mac_secret="fencing-credentials-${hash}"
            if oc get secret "${mac_secret}" --namespace="${namespace}" &>/dev/null; then
                echo "${mac_secret}"
                return 0
            fi
        done
    fi

    # Phase 3: targeted address matching (no bulk secret dump)
    local stonith_ip="$2"
    local stonith_uri="$3"
    if [ -n "${stonith_ip}" ] && [ -n "${stonith_uri}" ]; then
        local name addr
        for name in $(oc get secrets -n "${namespace}" -o name 2>/dev/null | grep fencing-credentials-); do
            addr=$(oc get "${name}" -n "${namespace}" -o jsonpath='{.data.address}' 2>/dev/null | base64 -d) || continue
            if [[ "${addr}" == *"://${stonith_ip}:"* ]] || [[ "${addr}" == *"://${stonith_ip}/"* ]] || \
               [[ "${addr}" == *"://\[${stonith_ip}\]:"* ]] || [[ "${addr}" == *"://\[${stonith_ip}\]/"* ]]; then
                if [[ "${addr}" == *"${stonith_uri}" ]]; then
                    echo "${name#secret/}"
                    return 0
                fi
            fi
        done
    fi

    return 1
}

function redact_credentials {
    local input
    input=$(cat)
    input="${input//${USERNAME}/****}"
    input="${input//${PASSWORD}/****}"
    printf '%s\n' "${input}"
}

NODE=""
USERNAME=""
PASSWORD=""
ADDRESS=""
SSL_INSECURE=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --node)     NODE="$2";      shift 2 ;;
        --username) USERNAME="$2";  shift 2;;
        --password) PASSWORD="$2";  shift 2;;
        --address)  ADDRESS="$2";   shift 2;;
        --ssl-insecure) SSL_INSECURE=true;  shift ;;
        *) usage ;;
    esac
done

if [ -z "${NODE}" ] || [ -z "${USERNAME}" ] || [ -z "${PASSWORD}" ] || [ -z "${ADDRESS}" ]; then
    usage
fi

# mirrors getFencingConfig in pkg/tnf/pkg/pcs/fencing.go
if [[ "${ADDRESS}" != *redfish* ]]; then
    echo "Address does not contain a valid redfish URL: ${ADDRESS}"
    exit 1
fi

# strip the redfish+ prefix scheme (e.g. redfish+https://... -> https://..)
PARSED_URL="${ADDRESS#redfish+}"

# Parse IPv6 (bracketed) vs IPv4/hostname
if [[ "${PARSED_URL}" =~ ^https?://\[([^]]+)\](:([0-9]+))?(/.*)? ]]; then
    REDFISH_HOST="${BASH_REMATCH[1]}"
    REDFISH_PORT="${BASH_REMATCH[3]}"
    REDFISH_PATH="${BASH_REMATCH[4]}"
    REDFISH_IP="[${REDFISH_HOST}]"
else
    REDFISH_HOST=$(echo "${PARSED_URL}" | sed -E 's|https?://([^/:]+).*|\1|')
    REDFISH_PORT=$(echo "${PARSED_URL}" | sed -E 's|https?://[^/:]+:([0-9]+).*|\1|')
    REDFISH_PATH=$(echo "${PARSED_URL}" | sed -E 's|https?://[^/]+(/.*)|\1|')
    REDFISH_IP="${REDFISH_HOST}"
fi

# infer port from scheme if not explicitly provided
if [ -z "${REDFISH_PORT}" ] || [ "${REDFISH_PORT}" = "${PARSED_URL}" ]; then
    if [[ "${PARSED_URL}" == https://* ]]; then
        REDFISH_PORT="443"
    else
        REDFISH_PORT="80"
    fi
fi

if "${SSL_INSECURE}"; then
    SSL_INSECURE_VAL="1"
    CERT_VERIFICATION="Disabled"
else
    SSL_INSECURE_VAL="0"
    CERT_VERIFICATION="Enabled"
fi

DEVICE_ID="${NODE}_redfish"

if ! pcs stonith config "${DEVICE_ID}" &>/dev/null; then
    echo "ERROR: Stonith device '${DEVICE_ID}' not found"
    echo "Available devices: $(pcs stonith status 2>/dev/null | grep -oP '\S+_redfish' || echo 'none')"
    echo "Verify node name with: oc get nodes"
    exit 1
fi

NAMESPACE="openshift-etcd"

STONITH_CFG=$(pcs stonith config "${DEVICE_ID}" 2>/dev/null || true)
STONITH_IP=$(echo "${STONITH_CFG}" | sed -n 's/.*[[:space:]]ip=\([^[:space:]]*\).*/\1/p' | head -1)
STONITH_URI=$(echo "${STONITH_CFG}" | sed -n 's/.*[[:space:]]systems_uri=\([^[:space:]]*\).*/\1/p' | head -1)

echo "Detecting fencing secret for node ${NODE}..."
if ! SECRET_NAME=$(detect_fencing_secret "${NODE}" "${STONITH_IP}" "${STONITH_URI}"); then
    echo "No fencing secret found for node ${NODE}"
    echo "  Phase 1: tried fencing-credentials-${NODE}"
    echo "  Phase 2: tried MAC hashes from annotation tnf.openshift.io/mac-addresses"
    if [ -n "${STONITH_IP}" ] && [ -n "${STONITH_URI}" ]; then
        echo "  Phase 3: tried matching stonith address ip=${STONITH_IP} systems_uri=${STONITH_URI}"
    else
        echo "  Phase 3: skipped (stonith device has no address configured)"
    fi
    echo "Verify the node name matches 'oc get nodes' and that a fencing secret exists in namespace openshift-etcd"
    exit 1
fi
echo "Detected fencing secret: ${SECRET_NAME}"
if [ -n "${STONITH_IP}" ] && [ -n "${STONITH_URI}" ]; then
    if [ "${STONITH_IP}" != "${REDFISH_IP}" ] || [ "${STONITH_URI}" != "${REDFISH_PATH}" ]; then
        echo "ERROR: --address does not match the BMC address configured on stonith device ${DEVICE_ID}"
        echo "  Stonith device: ip=${STONITH_IP} systems_uri=${STONITH_URI}"
        echo "  --address:      ip=${REDFISH_IP} systems_uri=${REDFISH_PATH}"
        echo "No changes were made"
        exit 1
    fi
fi

echo "Validating new credentials against Redfish endpoint ${REDFISH_IP}:${REDFISH_PORT}${REDFISH_PATH}..."

FENCE_ARGS=(--username "${USERNAME}" --password "${PASSWORD}" --ip "${REDFISH_IP}" --ipport "${REDFISH_PORT}" --systems-uri "${REDFISH_PATH}" --action status)
if "${SSL_INSECURE}"; then
    FENCE_ARGS+=(--ssl-insecure)
fi

if ! PREFLIGHT_OUTPUT=$(/usr/sbin/fence_redfish "${FENCE_ARGS[@]}" 2>&1); then
    echo "Pre-flight validation failed: new credentials do not work against the Redfish endpoint"
    echo "  Endpoint: ${REDFISH_IP}:${REDFISH_PORT}${REDFISH_PATH}"
    echo "  SSL insecure: ${SSL_INSECURE}"
    echo "${PREFLIGHT_OUTPUT}" | redact_credentials
    echo "Check BMC connectivity and verify credentials are correct"
    echo "No changes were made to stonith device or Kubernetes secret"
    exit 1
fi
echo "Pre-flight validation passed: new credentials are valid"

echo "Updating stonith device ${DEVICE_ID}"
# matches getStonithCommand() format from fencing.go
STDERR=$(/usr/sbin/pcs stonith update "${DEVICE_ID}" \
    username="${USERNAME}" \
    password="${PASSWORD}" \
    ip="${REDFISH_IP}" \
    ipport="${REDFISH_PORT}" \
    systems_uri="${REDFISH_PATH}" \
    ssl_insecure="${SSL_INSECURE_VAL}" \
    --wait=120 2>&1 > /dev/null) || true

# stderr must contain "is running"
if [[ "${STDERR}" != *"is running"* ]]; then
    echo "Failed to update stonith device ${DEVICE_ID}: $(echo "${STDERR}" | redact_credentials)"
    exit 1
fi
echo "Stonith device ${DEVICE_ID} updated successfully"

echo "Updating secret ${SECRET_NAME} in namespace ${NAMESPACE}"
# KUBECONFIG is set by etcd-common-tools
if ! oc create secret generic "${SECRET_NAME}" \
    --namespace="${NAMESPACE}" \
    --from-literal=address="${ADDRESS}" \
    --from-literal=username="${USERNAME}" \
    --from-literal=password="${PASSWORD}" \
    --from-literal=certificateVerification="${CERT_VERIFICATION}" \
    --dry-run=client -o yaml | oc apply -f -; then
    echo "WARNING: Stonith device ${DEVICE_ID} was updated successfully,"
    echo "but the Kubernetes secret could not be updated."
    echo "The CEO operator may revert the stonith device on next reconciliation."
    echo "When API access is restored, re-run this script to update the secret."
    exit 1
fi

echo "Secret ${SECRET_NAME} updated successfully"

echo "Checking pacemaker cluster health..."
if ! PCS_STATUS=$(pcs status xml 2>&1); then
    echo "Failed to get pacemaker status: ${PCS_STATUS}"
    exit 1
fi

# check for fencing failures in pacemaker status
if echo "${PCS_STATUS}" | grep -q 'resource_agent="stonith:.*blocked="true"\|blocked="true".*resource_agent="stonith:'; then
    echo "Warning: pacemaker reports blocked stonith resources"
    exit 1
fi


echo "Pacemaker cluster health verified"

echo ""
echo "Fencing credentials update completed successfully:"
echo "Stonith device:   ${DEVICE_ID}"
echo "Secret:           ${NAMESPACE}/${SECRET_NAME}"
echo "Address:          ${ADDRESS}"
echo "Username:         ${USERNAME}"
echo "SSL insecure:     ${SSL_INSECURE}"