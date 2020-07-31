package kubecontrollermanager

import (
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
)

// RevisionConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var RevisionConfigMaps = []revision.RevisionResource{
	{Name: "kube-controller-manager-pod"},

	{Name: "config"},
	{Name: "cluster-policy-controller-config"},
	{Name: "controller-manager-kubeconfig"},
	{Name: "cloud-config", Optional: true},
	{Name: "kube-controller-cert-syncer-kubeconfig"},
	{Name: "serviceaccount-ca"},
	{Name: "service-ca"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	{Name: "service-account-private-key"},

	// this cert is created by the service-ca controller, which doesn't come up until after we are available. this piece of config must be optional.
	{Name: "serving-cert", Optional: true},

	// this needs to be revisioned as certsyncer's kubeconfig isn't wired to be live reloaded, nor will be autorecovery
	{Name: "localhost-recovery-client-token"},
}

var CertConfigMaps = []revision.RevisionResource{
	{Name: "aggregator-client-ca"},
	{Name: "client-ca"},

	// this is a copy of trusted-ca-bundle CM but with key modified to "tls-ca-bundle.pem" so that we can mount it the way we need
	{Name: "trusted-ca-bundle", Optional: true},
}

var CertSecrets = []revision.RevisionResource{
	{Name: "kube-controller-manager-client-cert-key"},
	{Name: "csr-signer"},
}
