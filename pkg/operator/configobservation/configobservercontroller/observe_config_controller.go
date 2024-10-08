package configobservercontroller

import (
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/controlplanereplicascount"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	libgoapiserver "github.com/openshift/library-go/pkg/operator/configobserver/apiserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type ConfigObserver struct {
	factory.Controller
}

func NewConfigObserver(
	operatorClient v1helpers.OperatorClient,
	configInformer configinformers.SharedInformerFactory,
	operatorConfigInformers operatorv1informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	masterNodeInformer cache.SharedIndexInformer,
	masterNodeLister corev1listers.NodeLister,
	resourceSyncer resourcesynccontroller.ResourceSyncer,
	eventRecorder events.Recorder,
) *ConfigObserver {
	interestingNamespaces := []string{
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		"openshift-etcd",
		operatorclient.OperatorNamespace,
		"kube-system",
	}

	configMapPreRunCacheSynced := []cache.InformerSynced{}
	for _, ns := range interestingNamespaces {
		configMapPreRunCacheSynced = append(configMapPreRunCacheSynced, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer().HasSynced)
	}

	informers := []factory.Informer{
		operatorConfigInformers.Operator().V1().Etcds().Informer(),
		configInformer.Config().V1().APIServers().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer(),
		masterNodeInformer,
	}

	for _, ns := range interestingNamespaces {
		informers = append(informers, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer())
	}

	c := &ConfigObserver{
		Controller: configobserver.NewConfigObserver(
			"etcd",
			operatorClient,
			eventRecorder,
			configobservation.Listers{
				APIServerLister_: configInformer.Config().V1().APIServers().Lister(),

				OpenshiftEtcdEndpointsLister:          kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Lister(),
				OpenshiftEtcdPodsLister:               kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Lister(),
				OpenshiftEtcdConfigMapsLister:         kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Lister(),
				NodeLister:                            masterNodeLister,
				ConfigMapListerForKubeSystemNamespace: kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Lister().ConfigMaps("kube-system"),

				ResourceSync: resourceSyncer,
				PreRunCachesSynced: append(configMapPreRunCacheSynced,
					operatorClient.Informer().HasSynced,
					operatorConfigInformers.Operator().V1().Etcds().Informer().HasSynced,
					configInformer.Config().V1().APIServers().Informer().HasSynced,

					kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer().HasSynced,
					kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer().HasSynced,
					kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer().HasSynced,
					masterNodeInformer.HasSynced,
				),
			},
			informers,
			libgoapiserver.ObserveTLSSecurityProfile,
			controlplanereplicascount.ObserveControlPlaneReplicas,
		),
	}

	return c
}
