package configobservation

import (
	corelistersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
)

type Listers struct {
	OpenshiftEtcdEndpointsLister  corelistersv1.EndpointsLister
	OpenshiftEtcdPodsLister       corelistersv1.PodLister
	OpenshiftEtcdConfigMapsLister corelistersv1.ConfigMapLister
	NodeLister                    corelistersv1.NodeLister

	ResourceSync       resourcesynccontroller.ResourceSyncer
	PreRunCachesSynced []cache.InformerSynced
}

func (l Listers) ResourceSyncer() resourcesynccontroller.ResourceSyncer {
	return l.ResourceSync
}

func (l Listers) PreRunHasSynced() []cache.InformerSynced {
	return l.PreRunCachesSynced
}
