package ceohelpers

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// IsArbiterNodeTopology returns if the cluster infrastructure ControlPlaneTopology is set to HighlyAvailableArbiterMode
// We use the infra interface in this situation instead of the lister because typically you are looking to find out this information
// in order to configure controllers before the informers are running.
func IsArbiterNodeTopology(ctx context.Context, infraClient configv1client.InfrastructureInterface) (bool, error) {
	infra, err := infraClient.Get(ctx, InfrastructureClusterName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return infra.Status.ControlPlaneTopology == configv1.HighlyAvailableArbiterMode, nil

}
