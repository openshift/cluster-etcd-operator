package kubescheduler

import (
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
)

// RevisionConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var RevisionConfigMaps = []revision.RevisionResource{
	{Name: "kube-scheduler-pod"},
	{Name: "config"},
	{Name: "serviceaccount-ca"},
	{Name: "policy-configmap", Optional: true},

	{Name: "scheduler-kubeconfig"},
	{Name: "kube-scheduler-cert-syncer-kubeconfig"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	{Name: "serving-cert", Optional: true},
	{Name: "localhost-recovery-client-token"},
}

var CertConfigMaps = []revision.RevisionResource{}

var CertSecrets = []revision.RevisionResource{
	{Name: "kube-scheduler-client-cert-key"},
}
