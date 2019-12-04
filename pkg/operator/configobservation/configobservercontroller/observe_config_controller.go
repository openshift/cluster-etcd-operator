package configobservercontroller

import (
	"k8s.io/client-go/tools/cache"

	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/etcd"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type ConfigObserver struct {
	*configobserver.ConfigObserver
}

func NewConfigObserver(
	operatorClient v1helpers.OperatorClient,
	operatorConfigInformers operatorv1informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	configInformer configinformers.SharedInformerFactory,
	resourceSyncer resourcesynccontroller.ResourceSyncer,
	eventRecorder events.Recorder,
) *ConfigObserver {
	interestingNamespaces := []string{
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
	}

	configMapPreRunCacheSynced := []cache.InformerSynced{}
	for _, ns := range interestingNamespaces {
		configMapPreRunCacheSynced = append(configMapPreRunCacheSynced, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer().HasSynced)
	}

	c := &ConfigObserver{
		ConfigObserver: configobserver.NewConfigObserver(
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
			//TODO enable after migratation from KAO
			etcd.ObserveStorageURLs,
			etcd.ObserveClusterMembers,
			etcd.ObservePendingClusterMembers,
		),
	}

	operatorConfigInformers.Operator().V1().Etcds().Informer().AddEventHandler(c.EventHandler())

	for _, ns := range interestingNamespaces {
		kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer().AddEventHandler(c.EventHandler())
	}
	kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer().AddEventHandler(c.EventHandler())
	kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer().AddEventHandler(c.EventHandler())
	kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer().AddEventHandler(c.EventHandler())
	kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer().AddEventHandler(c.EventHandler())

	return c
}
