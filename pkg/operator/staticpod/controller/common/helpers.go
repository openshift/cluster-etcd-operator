package common

import (
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
)

// NewIsSingleNodePlatformFn returns a function that checks if the cluster topology is single node (aka. SNO)
// In case the err is nil, preconditionFulfilled indicates whether the isSNO is valid.
// If preconditionFulfilled is false, the isSNO return value does not reflect the cluster topology and defaults to the bool default value.
//
// Note:
// usually when preconditionFulfilled is false you should gate your controller as this means we were not able to
// check the current topology
func NewIsSingleNodePlatformFn(infraInformer configv1informers.InfrastructureInformer) func() (isSNO, preconditionFulfilled bool, err error) {
	return func() (isSNO, precheckSucceeded bool, err error) {
		if !infraInformer.Informer().HasSynced() {
			// Do not return transient error
			return false, false, nil
		}
		infraData, err := infraInformer.Lister().Get("cluster")
		if err != nil {
			return false, true, fmt.Errorf("Unable to list infrastructures.config.openshift.io/cluster object, unable to determine topology mode")
		}
		if infraData.Status.ControlPlaneTopology == "" {
			return false, true, fmt.Errorf("ControlPlaneTopology was not set, unable to determine topology mode")
		}

		return infraData.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode, true, nil
	}
}
