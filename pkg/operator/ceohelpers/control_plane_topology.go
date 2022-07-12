package ceohelpers

import (
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"k8s.io/klog/v2"
)

const InfrastructureClusterName = "cluster"

func GetControlPlaneTopology(infraLister configv1listers.InfrastructureLister) (configv1.TopologyMode, error) {
	infraData, err := infraLister.Get(InfrastructureClusterName)
	if err != nil {
		klog.Warningf("Failed to get infrastructure resource %s", InfrastructureClusterName)
		return "", err
	}
	if infraData.Status.ControlPlaneTopology == "" {
		return "", fmt.Errorf("ControlPlaneTopology was not set")
	}

	return infraData.Status.ControlPlaneTopology, nil
}

func IsSingleNodeTopology(infraLister configv1listers.InfrastructureLister) (bool, error) {
	topology, err := GetControlPlaneTopology(infraLister)
	if err != nil {
		return false, err
	}

	return topology == configv1.SingleReplicaTopologyMode, nil
}
