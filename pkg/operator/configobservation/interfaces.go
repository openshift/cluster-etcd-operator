package configobservation

import (
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	corelistersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
)

type Listers struct {
	APIServerLister_                      configlistersv1.APIServerLister
	OpenshiftEtcdEndpointsLister          corelistersv1.EndpointsLister
	OpenshiftEtcdPodsLister               corelistersv1.PodLister
	OpenshiftEtcdConfigMapsLister         corelistersv1.ConfigMapLister
	NodeLister                            corelistersv1.NodeLister
	ConfigMapListerForKubeSystemNamespace corelistersv1.ConfigMapNamespaceLister

	ResourceSync       resourcesynccontroller.ResourceSyncer
	PreRunCachesSynced []cache.InformerSynced
}

func (l Listers) APIServerLister() configlistersv1.APIServerLister {
	return l.APIServerLister_
}

func (l Listers) ResourceSyncer() resourcesynccontroller.ResourceSyncer {
	return l.ResourceSync
}

func (l Listers) PreRunHasSynced() []cache.InformerSynced {
	return l.PreRunCachesSynced
}
