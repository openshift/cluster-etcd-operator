package ceohelpers

import (
	configv1 "github.com/openshift/api/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"k8s.io/klog/v2"
)

const infrastructureClusterName = "cluster"

func GetControlPlaneTopology(infraLister configv1listers.InfrastructureLister) (configv1.TopologyMode, error) {
	infraData, err := infraLister.Get(infrastructureClusterName)
	if err != nil {
		klog.Warningf("Failed to get infrastructure resource %s", infrastructureClusterName)
		return "", err
	}
	// Added to make sure that iif ha mode is not set in infrastructure object
	// we will get the default value configv1.FullHighAvailabilityMode
	if infraData.Status.ControlPlaneTopology == "" {
		klog.Infof("HA mode was not set in infrastructure resource setting it to default value %s", configv1.HighlyAvailableTopologyMode)
		infraData.Status.ControlPlaneTopology = configv1.HighlyAvailableTopologyMode
	}

	return infraData.Status.ControlPlaneTopology, nil
}

func IsSingleNodeTopology(infraLister configv1listers.InfrastructureLister) (bool, error) {
	topology, err := GetControlPlaneTopology(infraLister)
	if err != nil {
		return false, nil
	}

	return topology == configv1.SingleReplicaTopologyMode, nil
}
