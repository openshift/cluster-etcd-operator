package etcd

import (
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
)

// RevisionConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var RevisionConfigMaps = []revision.RevisionResource{
	{Name: "etcd-pod"},

	{Name: "config"},
	{Name: "etcd-serving-ca"},
	{Name: "etcd-peer-client-ca"},
	{Name: "etcd-metrics-proxy-serving-ca"},
	{Name: "etcd-metrics-proxy-client-ca"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	{Name: "etcd-all-peer"},
	{Name: "etcd-all-serving"},
	{Name: "etcd-all-serving-metrics"},
}

var CertConfigMaps = []revision.RevisionResource{
	{Name: "restore-etcd-pod"},
	{Name: "etcd-scripts"},
	{Name: "etcd-serving-ca"},
	{Name: "etcd-peer-client-ca"},
	{Name: "etcd-metrics-proxy-serving-ca"},
	{Name: "etcd-metrics-proxy-client-ca"},
}

var CertSecrets = []revision.RevisionResource{
	// these are also copied to certs to have a constant file location so we can refer to them in various recovery scripts
	// and in the PDB
	{Name: "etcd-all-peer"},
	{Name: "etcd-all-serving"},
	{Name: "etcd-all-serving-metrics"},
}
