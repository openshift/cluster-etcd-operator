package bootstrap

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// IsBootstrapComplete returns true if bootstrap has completed.
//
// This function checks if "bootstrap" config map in "kube-system" namespace
// was created and has status equal to complete.
//
// The configmap is created when bootstrapping is done by the installer (https://github.com/openshift/installer/blob/911203db89a5728f00aaef011b5fb8add7abf646/data/data/bootstrap/files/usr/local/bin/report-progress.sh#L21)
// As of today it is used by etcd and network operators
//
// As of today there is nothing else we could depend on to know if bootstrap was done.
//
// It is important to note that the bootstrap node might not be removed until additional conditions are met.
// For example, on a SNO cluster, the installer waits until the CEO removes the bootstrap member from the etcd cluster.
// In HA clusters, the bootstrap node is torn down as soon as the configmap is created with the appropriate content.
func IsBootstrapComplete(configMapClient corev1listers.ConfigMapLister) (bool, error) {
	bootstrapFinishedConfigMap, err := configMapClient.ConfigMaps("kube-system").Get("bootstrap")
	if err != nil {
		if errors.IsNotFound(err) {
			// If the resource was deleted (e.g. by an admin) after bootstrap is actually complete,
			// this is a false negative.
			klog.V(4).Infof("bootstrap considered incomplete because the kube-system/bootstrap configmap wasn't found")
			return false, nil
		}
		// We don't know, give up quickly.
		return false, fmt.Errorf("failed to get configmap %s/%s: %w", "kube-system", "bootstrap", err)
	}

	if status, ok := bootstrapFinishedConfigMap.Data["status"]; !ok || status != "complete" {
		// do nothing, not torn down
		klog.V(4).Infof("bootstrap considered incomplete because status is %q", status)
		return false, nil
	}

	return true, nil
}
