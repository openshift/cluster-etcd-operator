# update-fencing-credentials.sh

## Overview

`update-fencing-credentials.sh` rotates Redfish BMC fencing credentials on Two-Node with Fencing (TNF) clusters. It updates both the Pacemaker
stonith device configuration and the corresponding Kubernetes secret in a single operation.

The script is deployed automatically by the cluster-etcd-operator's scriptcontroller when the cluster topology is TNF. It is not present on
standard or arbiter topology clusters.

## Location

The script is embedded in the `etcd-scripts` ConfigMap and mounted on each control plane node at:

```
/etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/update-fencing-credentials.sh
```

## Usage

```bash
update-fencing-credentials.sh \
--node <node-name> \
--username <bmc-username> \
--password <bmc-password> \
--address <redfish-url> \
[--ssl-insecure]
```

The script must be run as root on a TNF control plane node.

## Parameters

| Parameter | Required | Description |
|-----------|----------|-------------|
| `--node` | Yes | Node name as it appears in `oc get nodes`.|
| `--username` | Yes | Redfish BMC username. |
| `--password` | Yes | Redfish BMC password. |
| `--address` | Yes | Full Redfish URL. Must contain `redfish` in the string (e.g. `redfish+https://192.168.1.10:443/redfish/v1/Systems/1`). |
| `--ssl-insecure` | No | Disable TLS certificate verification when communicating with the BMC. |

### Address format

The `--address` value must include the `redfish` scheme prefix and the full API path:

```
redfish+https://<host>:<port>/redfish/v1/Systems/1
```

For IPv6 addresses, use bracket notation:

```
redfish+https://[fd00::10]:443/redfish/v1/Systems/1
```

The script parses host, port, and path from this URL. If no port is specified, it defaults to 443 (https) or 80 (http).

## What the script does

1. **Validates prerequisites** -- checks root privileges, required dependency files (`etcd.env`, `etcd-common-tools`), and all required
arguments.

2. **Parses the Redfish URL** -- extracts host, port, and API path from the `--address` value; strips the `redfish+` scheme prefix.

3. **Pre-flight validation** -- runs `fence_redfish` with the new credentials to confirm the BMC is reachable and responds correctly. If
validation fails, the script exits without making any changes to the stonith device or Kubernetes secret.

4. **Updates the Pacemaker stonith device** -- runs `pcs stonith update <node>_redfish` with the new credentials (username, password, ip, ipport,
systems_uri, ssl_insecure) and waits up to 120 seconds for the update to apply.

5. **Creates or updates the Kubernetes secret** -- writes a secret named `fencing-credentials-<node>` in the `openshift-etcd` namespace
containing the address, username, password, and certificate verification setting. Uses `--dry-run=client -o yaml | oc apply` for idempotency.

6. **Checks Pacemaker health** -- parses `pcs status xml` to ensure no stonith resources are in a blocked state.

## Running the script

### Via oc debug (from outside the cluster)

```bash
oc debug node/<node-name> -- chroot /host \
/etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/update-fencing-credentials.sh \
--node <target-node> \
--username admin \
--password 'new-password' \
--address 'redfish+https://192.168.1.10:443/redfish/v1/Systems/1' \
--ssl-insecure
```

### Directly on a node (via SSH)

```bash
sudo /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/update-fencing-credentials.sh \
--node master-0.example.com \
--username admin \
--password 'new-password' \
--address 'redfish+https://192.168.1.10:443/redfish/v1/Systems/1'
```

## Security considerations

- The script requires root access on the node.
- The BMC password is passed as a command-line argument and will be visible in the process list while the script runs.
- The password is stored in a Kubernetes secret (`fencing-credentials-<node>`) in the `openshift-etcd` namespace.