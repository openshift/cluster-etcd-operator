package operator

import (
	"context"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/unsupportedconfigoverridescontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/backupconfigcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/configobservercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdcertsigner"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdendpointscontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdmemberscontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/hostendpointscontroller2"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/metriccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/resourcesynccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/scriptcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/targetconfigcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/resources/etcd"
)

func RunOperator(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	// This kube client use protobuf, do not use it for CR
	kubeClient, err := kubernetes.NewForConfig(controllerContext.ProtoKubeConfig)
	if err != nil {
		return err
	}
	operatorConfigClient, err := operatorversionedclient.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	operatorV1Client, err := operatorv1client.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	configClient, err := configv1client.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}

	operatorInformers := operatorv1informers.NewSharedInformerFactory(operatorConfigClient, 10*time.Minute)
	//operatorConfigInformers.ForResource()
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
		kubeClient,
		"",
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
		"kube-system",
		"openshift-kube-apiserver",
		"openshift-kube-controller-manager",
		"openshift-kube-scheduler",
	)
	configInformers := configv1informers.NewSharedInformerFactory(configClient, 10*time.Minute)
	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(controllerContext.KubeConfig, operatorv1.GroupVersion.WithResource("etcds"))
	if err != nil {
		return err
	}
	etcdClient := etcdcli.NewEtcdClient(
		kubeInformersForNamespaces,
		configInformers.Config().V1().Networks(),
		controllerContext.EventRecorder)

	resourceSyncController, err := resourcesynccontroller.NewResourceSyncController(
		operatorClient,
		kubeInformersForNamespaces,
		kubeClient,
		controllerContext.EventRecorder,
	)
	if err != nil {
		return err
	}

	configObserver := configobservercontroller.NewConfigObserver(
		operatorClient,
		operatorInformers,
		kubeInformersForNamespaces,
		resourceSyncController,
		controllerContext.EventRecorder,
	)

	staticResourceController := staticresourcecontroller.NewStaticResourceController(
		"EtcdStaticResources",
		etcd_assets.Asset,
		[]string{
			"etcd/ns.yaml",
			"etcd/sa.yaml",
			"etcd/svc.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient),
		operatorClient,
		controllerContext.EventRecorder,
	).AddKubeInformers(kubeInformersForNamespaces)

	envVarController := etcdenvvar.NewEnvVarController(
		os.Getenv("IMAGE"),
		operatorClient,
		kubeInformersForNamespaces,
		configInformers.Config().V1().Infrastructures(),
		configInformers.Config().V1().Networks(),
		controllerContext.EventRecorder,
	)

	targetConfigReconciler := targetconfigcontroller.NewTargetConfigController(
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		operatorClient,
		kubeInformersForNamespaces.InformersFor("openshift-etcd"),
		kubeInformersForNamespaces,
		configInformers.Config().V1().Infrastructures(),
		configInformers.Config().V1().Networks(),
		kubeClient,
		envVarController,
		controllerContext.EventRecorder,
	)

	backupConfigReconciler := backupconfigcontroller.NewBackupConfigController(
		os.Getenv("OPERATOR_IMAGE"),
		operatorClient,
		operatorV1Client,
		kubeInformersForNamespaces,
		kubeClient,
		controllerContext.EventRecorder,
	)

	versionRecorder := status.NewVersionGetter()
	clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, "etcd", metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	for _, version := range clusterOperator.Status.Versions {
		versionRecorder.SetVersion(version.Name, version.Version)
	}
	versionRecorder.SetVersion("raw-internal", status.VersionForOperatorFromEnv())
	versionRecorder.SetVersion("operator", status.VersionForOperatorFromEnv())

	staticPodControllers, err := staticpod.NewBuilder(operatorClient, kubeClient, kubeInformersForNamespaces).
		WithEvents(controllerContext.EventRecorder).
		WithInstaller([]string{"cluster-etcd-operator", "installer"}).
		WithPruning([]string{"cluster-etcd-operator", "prune"}, "etcd-pod").
		WithResources("openshift-etcd", "etcd", RevisionConfigMaps, RevisionSecrets).
		WithCerts("etcd-certs", CertConfigMaps, CertSecrets).
		WithVersioning(operatorclient.OperatorNamespace, "etcd", versionRecorder).
		ToControllers()
	if err != nil {
		return err
	}

	fsyncMetricController := metriccontroller.NewFSyncController(operatorClient, configClient.ConfigV1(), kubeClient.CoreV1(), controllerContext.EventRecorder)

	statusController := status.NewClusterOperatorStatusController(
		"etcd",
		[]configv1.ObjectReference{
			{Group: "operator.openshift.io", Resource: "etcds", Name: "cluster"},
			{Resource: "namespaces", Name: operatorclient.GlobalUserSpecifiedConfigNamespace},
			{Resource: "namespaces", Name: operatorclient.GlobalMachineSpecifiedConfigNamespace},
			{Resource: "namespaces", Name: operatorclient.OperatorNamespace},
			{Resource: "namespaces", Name: "openshift-etcd"},
		},
		configClient.ConfigV1(),
		configInformers.Config().V1().ClusterOperators(),
		operatorClient,
		versionRecorder,
		controllerContext.EventRecorder,
	)
	coreClient := clientset

	etcdCertSignerController := etcdcertsigner.NewEtcdCertSignerController(
		coreClient,
		operatorClient,
		kubeInformersForNamespaces,
		configInformers.Config().V1().Infrastructures(),
		controllerContext.EventRecorder,
	)
	hostEtcdEndpointController2 := hostendpointscontroller2.NewHostEndpoints2Controller(
		operatorClient,
		controllerContext.EventRecorder,
		coreClient,
		kubeInformersForNamespaces,
	)
	etcdEndpointsController := etcdendpointscontroller.NewEtcdEndpointsController(
		operatorClient,
		controllerContext.EventRecorder,
		coreClient,
		kubeInformersForNamespaces,
	)

	clusterMemberController := clustermembercontroller.NewClusterMemberController(
		operatorClient,
		kubeInformersForNamespaces,
		configInformers.Config().V1().Networks(),
		etcdClient,
		controllerContext.EventRecorder,
	)
	etcdMembersController := etcdmemberscontroller.NewEtcdMembersController(
		operatorClient,
		etcdClient,
		controllerContext.EventRecorder,
	)
	bootstrapTeardownController := bootstrapteardown.NewBootstrapTeardownController(
		operatorClient,
		kubeInformersForNamespaces,
		etcdClient,
		controllerContext.EventRecorder,
	)

	scriptController := scriptcontroller.NewScriptControllerController(
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		envVarController,
		controllerContext.EventRecorder,
	)

	unsupportedConfigOverridesController := unsupportedconfigoverridescontroller.NewUnsupportedConfigOverridesController(
		operatorClient,
		controllerContext.EventRecorder,
	)

	operatorInformers.Start(ctx.Done())
	kubeInformersForNamespaces.Start(ctx.Done())
	configInformers.Start(ctx.Done())
	dynamicInformers.Start(ctx.Done())

	go fsyncMetricController.Run(ctx, 1)
	go staticResourceController.Run(ctx, 1)
	go targetConfigReconciler.Run(ctx, 1)
	go backupConfigReconciler.Run(ctx, 1)
	go etcdCertSignerController.Run(ctx, 1)
	go hostEtcdEndpointController2.Run(ctx, 1)
	go etcdEndpointsController.Run(ctx, 1)
	go resourceSyncController.Run(ctx, 1)
	go statusController.Run(ctx, 1)
	go configObserver.Run(ctx, 1)
	go clusterMemberController.Run(ctx, 1)
	go etcdMembersController.Run(ctx, 1)
	go bootstrapTeardownController.Run(ctx, 1)
	go unsupportedConfigOverridesController.Run(ctx, 1)
	go scriptController.Run(ctx, 1)

	go envVarController.Run(1, ctx.Done())
	go staticPodControllers.Start(ctx)

	<-ctx.Done()
	return nil
}

// RevisionConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var RevisionConfigMaps = etcd.RevisionConfigMaps

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = etcd.RevisionSecrets

var CertConfigMaps = etcd.CertConfigMaps

var CertSecrets = etcd.CertSecrets
