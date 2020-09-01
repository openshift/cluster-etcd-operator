package operator

import (
	"context"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/unsupportedconfigoverridescontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
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
		"openshift-machine-config-operator", // TODO remove after quorum-guard is removed from MCO
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

	// clean up the old PDBs as soon as the replacements are in place.  This checks every minute, so it's fast enough
	// to work after the new deployment is created and before the MCO restarts enough nodes to get stuck.
	ensureMCOEtcQuorumGuardCleanup(ctx, kubeClient, controllerContext.EventRecorder)
	ensureMCOEtcQuorumGuardPDBCleanup(ctx, kubeClient, controllerContext.EventRecorder)

	go fsyncMetricController.Run(ctx, 1)
	go staticResourceController.Run(ctx, 1)
	go targetConfigReconciler.Run(ctx, 1)
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
var RevisionConfigMaps = []revision.RevisionResource{
	{Name: "etcd-pod"},

	{Name: "config"},
	{Name: "etcd-serving-ca"},
	{Name: "etcd-peer-client-ca"},
	{Name: "etcd-metrics-proxy-serving-ca"},
	{Name: "etcd-metrics-proxy-client-ca"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	{Name: "etcd-all-peer"},
	{Name: "etcd-all-serving"},
	{Name: "etcd-all-serving-metrics"},
}

var CertConfigMaps = []revision.RevisionResource{
	{Name: "restore-etcd-pod"},
	{Name: "etcd-scripts"},
	{Name: "etcd-serving-ca"},
	{Name: "etcd-peer-client-ca"},
	{Name: "etcd-metrics-proxy-serving-ca"},
	{Name: "etcd-metrics-proxy-client-ca"},
}

var CertSecrets = []revision.RevisionResource{
	// these are also copied to certs to have a constant file location so we can refer to them in various recovery scripts
	// and in the PDB
	{Name: "etcd-all-peer"},
	{Name: "etcd-all-serving"},
	{Name: "etcd-all-serving-metrics"},
}

// ensureMCOEtcQuorumGuardCleanup continually ensures the removal of the legacy etcd quorum guard
func ensureMCOEtcQuorumGuardCleanup(ctx context.Context, kubeClient *kubernetes.Clientset, eventRecorder events.Recorder) {
	// mco and etcd and deployment both use the same name
	resourceName := "etcd-quorum-guard"

	mcoClient := kubeClient.AppsV1().Deployments("openshift-machine-config-operator")
	etcdOClient := kubeClient.AppsV1().Deployments(operatorclient.TargetNamespace)

	go wait.UntilWithContext(ctx, func(_ context.Context) {
		// This function isn't expected to take long enough to suggest
		// checking that the context is done. The wait method will do that
		// checking.

		// Check whether the legacy etcd quorum guard exists and is not marked for deletion
		mcoEtcdQuorumGuard, err := mcoClient.Get(ctx, resourceName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			// Done - new etcd quorum guard does not exist
			return
		}
		if err != nil {
			klog.Warningf("Error retrieving legacy etcd quorum guard: %v", err)
			return
		}
		if mcoEtcdQuorumGuard.ObjectMeta.DeletionTimestamp != nil {
			// Done - daemonset has been marked for deletion
			return
		}

		// Check that the deployment managing the apiserver pods has the correct number of replicas
		etcdOperatorQuorumGuard, err := etcdOClient.Get(ctx, resourceName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			// No available replicas if the deployment doesn't exist
			return
		}
		if err != nil {
			klog.Warningf("Error retrieving the deployment that manages etcd quorum guard pods: %v", err)
			return
		}
		if etcdOperatorQuorumGuard.Status.AvailableReplicas == 3 {
			eventRecorder.Warning("LegacyDaemonSetCleanup", "the deployment replacing the etcd quorum guard does not have three available replicas yet")
			return
		}

		// Safe to remove legacy etcd quorum guard since the deployment has the correct number of replicas
		err = mcoClient.Delete(ctx, resourceName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			klog.Warningf("Failed to delete legacy etcd quorum guard: %v", err)
			return
		}
		eventRecorder.Event("LegacyEtcdQuorumGuardRemoved", "legacy etcd quorum guard has been removed")
	}, time.Minute)
}

// ensureMCOEtcQuorumGuardPDBCleanup continually ensures the removal of the legacy etcd quorum guard pdb
func ensureMCOEtcQuorumGuardPDBCleanup(ctx context.Context, kubeClient *kubernetes.Clientset, eventRecorder events.Recorder) {
	// mco and etcd and deployment both use the same name
	resourceName := "etcd-quorum-guard"

	mcoClient := kubeClient.PolicyV1beta1().PodDisruptionBudgets("openshift-machine-config-operator")
	etcdOClient := kubeClient.PolicyV1beta1().PodDisruptionBudgets(operatorclient.TargetNamespace)

	go wait.UntilWithContext(ctx, func(_ context.Context) {
		// This function isn't expected to take long enough to suggest
		// checking that the context is done. The wait method will do that
		// checking.

		// Check whether the legacy etcd quorum guard exists and is not marked for deletion
		mcoEtcdQuorumGuard, err := mcoClient.Get(ctx, resourceName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			// Done - new etcd quorum guard does not exist
			return
		}
		if err != nil {
			klog.Warningf("Error retrieving legacy pdb: %v", err)
			return
		}
		if mcoEtcdQuorumGuard.ObjectMeta.DeletionTimestamp != nil {
			// Done - daemonset has been marked for deletion
			return
		}

		// Check that the pdb exists
		_, err = etcdOClient.Get(ctx, resourceName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			// No available replicas if the deployment doesn't exist
			return
		}
		if err != nil {
			klog.Warningf("Error retrieving the pdb that manages etcd quorum guard pods: %v", err)
			return
		}

		// Safe to remove legacy etcd quorum guard since the deployment has the correct number of replicas
		err = mcoClient.Delete(ctx, resourceName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			klog.Warningf("Failed to delete legacy etcd quorum guard: %v", err)
			return
		}
		eventRecorder.Event("LegacyEtcdQuorumGuardPDBRemoved", "legacy etcd quorum guard pdb has been removed")
	}, time.Minute)
}
