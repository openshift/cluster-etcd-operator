# etcd TLS Assets

As of OpenShift 4.4, etcd is deployed on every control plane node as a static pod managed by cluster-etcd-operator (CEO). 
This document describes how the TLS certificates for these static pods are bootstrapped and managed.

**Note:** The current state reflects what the state since 4.16 is, for previous versions please look into the git history.

# Background

To understand this configuration, it's helpful to first review the [etcd transport security model documentation](https://etcd.io/docs/v3.5/op-guide/security/).

# etcd certificates

There are several categories of certificates involved in securing etcd
communications. This section looks at what each of those are and how
to inspect the canonical copy of each on a running cluster.

See also the [user-facing documentation](https://docs.openshift.com/container-platform/4.16/security/certificate-types-descriptions.html#etcd-certificates_ocp-certificates) for these certificates.

## etcd CA summary

All etcd CAs and their CA bundles are stored in the `openshift-etcd` namespace. This NS is considered the source of truth for all certificates.
To share CA bundles and client certificates with consumers (e.g. apiserver or the cluster-etcd-operator) they are copied by the `ResourceSyncController` into different places:

| CA (secret)                                      | CA bundle (configmap)                                         |
|--------------------------------------------------|---------------------------------------------------------------|
| openshift-config/etcd-signer (deprecated)        | openshift-config/etcd-ca-bundle                               |
|                                                  | openshift-etcd/etcd-ca-bundle                                 |
| openshift-etcd/etcd-signer (NEW)                 | openshift-etcd/etcd-ca-bundle                                 |
| openshift-config/etcd-metric-signer (deprecated) | openshift-config/etcd-metric-serving-ca                       |
|                                                  | openshift-etcd/etcd-metrics-ca-bundle                         |
| openshift-etcd/etcd-metric-signer (NEW)          | openshift-config/etcd-metric-serving-ca (to be removed later) |
|                                                  | openshift-etcd/etcd-metrics-ca-bundle                         |

Historically, the certificates were created in the `openshift-config` namespace. All public key bundles are found in configmaps, the private keys are stored in secrets.

## etcd cert summary

All etcd certificates are stored in secrets. 

We're currently transitioning away from `openshift-config` as a source of truth for the signers. As of today, we're using the signers in `openshift-config` to create all certificates, but we're bundling a newly created signer into all required places already. This allows us to switch more easy in the future and sets a definitive stop-date for all existing signer certificates that are about to expire soon.

| CA                                               | Certificate                               | Purpose                          | Certificate copied to                      |
|--------------------------------------------------|-------------------------------------------|----------------------------------|--------------------------------------------|
| openshift-config/etcd-signer (deprecated)        | openshift-etcd/etcd-client                | authn KAS to etcd                | openshift-config                           |
|                                                  |                                           | authn CEO to etcd                | openshift-etcd-operator                    |
|                                                  | openshift-etcd/etcd-peer-$node            | etcd peer communication          | collected in openshift-etcd/etcd-all-certs |
|                                                  | openshift-etcd/etcd-serving-$node         | etcd member serving              | collected in openshift-etcd/etcd-all-certs |
| openshift-config/etcd-metric-signer (deprecated) | openshift-etcd/etcd-metric-client         | authn prometheus to etcd metrics | openshift-etcd-operator/etcd-metric-client |
|                                                  | openshift-etcd/etcd-serving-metrics-$node | etcd member metrics serving      | collected in openshift-etcd/etcd-all-certs |

All signers and certificates are centralized logically in the `CertSignerController` in this repository.

## etcd-signer and etcd-metric-signer CA certs

The cluster hosts two certificate authorities (CA) for etcd - `etcd-signer` for certs relating to client/server and peer/peer communication, 
and `etcd-metric-signer` for certs relating to etcd metrics serving and collection. These certs can only be used for signing.

The certs and associated private keys for these CAs are stored in secrets in the `openshift-etcd` (new) and `openshift-config` (old) namespace. The CA certs alone also stored several `ca-bundle` config maps.

```
$ oc get -n openshift-config secret/etcd-signer -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 5666804984547586656 (0x4ea4891c035f3260)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-config_etcd-signer@1706867612
        Validity
            Not Before: Feb  2 09:53:31 2024 GMT
            Not After : Jan 31 09:53:32 2029 GMT
        Subject: CN = openshift-config_etcd-signer@1706867612
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
                Public-Key: (2048 bit)
...                
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier: 
                AD:5A:0E:AE:36:68:A2:84:BE:9A:0C:D1:B3:6C:2B:B4:BE:63:AF:9C
            X509v3 Authority Key Identifier: 
                AD:5A:0E:AE:36:68:A2:84:BE:9A:0C:D1:B3:6C:2B:B4:BE:63:AF:9C
...

$ oc get -n openshift-etcd secret/etcd-signer -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 1882009046775752485 (0x1a1e3d41b923e725)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-etcd_etcd-signer@1706867612
        Validity
            Not Before: Feb  2 09:53:31 2024 GMT
            Not After : Jan 31 09:53:32 2029 GMT
        Subject: CN = openshift-etcd_etcd-signer@1706867612
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
                Public-Key: (2048 bit)
...
       X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier: 
                51:69:98:F9:35:D2:9E:B8:90:2F:38:EA:C1:07:62:22:26:B6:58:89
            X509v3 Authority Key Identifier: 
                51:69:98:F9:35:D2:9E:B8:90:2F:38:EA:C1:07:62:22:26:B6:58:89
...


$ oc get -n openshift-etcd configmap/etcd-ca-bundle -o template='{{index .data "ca-bundle.crt"}}'  | openssl x509 -noout -ext subjectKeyIdentifier
X509v3 Subject Key Identifier: 
    51:69:98:F9:35:D2:9E:B8:90:2F:38:EA:C1:07:62:22:26:B6:58:89

$ oc get -n openshift-etcd configmap/etcd-serving-ca -o template='{{index .data "ca-bundle.crt"}}'  | openssl x509 -noout -ext subjectKeyIdentifier
X509v3 Subject Key Identifier: 
    51:69:98:F9:35:D2:9E:B8:90:2F:38:EA:C1:07:62:22:26:B6:58:89
```

```
$ oc get -n openshift-etcd secret/etcd-metric-signer -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 8949989853081977106 (0x7c34c05a304d4112)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-etcd_etcd-metric-signer@1706867612
        Validity
            Not Before: Feb  2 09:53:32 2024 GMT
            Not After : Jan 31 09:53:33 2029 GMT
        Subject: CN = openshift-etcd_etcd-metric-signer@1706867612
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier: 
                5F:C6:96:F4:CA:D6:AE:41:ED:96:0E:94:F0:00:44:4D:FC:CD:20:63
            X509v3 Authority Key Identifier: 
                5F:C6:96:F4:CA:D6:AE:41:ED:96:0E:94:F0:00:44:4D:FC:CD:20:63
...

$ oc get -n openshift-etcd configmap/etcd-metrics-ca-bundle -o template='{{index .data "ca-bundle.crt"}}'  | openssl x509 -noout -ext subjectKeyIdentifier
X509v3 Subject Key Identifier: 
    5F:C6:96:F4:CA:D6:AE:41:ED:96:0E:94:F0:00:44:4D:FC:CD:20:63

```

You can see that the CN also contains the namespace, which makes them easy to distinguish.

## etcd-client and etcd-metric-client certs

The `etcd-signer` CA issues an `etcd-client` cert that `kube-apiserver` and `cluster-etcd-operator` uses to authenticate to etcd. 
This is stored in the `openshift-etcd` namespace and copied into the `openshift-config` and `cluster-etcd-operator` namespace.

```
$ oc get -n openshift-etcd secret/etcd-client -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 3286966950741554736 (0x2d9da706cf663630)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-config_etcd-signer@1706867612
        Validity
            Not Before: Feb  2 09:53:31 2024 GMT
            Not After : Feb  1 09:53:32 2027 GMT
        Subject: O = etcd-client + O = system:etcd, CN = etcd-client
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage: 
                TLS Web Client Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Authority Key Identifier: 
                AD:5A:0E:AE:36:68:A2:84:BE:9A:0C:D1:B3:6C:2B:B4:BE:63:AF:9C
...
```

Similarly, the `etcd-metric-signer` CA issues a client cert that prometheus uses when authenticating with the etcd metrics server. 
Since the use of `ServiceEndpoints` in 4.15, the metric related certificates outside of `openshift-etcd` are just copies and unused.

```
$ oc get -n openshift-etcd secret/etcd-metric-client -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 2861583867292788305 (0x27b6635db0f9a651)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-config_etcd-metric-signer@1706867612
        Validity
            Not Before: Feb  2 09:53:32 2024 GMT
            Not After : Feb  1 09:53:33 2027 GMT
        Subject: O = etcd-metric + O = system:etcd, CN = etcd-metric
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage: 
                TLS Web Client Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Authority Key Identifier: 
                2C:EF:FD:79:FB:7F:70:B0:21:62:CC:06:C1:F1:F1:F2:EA:B8:D4:D2
...
```

Note that those certificates are signed by the CA's from the `openshift-config` namespace, this is to ensure backward compatibility on upgrades. 

## etcd-serving certs

Each control plane node is issued a serving cert (by the `etcd-signer` CA) to secure the client-server communication on port 2379. This cert 
(and its private key) is stored in an `etcd-serving-$node` secret in the `openshift-etcd` namespace.

```
$ oc get -n openshift-etcd secret/etcd-serving-master-0 -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 7627369043925387108 (0x69d9dbbbe1fbc764)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-config_etcd-signer@1706867612
        Validity
            Not Before: Feb  2 10:02:59 2024 GMT
            Not After : Feb  1 10:03:00 2027 GMT
        Subject: CN = 10.0.0.4
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage: 
                TLS Web Client Authentication, TLS Web Server Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier: 
                AD:CC:6A:00:4F:6F:94:FC:46:B9:49:9C:9E:9C:72:AA:FA:30:90:81
            X509v3 Authority Key Identifier: 
                AD:5A:0E:AE:36:68:A2:84:BE:9A:0C:D1:B3:6C:2B:B4:BE:63:AF:9C
            X509v3 Subject Alternative Name: 
                DNS:etcd.kube-system.svc, DNS:etcd.kube-system.svc.cluster.local, DNS:etcd.openshift-etcd.svc, DNS:etcd.openshift-etcd.svc.cluster.local, DNS:localhost, DNS:10.0.0.4, DNS:127.0.0.1, DNS:::1, IP Address:10.0.0.4, IP Address:127.0.0.1, IP Address:0:0:0:0:0:0:0:1

```

Note that the `Subject Alternative Name` field lists the various ways that a client might connect to etcd.

Note that this serving cert can also be used as a client cert, as required by etcd, but we're never using it as such in OpenShift.

## etcd-peer certs

Similarly, each control plane node is issued a certificate (again by the `etcd-signer` CA) to secure the peer-to-peer communication on port 2380.
This is stored in an `etc-peer-$node` secret.

```
$ oc get -n openshift-etcd secret/etcd-peer-master-0 -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 7675552765032453621 (0x6a850a92ae86d5f5)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-config_etcd-signer@1706867612
        Validity
            Not Before: Feb  2 10:02:59 2024 GMT
            Not After : Feb  1 10:03:00 2027 GMT
        Subject: CN = 10.0.0.4
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage: 
                TLS Web Client Authentication, TLS Web Server Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier: 
                B0:DA:CC:DB:8E:7C:99:90:EA:06:DD:2F:15:32:58:A7:74:27:59:4B
            X509v3 Authority Key Identifier: 
                AD:5A:0E:AE:36:68:A2:84:BE:9A:0C:D1:B3:6C:2B:B4:BE:63:AF:9C
            X509v3 Subject Alternative Name: 
                DNS:etcd.kube-system.svc, DNS:etcd.kube-system.svc.cluster.local, DNS:etcd.openshift-etcd.svc, DNS:etcd.openshift-etcd.svc.cluster.local, DNS:localhost, DNS:10.0.0.4, DNS:127.0.0.1, DNS:::1, IP Address:10.0.0.4, IP Address:127.0.0.1, IP Address:0:0:0:0:0:0:0:1
...
```

Note that this cert can also be for client authentication - i.e. peers use this to authenticate when connecting to another peer.

## etcd-serving-metrics certs

Each control plane node is also issued a certificate (by the `etcd-metric-signer` CA) for serving metrics on port 9979. It is stored in the `etcd-serving-metrics-$node` secret.

```
$ oc get -n openshift-etcd secret/etcd-serving-metrics-master-0 -o template='{{index .data "tls.crt"}}'  | base64 -d | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 2805443370039336688 (0x26eeefe194ab5ef0)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-config_etcd-metric-signer@1706867612
        Validity
            Not Before: Feb  2 10:02:59 2024 GMT
            Not After : Feb  1 10:03:00 2027 GMT
        Subject: CN = 10.0.0.4
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment
            X509v3 Extended Key Usage: 
                TLS Web Client Authentication, TLS Web Server Authentication
            X509v3 Basic Constraints: critical
                CA:FALSE
            X509v3 Subject Key Identifier: 
                7D:B1:A5:F8:55:37:8F:1D:5A:03:8F:19:F9:59:49:D8:E1:49:EC:C9
            X509v3 Authority Key Identifier: 
                2C:EF:FD:79:FB:7F:70:B0:21:62:CC:06:C1:F1:F1:F2:EA:B8:D4:D2
            X509v3 Subject Alternative Name: 
                DNS:etcd.kube-system.svc, DNS:etcd.kube-system.svc.cluster.local, DNS:etcd.openshift-etcd.svc, DNS:etcd.openshift-etcd.svc.cluster.local, DNS:localhost, DNS:10.0.0.4, DNS:127.0.0.1, DNS:::1, IP Address:10.0.0.4, IP Address:127.0.0.1, IP Address:0:0:0:0:0:0:0:1
...
```

## Operator Service CA

You may notice an
`openshift-etcd-operator/configmap/etcd-service-ca-bundle` resource:

```
$ oc get -n openshift-etcd-operator configmap/etcd-service-ca-bundle  -o template='{{index .data "service-ca.crt"}}' | openssl x509 -noout -text
Certificate:
    Data:
        Version: 3 (0x2)
        Serial Number: 8395602991184334199 (0x74832c704cbfcd77)
        Signature Algorithm: sha256WithRSAEncryption
        Issuer: CN = openshift-service-serving-signer@1706868181
        Validity
            Not Before: Feb  2 10:03:00 2024 GMT
            Not After : Apr  2 10:03:01 2026 GMT
        Subject: CN = openshift-service-serving-signer@1706868181
        Subject Public Key Info:
            Public Key Algorithm: rsaEncryption
...
        X509v3 extensions:
            X509v3 Key Usage: critical
                Digital Signature, Key Encipherment, Certificate Sign
            X509v3 Basic Constraints: critical
                CA:TRUE
            X509v3 Subject Key Identifier: 
                08:BA:46:C1:AA:F3:BE:F3:25:09:EA:B5:36:92:86:B1:4F:D0:12:82
            X509v3 Authority Key Identifier: 
                08:BA:46:C1:AA:F3:BE:F3:25:09:EA:B5:36:92:86:B1:4F:D0:12:82
...
```

This is the cert for the service CA implemented by the [service-ca-operator](https://github.com/openshift/service-ca-operator) component.

The CEO uses the service CA to issue a serving cert for the CEO's own metric serving. See the `openshift-etcd-operator/service/metrics` service:

```
$ oc get -n openshift-etcd-operator service/metrics -o template='{{range $k, $v := .metadata.annotations}}{{printf "%s = %s\n" $k $v}}{{end}}'
include.release.openshift.io/self-managed-high-availability = true
include.release.openshift.io/single-node-developer = true
service.alpha.openshift.io/serving-cert-secret-name = etcd-operator-serving-cert
service.alpha.openshift.io/serving-cert-signed-by = openshift-service-serving-signer@1706868181
service.beta.openshift.io/serving-cert-signed-by = openshift-service-serving-signer@1706868181

$ oc get -n openshift-etcd-operator secret/etcd-operator-serving-cert -o template='{{index .data "tls.crt"}}' | base64 -d | openssl x509 -noout -issuer -subject
issuer=CN = openshift-service-serving-signer@1706868181
subject=CN = metrics.openshift-etcd-operator.svc
```

Note the `openshift-etcd/services/etcd` resource is similarly annotated but the `openshift-etcd/secret/serving-cert` appears unused.

# etcd static pods configuration

The etcd static pod on each control plane node runs a number of containers - see `/etc/kubernetes/manifests/etcd-pod.yaml` - but etcd itself is executed with the following arguments:

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

- etcd clients are served on port 2379, peer etcd cluster members are served on port 2380, and prometheus metrics are served on port 9978
- `--cert-file` and `--key-file` specify the `etcd-serving` cert and key for client-to-server communication (on port 2379) with this instance of etcd. 
  Each node has its own serving cert; in this case the node name is `master-0`.
- Note that the metrics serving on port 9978 is secured with this same serving cert, but it is then proxied by `grpc-proxy` as described below.
- `--client-cert-auth=true` and `--trusted-ca-file` specify that clients must authenticate with a certificate signed the `etcd-signer` CA cert in the supplied CA bundle.
- `--peer-cert-file` and `--peer-key-file` specify the serving cert and key for peer-to-peer communication (on port 2380). Again, each node has its own serving cert.
- `--peer-client-cert-auth=true` and `-peer-trusted-ca-file` enables certificate based authentication of connections from etcd peers, but it's again the `etcd-signer` CA that is trusted.

Also in the etcd pod, a `grpc-proxy` instance is launched which will serve prometheus metrics on port 9979. This is secured as follows:

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

- `--endpoint ... :9978` specifies that we're proxying etcd's metrics serving.
- `--metrics-addr ... :9979` the proxied metrics serving will be on port 9979.
- `--cert`, `--key`, `--cacert` configures the proxy's TLS client with this node's `etcd-peer` cert to connect to etcd.
- `--cert-file`, `--key-file` configures the proxy's metrics server to use the etcd-serving cert for this node.
- `--trusted-ca-file` enables client authentication for the grpc proxy 
  - the `etcd-metric-signer` CA is trusted.

It can be helpful to inspect these certs on disk on a control plane machine. They are stored in the `/etc/kubernetes/static-pod-resources/etcd-certs` which is mounted into the etcd pod under `/etc/kubernetes/static-pod-certs`.

```
$ chroot /host
$ cd /etc/kubernetes/static-pod-resources/etcd-certs
$ for crt in $(find . -name "*.crt"); do echo -n "$crt: "; openssl x509 -noout -subject < $crt; done
./secrets/etcd-all-certs/etcd-serving-ci-ln-lzvwz92-72292-9l99x-master-2.crt: subject=CN = 10.0.0.5
./secrets/etcd-all-certs/etcd-peer-ci-ln-lzvwz92-72292-9l99x-master-0.crt: subject=CN = 10.0.0.4
./secrets/etcd-all-certs/etcd-serving-metrics-ci-ln-lzvwz92-72292-9l99x-master-0.crt: subject=CN = 10.0.0.4
./secrets/etcd-all-certs/etcd-peer-ci-ln-lzvwz92-72292-9l99x-master-1.crt: subject=CN = 10.0.0.3
./secrets/etcd-all-certs/etcd-serving-ci-ln-lzvwz92-72292-9l99x-master-1.crt: subject=CN = 10.0.0.3
./secrets/etcd-all-certs/etcd-peer-ci-ln-lzvwz92-72292-9l99x-master-2.crt: subject=CN = 10.0.0.5
./secrets/etcd-all-certs/etcd-serving-metrics-ci-ln-lzvwz92-72292-9l99x-master-2.crt: subject=CN = 10.0.0.5
./secrets/etcd-all-certs/etcd-serving-ci-ln-lzvwz92-72292-9l99x-master-0.crt: subject=CN = 10.0.0.4
./secrets/etcd-all-certs/etcd-serving-metrics-ci-ln-lzvwz92-72292-9l99x-master-1.crt: subject=CN = 10.0.0.3
./configmaps/etcd-metrics-proxy-client-ca/ca-bundle.crt: subject=CN = openshift-etcd_etcd-metric-signer@1706881792
./configmaps/etcd-metrics-proxy-serving-ca/ca-bundle.crt: subject=CN = openshift-etcd_etcd-metric-signer@1706881792
./configmaps/etcd-peer-client-ca/ca-bundle.crt: subject=CN = openshift-etcd_etcd-signer@1706881791
./configmaps/etcd-serving-ca/ca-bundle.crt: subject=CN = openshift-etcd_etcd-signer@1706881791
```

As expected, there are the two CA certs (`etcd-signer`, `etcd-metric-signer`) and three serving certs (`etcd-server`, `etcd-peer`, `etcd-metric`) and each one for every control plane node.

# cluster-etcd-operator

The `cluster-etcd-operator` component is what manages this static pod configuration and the TLS certificates.

## Static pod controller

To manage the etcd static pods, `cluster-etcd-operator` uses OpenShift's [static pod operator
library](https://github.com/openshift/library-go/blob/master/pkg/operator/staticpod/controllers.go) and reconciles a custom resource based on the [`StaticPodOperator`](https://github.com/openshift/api/blob/89de68875e7c763b0cb327ab68bf93af1e4c2f61/operator/v1/types.go#L165-L214)
type. The `kube-apisever`, `kube-scheduler`, and `kube-controller-manager` static pods are managed the same way.

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

- The `spec.managementState` field is usually set to `Managed`; setting it to `Unmanaged` or `Removed` has the effect of disabling 
  many of the controllers described below.
- The conditions give helpful information about the state of the etcd cluster - in the example above it shows that all the nodes are
  running the same static pod configuration, and all 3 etcd cluster members are healthy.
- The node statuses show in detail which configuration revision is installed on each node - in this case, all nodes are running the
  latest revision.

The below sections briefly discuss the role of each controller in the static pod operator library.

### Node controller

Ensures that any node with the `node-role.kubernetes.io/master` label has an entry in the `status.nodeStatuses` list. The other controllers
use this list to iterate over all control plane nodes.

### Revision controller

This controller watches a [set of config maps and secrets](https://github.com/openshift/cluster-etcd-operator/blob/0ecd5d2b72df7648769b8625a35de5e792cf707d/pkg/operator/starter.go#L260-L277) for changes. 
Any change to those resources constitute a new numbered "revision".

When a new revision is detected, copies of all these resources are created with the revision number as a name suffix. Resources may be
flagged as `optional`, in which case their non-existance will not be treated as an error. The new revision is then recorded in the `status.latestAvailableRevision` field and a `revision-$suffix` configmap is created to record the status (`In Progress`, `Succeeded`, `Failed`, `Abandoned`) of the installation of this revision.

### Installer controller

When a new revision is available, a pod is launched on each node to read the copied resources for this revision and write those to disk -
with some basic templating - to a revision-specific directory like `/etc/kubernetes/static-pod-resources/etcd-pod-$revision` (based on
the name of the static pod config map).

The installer is also asked to write out an additional [set of resources for certs and keys](https://github.com/openshift/cluster-etcd-operator/blob/0ecd5d2b72df7648769b8625a35de5e792cf707d/pkg/operator/starter.go#L279-L294)
for which revision-suffixed copies aren't created. These are written to an unrevisioned directory, in this case `/etc/kubernetes/static-pod-resources/etcd-certs`.

In the case of etcd, `cluster-etcd-operator installer` is the installer command executed, and it is supplied with the details of the
resources to copy and their on-disk destination for. The most significant of these resources is the manifest for the static pod
itself, which gets written to `/etc/kubernetes/manifests` causing kubelet to launch this new revision of the pod.

A separate controller - the backing resource controller - is responsible for ensuring the existence of an `installer-sa` service
account with the `cluster-admin` role. This is the service account under which the installer (and pruner) pods runs.

The revisioned resources destined for `/etc/kubernetes/static-pod-resources/etcd-pod-$revision` for etcd are:

* Config maps
    - `etcd-pod` - the first element in the list has a special meaning it is the static pod itself, and is to be installed in
      `/etc/kubernetes/manifests`.
    - `config` - an `EtcdConfig` file
    - `etcd-serving-ca`, `etcd-peer-client-ca`, `etcd-metrics-proxy-serving-ca`, `etcd-metrics-proxy-client-ca`
      - CA certs described above, copied from the `openshift-config` namespace by the resource sync controller described below.
* Secrets:
    - `etcd-all-certs` includes all certs for all nodes in a single secret to reduce the complexity of managing the resources to watch. 
      The `etcd-signer` controller maintains both the `etcd-all-certs` secret and the node- and type-specific cert secrets it aggregates.

The unrevisioned resources destined for
`/etc/kubernetes/static-pod-resources/etcd-certs` are:

* Config maps:
    - `etcd-scripts`, `restore-etcd-pod` - etcd backup and restore scripts.
    - `etcd-serving-ca`, `etcd-peer-client-ca`, `etcd-metrics-proxy-serving-ca`, `etcd-metrics-proxy-client-ca` - as above.
* Secrets:
    - `etcd-all-certs` as above.

Note it appears we [only use the unrevisioned copies](https://github.com/openshift/cluster-etcd-operator/pull/217) of all these certs and secrets.

### Prune controller

Launches a pruner pod - again under the `installer-sa` service account - on each node that runs `cluster-etcd-operator prune` to
delete old revisions from disk. Also deletes old revisions of the API resources.

Retains a configurable number of revisions that have been successfully installed, another set of failed revisions, along with any revision in other states.

## etcd cert signer

The cluster-hosted `etcd-signer` CA (and the `etcd-metrics-signer` CA) is implemented by another control loop in CEO, separate from the
`StaticPodOperator` control loops described above.

The cert signer controller is the source of truth for all certificates and implements rotation and creation of all required certificates for etcd.

It's primary function however is to iterate across all control plane nodes (i.e. those labelled with `node-role.kubernetes.io/master`), creating
a secrets for each node's `etcd-serving-$node`, `etcd-peer-$node`, and `etcd-metrics-serving-$node` certificates in the `openshift-etcd`
namespace. It also creates the `etcd-all-certs` combined secret. Those certificates are called "dynamically created" certificates.


## Resource sync controller

A CEO controller, separate from the `StaticPodOperator` controllers above, copies a bunch of config maps and secrets from the `openshift-etcd` namespace to either the `openshift-config` or `openshift-etcd-operator` namespace. The list of resources copied are:

- `openshift-etcd/configmaps/etcd-ca-bundle` to operator and config NS
- `openshift-etcd/configmaps/etcd-ca-bundle` to `openshift-etcd/configmaps/etcd-peer-client-ca`
- `openshift-etcd/configmaps/etcd-ca-bundle`  to `openshift-etcd/configmaps/etcd-serving-ca`
- `openshift-etcd/configmaps/etcd-metric-ca-bundle` to `openshift-etcd/configmaps/etcd-metric-serving-ca`, `openshift-etcd/configmaps/etcd-metrics-proxy-serving-ca` and `openshift-etcd/configmaps/etcd-metrics-proxy-client-ca`
 secrets


The etcd client certificates secrets (`etcd-client`) are copied into the `cluster-etcd-operator` and `openshift-config` namespace for sharing.  

# Bootstrap Process

The above sections described the TLS assets, etcd static pods, and related CEO control loops on a running cluster. 
This section considers how this configuration was initially bootstrapped on the bootstrap machine.

Generally the bootstrap process is implemented in the installer, but it will run component-specific code in each of the operator codebases. 
CEO implements all of the certificate related bootstrap in the cmdline function `render`.


## Bootstrap Ignition

To get oriented with the bootstrap ignition process, you can browse the list of files and systemd units in the bootstrap ignition file:

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

* `kubelet` and `bootkube` are the two key services to kick off the bootstrapping process.
* The TLS assets in `/opt/openshift/tls/` are used early in the bootstrapping process before there is an API server.
* The API resource manifests `/opt/openshift/manifests` are used to load the TLS assets once the API server is running.

## Bootstrap serving certs

// TODO with render update PR

## CSR Signing

Early in `bootkube` script, the `kube-etcd-signer-server` service is launched.

This is an implementation of the `etcd-signer` CA from the [kubecsr project](https://github.com/openshift/kubecsr/tree/openshift-4.16), and
is distinct from the `etcd-signer` controller in CEO.

`kube-etcd-signer-server` is only used during the bootstrap process. It implements a fake kubernetes API server, solely for the
purpose of handling the bootstrap Certificate Signing Requests (CSR):

```
mux.HandleFunc("/apis/certificates.k8s.io/v1beta1/certificatesigningrequests", server.HandlePostCSR).Methods("POST")
mux.HandleFunc("/apis/certificates.k8s.io/v1beta1/certificatesigningrequests/{csrName}", server.HandleGetCSR).Methods("GET")
```

The service is provided with the `etcd-signer` and `etcd-metric-signer` CA certs and keys for the purposes of signing
`etcd-servers`, `etcd-peers`, and `etcd-metrics` CSRs. Later, once the etcd is running, `bootkube` kills `kube-etcd-signer-server`.

## Bootstrap etcd pod

Immediately after `kube-etcd-signer-server` has been launched, `bootkube` uses the `cluster-etcd-operator render` to generate all 
certificate-related assets into `/etc/openshift/etcd-bootstrap`. One of these is the bootstrap `etcd-bootstrap-member` static pod 
manifest, which is copied into `/etc/kubernetes/manifests` causing `kubelet` to launch the pod.

Many of the details of etcd and grpc-proxy is launched is quite similar to the standard control plane node etcd static pod
configuration described above. One major difference is that an init container uses `kube-client-agent` - also from the kubecsr project -
to generate the `etcd-servers`, `etcd-peers`, and `etcd-metrics` CSRs. These new TLS assets are written to `/etc/kubernetes/static-pod-resources/etcd-member`.

```
      exec etcd \
        ...
        --cert-file=/etc/ssl/etcd/system:etcd-server:{{ .Hostname }}.crt \
        --key-file=/etc/ssl/etcd/system:etcd-server:{{ .Hostname }}.key \
        --trusted-ca-file=/etc/ssl/etcd/ca.crt \
        --client-cert-auth=true \
        ...
```

* `/etc/kubernetes/static-pod-resources/etcd-member` is mounted into the pod at `/etc/ssl/etcd`.
* `cert-file` and `key-file` is the bootstrap node serving cert, generated by `kube-client-agent`.
* `trusted-ca-file` is a copy of `etcd-ca-bundle.crt` made by bootkube and `client-cert-auth` specifies 
  that clients should be authenticated using this CA cert.

## Cluster bootstrap

Once etcd is running, `bootkube` runs the `cluster-bootstrap` command which will use the earlier-generated bootstrap assets to bring up
other control plane components, such that the Cluster Version Operator (CVO) launched earlier by `bootkube` can make progress installing
other components.

# Cert Rotation

With 4.16 we want to take an initial stab at the rotation of the signer certificates. 
Below rotation procedure do not invalidate any existing KCS articles, they are merely overviews for (support) engineers to understand the process. 

In general the process depends on _what_ needs to be rotated, signers have a slightly more complicated rotation procedure due to their dependencies to many certificates. Leaving the signer untouched, most certificates can be rotated fairly easily.

## Pre-4.16

### Certificates

Deleting any of the dynamically generated certificates (peer, serving, metrics) will re-generate automatically through the operator by simply deleting the respective secret:

```
$ oc delete secret -n openshift-etcd etcd-peer-master-0
```

This will trigger a newly issued certificate for the master-0 peer, which will then rollout via static pods.
The operator will automatically rotate those certificate if it reaches 20% remaining lifetime of three years.

For all other certificates (e.g. the clients) the rotation must be done manually using openssl or the go code in the cluster-etcd-operator. 

### Signers

See [KCS-7003549](https://access.redhat.com/solutions/7003549) and [ETCD-445](https://issues.redhat.com/browse/ETCD-445). 

## Since 4.16

### Certificates

As before, deleting any of the dynamically generated certificates (peer, serving, metrics) will re-generate automatically through the operator by simply deleting the respective secret:

```
$ oc delete secret -n openshift-etcd etcd-peer-master-0
```

In addition, the client certificates can be rotated in similar fashion:

```
$ oc delete secret -n openshift-etcd etcd-client
```

Client rotation will trigger additional rollouts/restarts, e.g. on the cluster-etcd-operator or the kube-apiserver. Metrics client cert updates are handled by Prometheus dynamically and do not require rollouts/restarts.

Auto-rotation before expiry is still supported, certificates will rotate once they reach 30 months (or 2.5 years) of their 3 year lifetime.

### Signers

TODO(thomas): highly experimental

(Disclaimer: this is subject to change over the course of the 4.16 dev cycle)

The CA signers used to sign the certificates are still read from `openshift-config` for backward compatibility reasons. 
Starting with 4.16, we're creating a new signer CA in `openshift-etcd` which is _not_ used to sign anything, 
but is bundled together with the existing signers from `openshift-config`.

This ensures that when we're rotating to the new signer, the rollout will function flawlessly and not 
create a crash looping etcd peer because the certificates where signed by an unknown CA.

On the happy path, the rotation ceremony is a simple copy from `openshift-etcd` to `openshift-config`:

```
$ oc get secret etcd-signer -n openshift-etcd -ojson | jq 'del(.metadata["namespace","creationTimestamp","resourceVersion","selfLink","uid"])' | oc apply -n openshift-config -f -
```

You should see the `cluster-etcd-operator` detecting the signer has changed, which will recreate all existing certificates that depend on it.

Finally, rotate the surrogate signer in `openshift-etcd` again for the next time:

```
$ oc delete secret etcd-signer -n openshift-etcd
```

You can observe that the ca-bundle now contains three certificates:

```
$ oc get configmap -n openshift-config -oyaml etcd-serving-ca
```

Still sketchy with the signers in openshift-etcd: rotation based on expiration is supported now, signers will rotate once they reach 54 months (or 4.5 years) of their 5 year lifetime. Needs to be figured out before we release 4.16.

## Plan for automated signer rotation

The whole process can be split into a two-step migration:
1. Operator must manage all etcd certificates [CEO#1194](https://github.com/openshift/cluster-etcd-operator/pull/1177) 
   1. Operator should create new signers, in addition to the current ones in openshift-config. Both bundled and distributed.  
      This allows us to stop the "death clock" on any existing signer expiration and allowing manual rotation immediately.
   2. Having the rotation code already in place also enables easier offline rotation [ETCD-510](https://issues.redhat.com/browse/ETCD-510)
2. Operator must orchestrate an automated signer rotation process (> 4.16)
   1. A new signer needs to be generated, distributed via bundle and static pod rollout
   2. All certificates must be re-generated using the new signer
   3. All consumers of client certs (Prometheus, CEO, KAS) need to be notified or must react correctly to a rotated certificate secret

With step (1), we want to move the source-of-truth for etcd certificates into the `openshift-etcd` namespace. This should 
help in future backup and restore stories, allowing us to more easily collect all certificates required for etcd and guard 
them better by RBAC.
