package network

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/configobserver/network"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
)

// ObserveRestrictedCIDRs observes list of restrictedCIDRs.
func ObserveRestrictedCIDRs(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	listers := genericListers.(configobservation.Listers)

	var errs []error
	restrictedCIDRsPath := []string{"admissionPluginConfig", "network.openshift.io/RestrictedEndpointsAdmission", "configuration", "restrictedCIDRs"}

	previouslyObservedConfig := map[string]interface{}{}
	if currentRestrictedCIDRBs, _, err := unstructured.NestedStringSlice(existingConfig, restrictedCIDRsPath...); len(currentRestrictedCIDRBs) > 0 {
		if err != nil {
			errs = append(errs, err)
		}
		if err := unstructured.SetNestedStringSlice(previouslyObservedConfig, currentRestrictedCIDRBs, restrictedCIDRsPath...); err != nil {
			errs = append(errs, err)
		}
	}

	observedConfig := map[string]interface{}{}
	clusterCIDRs, err := network.GetClusterCIDRs(listers.NetworkLister, recorder)
	if err != nil {
		errs = append(errs, err)
		return previouslyObservedConfig, errs
	}
	serviceCIDR, err := network.GetServiceCIDR(listers.NetworkLister, recorder)
	if err != nil {
		errs = append(errs, err)
		return previouslyObservedConfig, errs
	}

	// set observed values
	//  admissionPluginConfig:
	//    network.openshift.io/RestrictedEndpointsAdmission:
	//	  configuration:
	//	    restrictedCIDRs:
	//	    - 10.3.0.0/16 # ServiceCIDR
	//	    - 10.2.0.0/16 # ClusterCIDR
	//  servicesSubnet: 10.3.0.0/16
	restrictedCIDRs := clusterCIDRs
	if len(serviceCIDR) > 0 {
		restrictedCIDRs = append(restrictedCIDRs, serviceCIDR)
	}
	if len(restrictedCIDRs) > 0 {
		if err := unstructured.SetNestedStringSlice(observedConfig, restrictedCIDRs, restrictedCIDRsPath...); err != nil {
			errs = append(errs, err)
		}
	}
	if len(serviceCIDR) > 0 {
		if err := unstructured.SetNestedField(observedConfig, serviceCIDR, "servicesSubnet"); err != nil {
			errs = append(errs, err)
		}
	}

	return observedConfig, errs
}
