package resourcesynccontroller

import (
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

func NewResourceSyncController(
	operatorConfigClient v1helpers.OperatorClient,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder) (*resourcesynccontroller.ResourceSyncController, error) {

	resourceSyncController := resourcesynccontroller.NewResourceSyncController(
		operatorConfigClient,
		kubeInformersForNamespaces,
		v1helpers.CachedSecretGetter(kubeClient.CoreV1(), kubeInformersForNamespaces),
		v1helpers.CachedConfigMapGetter(kubeClient.CoreV1(), kubeInformersForNamespaces),
		eventRecorder,
	)

	if err := resourceSyncController.SyncConfigMap(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.OperatorNamespace, Name: "etcd-ca-bundle"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-ca-bundle"},
	); err != nil {
		return nil, err
	}

	if err := resourceSyncController.SyncConfigMap(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.TargetNamespace, Name: "etcd-ca-bundle"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-ca-bundle"},
	); err != nil {
		return nil, err
	}

	if err := resourceSyncController.SyncSecret(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.OperatorNamespace, Name: "etcd-client"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-client"},
	); err != nil {
		return nil, err
	}

	if err := resourceSyncController.SyncSecret(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.TargetNamespace, Name: "etcd-client"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-client"},
	); err != nil {
		return nil, err
	}

	if err := resourceSyncController.SyncConfigMap(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.TargetNamespace, Name: "etcd-peer-client-ca"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-ca-bundle"},
	); err != nil {
		return nil, err
	}
	if err := resourceSyncController.SyncConfigMap(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.TargetNamespace, Name: "etcd-metrics-proxy-client-ca"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-metric-serving-ca"},
	); err != nil {
		return nil, err
	}
	if err := resourceSyncController.SyncConfigMap(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.TargetNamespace, Name: "etcd-serving-ca"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-serving-ca"},
	); err != nil {
		return nil, err
	}
	if err := resourceSyncController.SyncConfigMap(
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.TargetNamespace, Name: "etcd-metrics-proxy-serving-ca"},
		resourcesynccontroller.ResourceLocation{Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace, Name: "etcd-metric-serving-ca"},
	); err != nil {
		return nil, err
	}

	return resourceSyncController, nil
}
