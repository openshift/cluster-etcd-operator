package kubeapiserver

import (
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
)

// RevisionConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var RevisionConfigMaps = []revision.RevisionResource{
	{Name: "kube-apiserver-pod"},

	{Name: "config"},
	{Name: "kube-apiserver-cert-syncer-kubeconfig"},
	{Name: "oauth-metadata", Optional: true},
	{Name: "cloud-config", Optional: true},

	// This configmap is managed by the operator, but ensuring a revision history
	// supports signing key promotion. Promotion requires knowing whether the current
	// public key is present in the configmap(s) associated with the current
	// revision(s) of the master nodes.
	{Name: "bound-sa-token-signing-certs"},

	// these need to removed, but if we remove them now, the cluster will die because we don't reload them yet
	{Name: "etcd-serving-ca"},
	{Name: "kube-apiserver-server-ca", Optional: true},
	{Name: "kubelet-serving-ca"},
	{Name: "sa-token-signing-certs"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	// these need to removed, but if we remove them now, the cluster will die because we don't reload them yet
	{Name: "etcd-client"},
	{Name: "kubelet-client"},
	// etcd encryption
	{Name: "encryption-config", Optional: true},

	// this needs to be revisioned as certsyncer's kubeconfig isn't wired to be live reloaded, nor will be autorecovery
	{Name: "localhost-recovery-serving-certkey"},
	{Name: "localhost-recovery-client-token"},

	{Name: "webhook-authenticator", Optional: true},
}

var CertConfigMaps = []revision.RevisionResource{
	{Name: "aggregator-client-ca"},
	{Name: "client-ca"},

	// this is a copy of trusted-ca-bundle CM without the injection annotations
	{Name: "trusted-ca-bundle", Optional: true},

	// kubeconfig that is a system:master.  this ensures a stable location
	{Name: "control-plane-node-kubeconfig"},
}

var CertSecrets = []revision.RevisionResource{
	{Name: "aggregator-client"},
	{Name: "localhost-serving-cert-certkey"},
	{Name: "service-network-serving-certkey"},
	{Name: "external-loadbalancer-serving-certkey"},
	{Name: "internal-loadbalancer-serving-certkey"},
	{Name: "bound-service-account-signing-key"},
	{Name: "control-plane-node-admin-client-cert-key"},

	{Name: "user-serving-cert", Optional: true},
	{Name: "user-serving-cert-000", Optional: true},
	{Name: "user-serving-cert-001", Optional: true},
	{Name: "user-serving-cert-002", Optional: true},
	{Name: "user-serving-cert-003", Optional: true},
	{Name: "user-serving-cert-004", Optional: true},
	{Name: "user-serving-cert-005", Optional: true},
	{Name: "user-serving-cert-006", Optional: true},
	{Name: "user-serving-cert-007", Optional: true},
	{Name: "user-serving-cert-008", Optional: true},
	{Name: "user-serving-cert-009", Optional: true},
}
