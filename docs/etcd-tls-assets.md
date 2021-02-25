# etcd TLS Assets

As of OpenShift 4.4, etcd is deployed on every control plane node as a
static pod managed by cluster-etcd-operator (CEO).

This document describes how the TLS certificates for these static pods
are bootstrapped and managed.

# Background

To understand this configuration, it's helpful to first review the
[etcd transport security model
documentation](https://etcd.io/docs/v3.4.0/op-guide/security/).

# etcd certificates

There are several categories of certificates involved in securing etcd
communications. This section looks at what each of those are and how
to inspect the canonical copy of each on a running cluster.

See also the [user-facing
documentation](https://docs.openshift.com/container-platform/4.5/security/certificate-types-descriptions.html#etcd-certificates_ocp-certificates)
for these certificates.

## etcd CA summary

All etcd CAs and their CA bundles are stored in the `openshift-config`
namespace.

| CA (secret)        | CA bundle (configmap)            | CA bundle also appearing in                  |
| ------------------ | -------------------------------- | -------------------------------------------- |
| etcd-signer        | etcd-ca-bundle                   | openshift-etcd                               |
|                    |                                  | openshift-etcd-operator                      |
|                    |                                  | openshift-etcd/etcd-peer-client-ca           |
|                    | etcd-serving-ca                  | openshift-etcd                               |
| etcd-metric-signer | etcd-metric-serving-ca           | openshift-etcd/etcd-metrics-proxy-client-ca  |
|                    |                                  | openshift-etcd/etcd-metrics-proxy-serving-ca |

## etcd cert summary

All etcd certificates are stored in secrets.

| CA                 | Certificate                               | Purpose                          | Certifiate also appearing in                |
| ------------------ | ----------------------------------------- | -------------------------------- | ------------------------------------------- |
| etcd-signer        | openshift-config/etcd-client              | authn kube api to etcd           | openshift-etcd                              |
|                    |                                           |                                  | openshift-etcd-operator                     |
|                    | openshift-etcd/etcd-peer-$node            | etcd peer communication          | collected in etcd-all-certs                 |
|                    | openshift-etcd/etcd-serving-$node         | etcd member serving              | collected in etcd-all-certs                 |
| etcd-metric-signer | openshift-config/etcd-metric-client       | authn prometheus to etcd metrics | openshift-monitoring/kube-etcd-client-certs |
|                    | openshift-etcd/etcd-serving-metrics-$node | etcd member metrics serving      | collected in etcd-all-certs                 |

## etcd-signer and etcd-metric-signer CA certs

The cluster hosts two certificate authorities (CA) for etcd -
`etcd-signer` for certs relating to client/server and peer/peer
communication, and `etcd-metric-signer` for certs relating to etcd
metrics serving and collection. These certs can only be used for
signing.

The self-signed certs (and associated private keys) for these CAs are
stored in secrets in the `openshift-config` namespace. The CA certs
alone also stored several `ca-bundle` config maps.

```
$ oc get -n openshift-config secret/etcd-signer -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-signer
        Validity
            Not Before: Jul 20 13:31:58 2020 GMT
            Not After : Jul 18 13:31:58 2030 GMT
        Subject: OU = openshift, CN = etcd-signer
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier:
                7E:88:6E:AB:ED:36:42:88:6D:99:BD:3F:C6:53:EB:7C:7B:FB:B6:14

$ oc get -n openshift-config configmap/etcd-ca-bundle -o template='{{index .data "ca-bundle.crt"}}'  | openssl x509 -noout -ext subjectKeyIdentifier
X509v3 Subject Key Identifier:
    7E:88:6E:AB:ED:36:42:88:6D:99:BD:3F:C6:53:EB:7C:7B:FB:B6:14

$ oc get -n openshift-config configmap/etcd-serving-ca -o template='{{index .data "ca-bundle.crt"}}'  | openssl x509 -noout -ext subjectKeyIdentifier
X509v3 Subject Key Identifier
    7E:88:6E:AB:ED:36:42:88:6D:99:BD:3F:C6:53:EB:7C:7B:FB:B6:14
```

```
$ oc get -n openshift-config secret/etcd-metric-signer -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-metric-signer
        Validity
            Not Before: Jul 20 13:31:59 2020 GMT
            Not After : Jul 18 13:31:59 2030 GMT
        Subject: OU = openshift, CN = etcd-metric-signer
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier:
                8B:91:6C:2E:13:A2:D5:DB:DD:3D:C1:7A:33:86:F7:31:F6:EB:45:68

$ oc get -n openshift-config configmap/etcd-metric-serving-ca -o template='{{index .data "ca-bundle.crt"}}'  | openssl x509 -noout -ext subjectKeyIdentifier
X509v3 Subject Key Identifier:
    8B:91:6C:2E:13:A2:D5:DB:DD:3D:C1:7A:33:86:F7:31:F6:EB:45:68
```

## etcd-client and etcd-metric-client certs

The `etcd-signer` CA issues an `etcd-client` cert that
`kube-apiserver` (for example) uses to authenticate to etcd. This is
stored in the `openshift-config` namespace.

```
$ oc get -n openshift-config secret/etcd-client -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-signer
        Validity
            Not Before: Jul 20 13:31:58 2020 GMT
            Not After : Jul 18 13:31:59 2030 GMT
        Subject: OU = etcd, CN = etcd
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Key Encipherment
            X509v3 Extended Key Usage:
                TLS Web Client Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier:
                6F:42:21:98:A8:19:1E:78:82:D5:49:5B:04:79:08:BE:53:10:E7:CB
            X509v3 Authority Key Identifier:
                keyid:7E:88:6E:AB:ED:36:42:88:6D:99:BD:3F:C6:53:EB:7C:7B:FB:B6:14
```

Similarly, the `etcd-metric-signer` CA issues a client cert that
prometheus uses when authenticating with the etcd metrics server. The
`cluster-monitoring-operator` copies this into the
`kube-etcd-client-certs` secret in the `openshift-monitoring`
namespace.

```
$ oc get -n openshift-config secret/etcd-metric-client -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-metric-signer
        Validity
            Not Before: Jul 20 13:31:59 2020 GMT
            Not After : Jul 18 13:31:59 2030 GMT
        Subject: OU = etcd-metric, CN = etcd-metric
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage:
                TLS Web Client Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier:
                3E:8B:89:44:B4:57:36:5F:26:38:83:55:8C:51:DD:80:21:6F:EB:D9
            X509v3 Authority Key Identifier:
                keyid:8B:91:6C:2E:13:A2:D5:DB:DD:3D:C1:7A:33:86:F7:31:F6:EB:45:68
```

## etcd-serving certs

Each control plane node is issued a serving cert (by the `etcd-signer`
CA) to secure the client-server communication on port 2379. This cert
(and its private key) is stored in an `etcd-serving-$node` secret in
the `openshift-etcd` namespace.

```
$ oc get -n openshift-etcd secret/etcd-serving-master-0 -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-signer
        Validity
            Not Before: Jul 20 13:53:54 2020 GMT
            Not After : Jul 20 13:53:55 2023 GMT
        Subject: O = system:etcd-servers, CN = system:etcd-server:etcd-client
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage:
                TLS Web Client Authentication, TLS Web Server Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier:
                86:B7:BD:AE:B7:2B:A0:28:03:E9:E8:E8:BA:5C:7C:EE:28:E9:35:4B
            X509v3 Authority Key Identifier:
                keyid:7E:88:6E:AB:ED:36:42:88:6D:99:BD:3F:C6:53:EB:7C:7B:FB:B6:14

            X509v3 Subject Alternative Name:
                DNS:etcd.kube-system.svc, DNS:etcd.kube-system.svc.cluster.local, DNS:etcd.openshift-etcd.svc, DNS:etcd.openshift-etcd.svc.cluster.local, DNS:localhost, DNS:::1, DNS:127.0.0.1, DNS:192.168.111.20, DNS:::1, IP Address:0:0:0:0:0:0:0:1, IP Address:127.0.0.1, IP Address:192.168.111.20, IP Address:0:0:0:0:0:0:0:1
    ...
```

Note that the `subjectAltName` field lists the various ways that a
client might connect to etcd.

Note that this serving cert can also be used as a client cert, but it
appears it is never used for this.

## etcd-peer certs

Similarly, each control plane node is issued a certificate (again by
the `etcd-signer` CA) to secure the peer-to-peer communication on port
2380. This is stored in an `etc-peer-$node` secret.

```
$ oc get -n openshift-etcd secret/etcd-peer-master-0 -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-signer
        Validity
            Not Before: Jul 20 13:53:53 2020 GMT
            Not After : Jul 20 13:53:54 2023 GMT
        Subject: O = system:etcd-peers, CN = system:etcd-peer:etcd-client
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage:
                TLS Web Client Authentication, TLS Web Server Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier:
                5B:7B:04:E6:0B:4C:CD:DB:32:0C:F6:6C:25:5C:1E:16:11:A0:CF:6E
            X509v3 Authority Key Identifier:
                keyid:7E:88:6E:AB:ED:36:42:88:6D:99:BD:3F:C6:53:EB:7C:7B:FB:B6:14

            X509v3 Subject Alternative Name:
                DNS:localhost, DNS:192.168.111.20, IP Address:192.168.111.20
```

Note that this cert can also be for client authentication - i.e. peers
use this to authenticate when connecting to another peer.

Note that there is a much more limited set of names listed in the
`subectAltName` field. Peers usually connect via an explicitly
specified IP address.

## etcd-serving-metrics certs

Each control plane node is also issued a certificate (by the
`etcd-metric-signer` CA) for serving metrics on port 9979. It is
stored in the `etcd-serving-metrics-$node` secret.

```
$ oc get -n openshift-etcd secret/etcd-serving-metrics-master-0 -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: OU = openshift, CN = etcd-metric-signer
        Validity
            Not Before: Jul 20 13:53:54 2020 GMT
            Not After : Jul 20 13:53:55 2023 GMT
        Subject: O = system:etcd-metrics, CN = system:etcd-metric:etcd-client
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage:
                TLS Web Client Authentication, TLS Web Server Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier:
                91:AF:FE:16:FD:29:53:2B:52:36:A8:95:27:E7:82:47:08:C4:0F:0A
            X509v3 Authority Key Identifier:
                keyid:8B:91:6C:2E:13:A2:D5:DB:DD:3D:C1:7A:33:86:F7:31:F6:EB:45:68

            X509v3 Subject Alternative Name:
                DNS:etcd.kube-system.svc, DNS:etcd.kube-system.svc.cluster.local, DNS:etcd.openshift-etcd.svc, DNS:etcd.openshift-etcd.svc.cluster.local, DNS:localhost, DNS:::1, DNS:127.0.0.1, DNS:192.168.111.20, DNS:::1, IP Address:0:0:0:0:0:0:0:1, IP Address:127.0.0.1, IP Address:192.168.111.20, IP Address:0:0:0:0:0:0:0:1
```

## Service CA

You may notice an
`openshift-etcd-operator/configmap/etcd-service-ca-bundle` resource:

```
$ oc get -n openshift-etcd-operator configmap/etcd-service-ca-bundle  -o template='{{index .data "service-ca.crt"}}' | openssl x509 -noout -text
Certificate:
    Data:
        ...
        Issuer: CN = openshift-service-serving-signer@1595253236
        Validity
            Not Before: Jul 20 13:53:56 2020 GMT
            Not After : Sep 18 13:53:57 2022 GMT
        Subject: CN = openshift-service-serving-signer@1595253236
        ...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier:
                E8:2A:E0:3F:09:29:F9:79:59:DB:CE:1A:E3:0D:47:2F:0E:43:43:25
            X509v3 Authority Key Identifier:
                keyid:E8:2A:E0:3F:09:29:F9:79:59:DB:CE:1A:E3:0D:47:2F:0E:43:43:25
```

This is the cert for the service CA implemented by the
[service-ca-operator](https://github.com/openshift/service-ca-operator)
component.

The CEO uses the service CA to issue a serving cert for the CEO's own
metric serving. See the `openshift-etcd-operator/service/metrics`
service:

```
$ oc get -n openshift-etcd-operator service/metrics -o template='{{range $k, $v := .metadata.annotations}}{{printf "%s = %s\n" $k $v}}{{end}}'
service.alpha.openshift.io/serving-cert-secret-name = etcd-operator-serving-cert
service.alpha.openshift.io/serving-cert-signed-by = openshift-service-serving-signer@1595253236

$ $ oc get -n openshift-etcd-operator secret/etcd-operator-serving-cert -o template='{{index .data "tls.crt"}}' | base64 -d | openssl x509 -noout -issuer -subject
issuer=CN = openshift-service-serving-signer@1595253236
subject=CN = metrics.openshift-etcd-operator.svc
```

Note the `openshift-etcd/services/etcd` resource is similarly
annotated but the `openshift-etcd/secret/serving-cert` appears
unused.

# etcd static pods configuration

The etcd static pod on each control plane node runs a number of
containers - see `/etc/kubernetes/manifests/etcd-pod.yaml` - but etcd
itself is executed with the following arguments:

```
        exec etcd \
          ...
          --cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-master-0.crt \
          --key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-master-0.key \
          --trusted-ca-file=/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt \
          --client-cert-auth=true \
          --peer-cert-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-master-0.crt \
          --peer-key-file=/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-master-0.key \
          --peer-trusted-ca-file=/etc/kubernetes/static-pod-certs/configmaps/etcd-peer-client-ca/ca-bundle.crt \
          --peer-client-cert-auth=true \
          ...
          --listen-client-urls=https://0.0.0.0:2379 \
          --listen-peer-urls=https://0.0.0.0:2380 \
          --listen-metrics-urls=https://0.0.0.0:9978
```

This can be summarized as follows:

- etcd clients are served on port 2379, peer etcd cluster members are
  served on port 2380, and prometheus metrics are served on port 9978
- `--cert-file` and `--key-file` specify the `etcd-serving` cert and
  key for client-to-server communication (on port 2379) with this
  instance of etcd. Each node has its own serving cert; in this case
  the node name is `master-0`.
- Note that the metrics serving on port 9978 is secured with this same
  serving cert, but it is then proxied by `grpc-proxy` as described
  below.
- `--client-cert-auth=true` and `--trusted-ca-file` specify that
  clients must authenticate with a certificate signed the
  `etcd-signer` CA cert in the supplied CA bundle.
- `--peer-cert-file` and `--peer-key-file` specify the serving cert
  and key for peer-to-peer communication (on port 2380). Again, each
  node has its own serving cert.
- `--peer-client-cert-auth=true` and `-peer-trusted-ca-file` enables
  certificate based authentication of connections from etcd peers, but
  it's again the `etcd-signer` CA that is trusted.

Also in the etcd pod, a `grpc-proxy` instance is launched which will
serve prometheus metrics on port 9979. This is secured as follows:

```
        exec etcd grpc-proxy start \
          --endpoints https://${NODE_master_0_ETCD_URL_HOST}:9978 \
          --metrics-addr https://0.0.0.0:9979 \
          ...
          --key /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-master-0.key \
          --key-file /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-metrics-master-0.key \
          --cert /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-master-0.crt \
          --cert-file /etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-serving-metrics-master-0.crt \
          --cacert /etc/kubernetes/static-pod-certs/configmaps/etcd-peer-client-ca/ca-bundle.crt \
          --trusted-ca-file /etc/kubernetes/static-pod-certs/configmaps/etcd-metrics-proxy-serving-ca/ca-bundle.crt
```

- `--endpoint ... :9978` specifies that we're proxying etcd's metrics
  serving.
- `--metrics-addr ... :9979` the proxied metrics serving will be on
  port 9979.
- `--cert`, `--key`, `--cacert` configures the proxy's TLS client with
  this node's `etcd-peer` cert to connect to etcd.
- `--cert-file`, `--key-file` configures the proxy's metrics server to
  use the etcd-serving cert for this node.
- `--trusted-ca-file` enables client authentication for the grpc proxy
  - the `etcd-metric-signer` CA is trusted.

It can be helpful to inspect these certs on disk on a control plane
machine. They are stored in the
`/etc/kubernetes/static-pod-resources/etcd-certs` which is mounted
into the etcd pod under `/etc/kubernetes/static-pod-certs`.

```
$ cd /etc/kubernetes/static-pod-resources/etcd-certs
$ for crt in configmaps/etcd-serving-ca/ca-bundle.crt configmaps/etcd-peer-client-ca/ca-bundle.crt configmaps/etcd-metrics-proxy-serving-ca/ca-bundle.crt secrets/etcd-all-certs/etcd-serving-master-0.crt secrets/etcd-all-certs/etcd-peer-master-0.crt secrets/etcd-all-certs/etcd-serving-metrics-master-0.crt; do echo -n "$crt: "; openssl x509 -noout -subject < $crt; done
configmaps/etcd-serving-ca/ca-bundle.crt: subject=OU = openshift, CN = etcd-signer
configmaps/etcd-peer-client-ca/ca-bundle.crt: subject=OU = openshift, CN = etcd-signer
configmaps/etcd-metrics-proxy-serving-ca/ca-bundle.crt: subject=OU = openshift, CN = etcd-metric-signer
secrets/etcd-all-certs/etcd-serving-master-0.crt: subject=O = system:etcd-servers, CN = system:etcd-server:etcd-client
secrets/etcd-all-certs/etcd-peer-master-0.crt: subject=O = system:etcd-peers, CN = system:etcd-peer:etcd-client
secrets/etcd-all-certs/etcd-serving-metrics-master-0.crt: subject=O = system:etcd-metrics, CN = system:etcd-metric:etcd-client
```

As expected, there are the two CA certs (`etcd-signer`,
`etcd-metric-signer`) and three serving certs (`etcd-server`,
`etcd-peer`, `etcd-metric`).

You may notice the `/etc/kubernetes/static-pod-resources/etcd-member`
directory contains a backup copy of some of these assets. These are
intended to be used in recovery situations where you can connect to
the `ectdctl` container in the etcd pod:

```
$ oc exec -n openshift-etcd etcd-master-0 -c etcdctl -it -- ls /etc/kubernetes/etcd-backup-dir/
ca.crt	metric-ca.crt  root-ca.crt  system:etcd-peer-master-0.crt  system:etcd-peer-master-0.key
```

# cluster-etcd-operator

The `cluster-etcd-operator` component is what manages this static pod
configuration and the TLS certificates.

## Static pod operator

To manage the etcd static pods, `cluster-etcd-operator` uses
OpenShift's [static pod operator
library](https://github.com/openshift/library-go/blob/master/pkg/operator/staticpod/controllers.go)
and reconciles a custom resource based on the
[`StaticPodOperator`](https://github.com/openshift/api/blob/89de68875e7c763b0cb327ab68bf93af1e4c2f61/operator/v1/types.go#L165-L214)
type. The `kube-apisever`, `kube-scheduler`, and
`kube-controller-manager` static pods are managed the same way.

Inspecting the etcd operator resource is a helpful starting point:

```
$ oc describe etcd/cluster
Name:         cluster
Namespace:
...
API Version:  operator.openshift.io/v1
Kind:         Etcd
...
Spec:
  Management State:  Managed
Status:
  Conditions:
    ...
    Last Transition Time:            2020-07-20T13:55:13Z
    Message:                         3 nodes are active; 3 nodes are at revision 4
    Status:                          True
    Type:                            StaticPodsAvailable
    ...
    Last Transition Time:            2020-07-20T13:55:58Z
    Message:                         3 members are available
    Reason:                          EtcdQuorate
    Status:                          True
    Type:                            EtcdMembersAvailable
  Latest Available Revision:         4
  ...
  Node Statuses:
    Current Revision:  4
    Node Name:         master-0
    Current Revision:  4
    Node Name:         master-2
    Current Revision:  4
    Node Name:         master-1
...
```

Some key points to consider:

- The `spec.managementState` field is usually set to `Managed`;
  setting it to `Unmanaged` or `Removed` has the effect of disabling
  many of the controllers described below.
- The conditions give helpful information about the state of the etcd
  cluster - in the example above it shows that all the nodes are
  running the same static pod configuration, and all 3 etcd cluster
  members are healthy.
- The node statuses show in detail which configuration revision is
  installed on each node - in this case, all nodes are running the
  latest revision.

The below sections briefly discuss the role of each controller in the
static pod operator library.

### Node controller

Ensures that any node with the `node-role.kubernetes.io/master` label
has an entry in the `status.nodeStatuses` list. The other controllers
use this list to iterate over all control plane nodes.

### Revision controller

This controller watches a [set of config maps and
secrets](https://github.com/openshift/cluster-etcd-operator/blob/0ecd5d2b72df7648769b8625a35de5e792cf707d/pkg/operator/starter.go#L260-L277)
for changes. Any change to those resources constitute a new numbered
"revision".

When a new revision is detected, copies of all these resources are
created with the revision number as a name suffix. Resources may be
flagged as `optional`, in which case their non-existance will not be
treated as an error.

The new revision is then recorded in the
`status.latestAvailableRevision` field and a `revision-$suffix`
configmap is created to record the status (`In Progress`, `Succeeded`,
`Failed`, `Abandoned`) of the installation of this revision.

### Installer controller

When a new revision is available, a pod is launched on each node to
read the copied resources for this revision and write those to disk -
with some basic templating - to a revision-specific directory like
`/etc/kubernetes/static-pod-resources/etcd-pod-$revision` (based on
the name of the static pod config map).

The installer is also asked to write out an additional [set of
resources for certs and
keys](https://github.com/openshift/cluster-etcd-operator/blob/0ecd5d2b72df7648769b8625a35de5e792cf707d/pkg/operator/starter.go#L279-L294)
for which revision-suffixed copies aren't created. These are written
to an unrevisioned directory, in this case
`/etc/kubernetes/static-pod-resources/etcd-certs`.

In the case of etcd, `cluster-etcd-operator installer` is the
installer command executed, and it is supplied with the details of the
resources to copy and their on-disk destination for. The most
significant of these resources is the manifest for the static pod
itself, which gets written to `/etc/kubernetes/manifests` causing
kubelet to launch this new revision of the pod.

A separate controller - the backing resource controller - is
responsible for ensuring the existence of an `installer-sa` service
account with the `cluster-admin` role. This is the service account
under which the installer (and pruner) pods runs.

The revisioned resources destined for
`/etc/kubernetes/static-pod-resources/etcd-pod-$revision` for etcd
are:

* Config maps
    - `etcd-pod` - the first element in the list has a special meaning
      it is the static pod itself, and is to be installed in
      `/etc/kubernetes/manifests`.
    - `config` - an `EtcdConfig` file
    - `etcd-serving-ca`, `etcd-peer-client-ca`,
      `etcd-metrics-proxy-serving-ca`, `etcd-metrics-proxy-client-ca`
      - CA certs described above, copied from the `openshift-config`
      namespace by the resource sync controller described below.
* Secrets:
    - `etcd-all-certs` includes all certs for all nodes in a single
      secret to reduce the complexity of managing the resources to
      watch. The `etcd-signer` controller maintains both the
      `etcd-all-certs` secret and the node- and type-specific cert
      secrets it aggregates.

The unrevisioned resources destined for
`/etc/kubernetes/static-pod-resources/etcd-certs` are:

* Config maps:
    - `etcd-scripts`, `restore-etcd-pod` - etcd backup and restore
      scripts.
    - `etcd-serving-ca`, `etcd-peer-client-ca`,
      `etcd-metrics-proxy-serving-ca`, `etcd-metrics-proxy-client-ca` -
      as above.
* Secrets:
    - `etcd-all-certs` as above.

Note it appears we [only use the unrevisioned
copies](https://github.com/openshift/cluster-etcd-operator/pull/217)
of all these certs and secrets.

### Prune controller

Launches a pruner pod - again under the `installer-sa` service account
- on each node that runs `cluster-kube-apiserver-operator prune` to
delete old revisions from disk. Also deletes old revisions of the API
resources.

Retains a configurable number of revisions that have been successfully
installed, another set of failed revisions, along with any revision in
other states.

## Resource sync controller

A CEO controller, separate from the `StaticPodOperator` controllers
above, copies a bunch of config maps and secrets from the
`openshift-config` namespace to either the `openshift-etcd` or
`openshift-etcd-operator` namespace. The list of resources copied are:

- `configmaps/etcd-ca-bundle` and `secrets/etcd-client` to both
  namespaces
- `openshift-config/configmaps/etcd-serving-ca` to `openshift-etcd`
- `openshift-config/configmaps/etcd-ca-bundle` to
  `openshift-etcd/configmaps/etcd-peer-client-ca`
- `openshift-config/configmaps/etcd-metric-serving-ca` to
  `openshift-etcd/configmaps/etcd-metrics-proxy-serving-ca` and
  `openshift-etcd/configmaps/etcd-metrics-proxy-client-ca`

## etcd cert signer

The cluster-hosted `etcd-signer` CA (and the `etcd-metrics-signer` CA)
is implemented by another control loop in CEO, separate from the
`StaticPodOperator` control loops described above.

It's primary function is to iterate across all control plane nodes
(i.e. those labelled with `node-role.kubernetes.io/master`), creating
a secrets for each node's `etcd-serving-$node`, `etcd-peer-$node`, and
`etcd-metrics-serving-$node` certificates in the `openshift-etcd`
namespace. It also creates the `etcd-all-certs` combined secret.

As described above, the `etcd-signer` and `etcd-metrics-signer`
secrets in the `openshift-config` contain the signing certs and keys
used by this CA.

# Bootstrap process

The above sections described the TLS assets, etcd static pods, and
related CEO control loops on a running cluster. This section considers
how this configuration was initially bootstrapped on the bootstrap
machine.

## Bootstrap Ignition

To get oriented with the bootstrap ignition process, you can browse
the list of files and systemd units in the bootstrap ignition file:

```
$ jq -r .storage.files[].path bootstrap.ign
...
/usr/local/bin/bootkube.sh
...
/opt/openshift/manifests/etcd-ca-bundle-configmap.yaml
/opt/openshift/manifests/etcd-client-secret.yaml
/opt/openshift/manifests/etcd-metric-client-secret.yaml
/opt/openshift/manifests/etcd-metric-serving-ca-configmap.yaml
/opt/openshift/manifests/etcd-metric-signer-secret.yaml
/opt/openshift/manifests/etcd-signer-secret.yaml
...
/opt/openshift/tls/etcd-ca-bundle.crt
/opt/openshift/tls/etcd-metric-ca-bundle.crt
/opt/openshift/tls/etcd-metric-signer.key
/opt/openshift/tls/etcd-metric-signer.crt
/opt/openshift/tls/etcd-metric-signer-client.key
/opt/openshift/tls/etcd-metric-signer-client.crt
/opt/openshift/tls/etcd-signer.key
/opt/openshift/tls/etcd-signer.crt
/opt/openshift/tls/etcd-client.key
/opt/openshift/tls/etcd-client.crt
...
$ jq -r .systemd.units[].name bootstrap.ign
...
bootkube.service
...
kubelet.service
...
```

Some key points:

* `kubelet` and `bootkube` are the two key services to kick off the
  bootstrapping process.
* The TLS assets in `/opt/openshift/tls/` are used early in the
  bootstrapping process before there is an API server.
* The API resource manifests `/opt/openshift/manifests` are used to
  load the TLS assets once the API server is running.

## Installer

Installer generated assets are represented as a directional acyclic
graph of dependencies. And so, the `Bootstrap` asset depends on the
following assets:

* `EtcdSignerCertKey` - generates the `etcd-signer.{crt,key}` files
  with a self-signed CA cert and key for the `etcd-signer` CA.
* `EtcdCABundle` - the `etcd-signer` CA cert in an
  `etcd-ca-bundle.crt` file.
* `EtcdSignerClientCertKey` - the `etcd-client` cert and key, signed
  by the `etcd-signer` CA.
* `EtcdMetricSignerCertKey`, `EtcdMetricCABundle`,
  `EtcdMetricSignerClientCertKey` - similar to the above set of
  assets, but with the `etcd-metric-signer` CA.

## Bootstrap serving certs

Early in `bootkube` script, the `kube-etcd-signer-server` service is
launched.

This is an implementation of the `etcd-signer` CA from the [kubecsr
project](https://github.com/openshift/kubecsr/tree/openshift-4.7), and
is distinct from the `etcd-signer` controller in CEO.

`kube-etcd-signer-server` is only used during the bootstrap
process. It implements a fake kubernetes API server, solely for the
purpose of handling the bootstrap Certificate Signing Requests (CSR):

```
mux.HandleFunc("/apis/certificates.k8s.io/v1beta1/certificatesigningrequests", server.HandlePostCSR).Methods("POST")
mux.HandleFunc("/apis/certificates.k8s.io/v1beta1/certificatesigningrequests/{csrName}", server.HandleGetCSR).Methods("GET")
```

The service is provided with the `etcd-signer` and
`etcd-metric-signer` CA certs and keys for the purposes of signing
`etcd-servers`, `etcd-peers`, and `etcd-metrics` CSRs.

Later, once the etcd is running, `bootkube` kills
`kube-etcd-signer-server`.

## Bootstrap etcd pod

Immediately after `kube-etcd-signer-server` has been launched,
`bootkube` uses the `cluster-etcd-operator render` to generate some
bootstrap assets into `/etc/openshift/etcd-bootstrap`. One of these is
the bootstrap `etcd-bootstrap-member` static pod manifest which is
copied into `/etc/kubernetes/manifests` causing `kubelet` to launch
the pod.

Many of the details of etcd and grpc-proxy is launched is quite
similar to the standard control plane node etcd static pod
configuration described above. One major difference is that an init
container uses `kube-client-agent` - also from the kubecsr project -
to generate the `etcd-servers`, `etcd-peers`, and `etcd-metrics`
CSRs. These new TLS assets are written to
`/etc/kubernetes/static-pod-resources/etcd-member`.

```
      exec etcd \
        ...
        --cert-file=/etc/ssl/etcd/system:etcd-server:{{ .Hostname }}.crt \
        --key-file=/etc/ssl/etcd/system:etcd-server:{{ .Hostname }}.key \
        --trusted-ca-file=/etc/ssl/etcd/ca.crt \
        --client-cert-auth=true \
        ...
```

* `/etc/kubernetes/static-pod-resources/etcd-member` is mounted into
  the pod at `/etc/ssl/etcd`.
* `cert-file` and `key-file` is the bootstrap node serving cert,
  generated by `kube-client-agent`.
* `trusted-ca-file` is a copy of `etcd-ca-bundle.crt` made by bootkube
  and `client-cert-auth` specifies that clients should be
  authenticated using this CA cert.

## Cluster bootstrap

Once etcd is running, `bootkube` runs the `cluster-bootstrap` command
which will use the earlier-generated bootstrap assets to bring up
other control plane components, such that the Cluster Version Operator
(CVO) launched earlier by `bootkube` can make progress installing
other components.

Eventually, when `cluster-etcd-operator` is installed, running, and
reporting the `EtcdRunningInCluster = True` condition. This is usually
when the etcd cluster has scaled to 4, and it is safe to remove the
bootstrap member.
