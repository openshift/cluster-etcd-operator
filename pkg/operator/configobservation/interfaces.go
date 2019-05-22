package configobservation

import (
	corelistersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/cloudprovider"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
)

var _ cloudprovider.InfrastructureLister = &Listers{}

type Listers struct {
	APIServerLister       configlistersv1.APIServerLister
	AuthConfigLister      configlistersv1.AuthenticationLister
	FeatureGateLister_    configlistersv1.FeatureGateLister
	InfrastructureLister_ configlistersv1.InfrastructureLister
	ImageConfigLister     configlistersv1.ImageLister
	NetworkLister         configlistersv1.NetworkLister
	SchedulerLister       configlistersv1.SchedulerLister

	OpenshiftEtcdEndpointsLister corelistersv1.EndpointsLister
	ConfigmapLister              corelistersv1.ConfigMapLister

	ResourceSync       resourcesynccontroller.ResourceSyncer
	PreRunCachesSynced []cache.InformerSynced
}

func (l Listers) FeatureGateLister() configlistersv1.FeatureGateLister {
	return l.FeatureGateLister_
}

func (l Listers) InfrastructureLister() configlistersv1.InfrastructureLister {
	return l.InfrastructureLister_
}

func (l Listers) ResourceSyncer() resourcesynccontroller.ResourceSyncer {
	return l.ResourceSync
}

func (l Listers) PreRunHasSynced() []cache.InformerSynced {
	return l.PreRunCachesSynced
}
