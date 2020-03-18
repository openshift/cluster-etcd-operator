package configobservercontroller

import (
	"k8s.io/client-go/tools/cache"

	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/library-go/pkg/controller/factory"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type ConfigObserver struct {
	factory.Controller
}

func NewConfigObserver(
	operatorClient v1helpers.OperatorClient,
	operatorConfigInformers operatorv1informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	resourceSyncer resourcesynccontroller.ResourceSyncer,
	eventRecorder events.Recorder,
) *ConfigObserver {
	interestingNamespaces := []string{
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		"openshift-etcd",
		operatorclient.OperatorNamespace,
	}

	configMapPreRunCacheSynced := []cache.InformerSynced{}
	for _, ns := range interestingNamespaces {
		configMapPreRunCacheSynced = append(configMapPreRunCacheSynced, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer().HasSynced)
	}

	informers := []factory.Informer{
		operatorConfigInformers.Operator().V1().Etcds().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer(),
		kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
	}

	for _, ns := range interestingNamespaces {
		informers = append(informers, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer())
	}

	c := &ConfigObserver{
		Controller: configobserver.NewConfigObserver(
			operatorClient,
			eventRecorder,
			configobservation.Listers{
				OpenshiftEtcdEndpointsLister:  kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Lister(),
				OpenshiftEtcdPodsLister:       kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Lister(),
				OpenshiftEtcdConfigMapsLister: kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Lister(),
				NodeLister:                    kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),

				ResourceSync: resourceSyncer,
				PreRunCachesSynced: append(configMapPreRunCacheSynced,
					operatorClient.Informer().HasSynced,
					operatorConfigInformers.Operator().V1().Etcds().Informer().HasSynced,

					kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer().HasSynced,
					kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer().HasSynced,
					kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer().HasSynced,
					kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
				),
			},
			informers,
		),
	}

	return c
}
