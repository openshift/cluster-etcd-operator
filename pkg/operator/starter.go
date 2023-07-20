package operator

import (
	"context"
	"fmt"
	"github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"os"
	"regexp"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"

	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	machineinformersv1beta1 "github.com/openshift/client-go/machine/informers/externalversions/machine/v1beta1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorversionedclientv1alpha1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/backupcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/unsupportedconfigoverridescontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermemberremovalcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/configobservercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/defragcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdcertsigner"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdendpointscontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdmemberscontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/machinedeletionhooks"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/metriccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/resourcesynccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/scriptcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/targetconfigcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/upgradebackupcontroller"
)

// masterMachineLabelSelectorString allows for getting only the master machines, it matters in larger installations with many worker nodes
const masterMachineLabelSelectorString = "machine.openshift.io/cluster-api-machine-role=master"

// masterNodeLabelSelectorString allows for getting only the master nodes, it matters in larger installations with many worker nodes
const masterNodeLabelSelectorString = "node-role.kubernetes.io/master"

const releaseVersionEnvVariableName = "RELEASE_VERSION"
const missingVersion = "0.0.1-snapshot"

var AlivenessChecker = health.NewMultiAlivenessChecker()

func RunOperator(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	// This kube client use protobuf, do not use it for CR
	kubeClient, err := kubernetes.NewForConfig(controllerContext.ProtoKubeConfig)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(controllerContext.ProtoKubeConfig)
	if err != nil {
		return err
	}
	operatorConfigClient, err := operatorversionedclient.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	operatorConfigClientv1Alpha1, err := operatorversionedclientv1alpha1.NewForConfig(controllerContext.KubeConfig)
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
	machineClientSet, err := machineclient.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	machineClient := machineClientSet.MachineV1beta1().Machines("openshift-machine-api")

	// we create a new informer directly because we are only interested in observing changes to the master machines
	// primarily to avoid reconciling on every update in large clusters (~2K machines)
	masterMachineInformer := machineinformersv1beta1.NewFilteredMachineInformer(machineClientSet, "openshift-machine-api", 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, func(listOptions *metav1.ListOptions) {
		listOptions.LabelSelector = masterMachineLabelSelectorString
	})
	masterMachineLabelSelector, err := labels.Parse(masterMachineLabelSelectorString)
	if err != nil {
		return err
	}
	// we create a new informer directly because we are only interested in observing changes to the master nodes
	// primarily to avoid reconciling on every update in large clusters (~2K nodes)
	masterNodeInformer := corev1informers.NewFilteredNodeInformer(kubeClient, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, func(listOptions *metav1.ListOptions) {
		listOptions.LabelSelector = masterNodeLabelSelectorString
	})
	masterNodeLabelSelector, err := labels.Parse(masterNodeLabelSelectorString)
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
	)
	configInformers := configv1informers.NewSharedInformerFactory(configClient, 10*time.Minute)

	versionRecorder := status.NewVersionGetter()
	clusterOperator, err := configClient.ConfigV1().ClusterOperators().Get(ctx, "etcd", metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	for _, version := range clusterOperator.Status.Versions {
		versionRecorder.SetVersion(version.Name, version.Version)
	}
	// Don't set operator version. library-go will take care of it after setting operands.
	versionRecorder.SetVersion("raw-internal", status.VersionForOperatorFromEnv())
	klog.Infof("recorded cluster versions: %v", versionRecorder.GetVersions())

	featureGateAccessor := featuregates.NewFeatureGateAccess(
		versionRecorder.GetVersions()["raw-internal"],
		missingVersion,
		configInformers.Config().V1().ClusterVersions(),
		configInformers.Config().V1().FeatureGates(),
		controllerContext.EventRecorder)

	featureGateAccessor.SetChangeHandler(func(featureChange featuregates.FeatureChange) {
		// Do nothing here. The controller watches feature gate changes and will react to them.
		klog.InfoS("FeatureGates changed", "enabled", featureChange.New.Enabled, "disabled", featureChange.New.Disabled)
	})

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
		configInformers,
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
			"etcd/sm.yaml",
			"etcd/minimal-sm.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient).WithDynamicClient(dynamicClient),
		operatorClient,
		controllerContext.EventRecorder,
	).WithIgnoreNotFoundOnCreate().AddKubeInformers(kubeInformersForNamespaces)

	envVarController := etcdenvvar.NewEnvVarController(
		os.Getenv("IMAGE"),
		operatorClient,
		kubeInformersForNamespaces,
		configInformers.Config().V1().Infrastructures(),
		configInformers.Config().V1().Networks(),
		controllerContext.EventRecorder,
	)

	quorumChecker := ceohelpers.NewQuorumChecker(
		kubeInformersForNamespaces.ConfigMapLister(),
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Namespaces().Lister(),
		configInformers.Config().V1().Infrastructures().Lister(),
		operatorClient,
		etcdClient)

	targetConfigReconciler := targetconfigcontroller.NewTargetConfigController(
		AlivenessChecker,
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
		quorumChecker,
	)

	// The guardRolloutPreCheck function always waits until the etcd pods have rolled out to the new version
	// i.e clusteroperator version is the desired version, so that the PDB doesn't block the rollout
	// Also prevents guard pods from being created in SNO topology
	guardRolloutPreCheck := func() (bool, bool, error) {
		clusterOperatorInformer := configInformers.Config().V1().ClusterOperators().Informer()
		clusterOperatorLister := configInformers.Config().V1().ClusterOperators().Lister()
		expectedOperatorVersion := status.VersionForOperatorFromEnv()

		if !clusterOperatorInformer.HasSynced() {
			// Skip and don't error until synced
			return false, false, nil
		}

		etcdClusterOperator, err := clusterOperatorLister.Get("etcd")
		if err != nil {
			return false, false, fmt.Errorf("failed to get clusteroperator/etcd: %w", err)
		}
		operatorVersion := ""
		for _, version := range etcdClusterOperator.Status.Versions {
			if version.Name == "operator" {
				operatorVersion = version.Version
			}
		}
		if len(operatorVersion) == 0 {
			return false, true, nil
		}

		if operatorVersion != expectedOperatorVersion {
			klog.V(2).Infof("clusterOperator/etcd's operator version (%s) and expected operator version (%s) do not match. Will not create guard pods until operator reaches desired version.", operatorVersion, expectedOperatorVersion)
			return false, true, nil
		}

		// create only when not a single node topology
		isSNO, precheckSucceeded, err := common.NewIsSingleNodePlatformFn(configInformers.Config().V1().Infrastructures())()
		return !isSNO, precheckSucceeded, err
	}

	staticPodControllers, err := staticpod.NewBuilder(operatorClient, kubeClient, kubeInformersForNamespaces, configInformers).
		WithEvents(controllerContext.EventRecorder).
		WithInstaller([]string{"cluster-etcd-operator", "installer"}).
		WithPruning([]string{"cluster-etcd-operator", "prune"}, "etcd-pod").
		WithRevisionedResources("openshift-etcd", "etcd", RevisionConfigMaps, RevisionSecrets).
		WithUnrevisionedCerts("etcd-certs", CertConfigMaps, CertSecrets).
		WithVersioning("etcd", versionRecorder).
		WithPodDisruptionBudgetGuard(
			"openshift-etcd-operator",
			"etcd-operator",
			"9980",
			"readyz",
			guardRolloutPreCheck,
		).
		WithOperandPodLabelSelector(labels.Set{"etcd": "true"}.AsSelector()).
		ToControllers()
	if err != nil {
		return err
	}

	fsyncMetricController := metriccontroller.NewFSyncController(
		configClient.ConfigV1(),
		operatorClient,
		controllerContext.EventRecorder)

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
	).WithDegradedInertia(status.MustNewInertia(
		2*time.Minute,
		status.InertiaCondition{
			ConditionTypeMatcher: regexp.MustCompile("^(NodeController|EtcdMembers|DefragController)Degraded$"),
			Duration:             5 * time.Minute,
		},
	).Inertia)

	coreClient := clientset

	etcdCertSignerController := etcdcertsigner.NewEtcdCertSignerController(
		AlivenessChecker,
		coreClient,
		operatorClient,
		kubeInformersForNamespaces,
		controllerContext.EventRecorder,
		quorumChecker,
	)

	etcdEndpointsController := etcdendpointscontroller.NewEtcdEndpointsController(
		AlivenessChecker,
		operatorClient,
		etcdClient,
		controllerContext.EventRecorder,
		coreClient,
		kubeInformersForNamespaces,
		quorumChecker,
	)

	machineAPI := ceohelpers.NewMachineAPI(masterMachineInformer, machinelistersv1beta1.NewMachineLister(masterMachineInformer.GetIndexer()), masterMachineLabelSelector)

	clusterMemberController := clustermembercontroller.NewClusterMemberController(
		AlivenessChecker,
		operatorClient,
		machineAPI,
		masterNodeInformer,
		masterNodeLabelSelector,
		masterMachineInformer,
		masterMachineLabelSelector,
		kubeInformersForNamespaces,
		configInformers.Config().V1().Networks(),
		etcdClient,
		controllerContext.EventRecorder,
	)

	clusterMemberRemovalController := clustermemberremovalcontroller.NewClusterMemberRemovalController(
		AlivenessChecker,
		operatorClient,
		etcdClient,
		machineAPI,
		masterMachineLabelSelector, masterNodeLabelSelector,
		kubeInformersForNamespaces,
		masterNodeInformer,
		masterMachineInformer,
		configInformers.Config().V1().Networks(),
		kubeInformersForNamespaces.ConfigMapLister(),
		controllerContext.EventRecorder,
	)

	machineDeletionHooksController := machinedeletionhooks.NewMachineDeletionHooksController(
		AlivenessChecker,
		operatorClient,
		machineClient,
		etcdClient,
		kubeClient,
		machineAPI,
		masterMachineLabelSelector,
		kubeInformersForNamespaces,
		masterMachineInformer,
		controllerContext.EventRecorder)

	etcdMembersController := etcdmemberscontroller.NewEtcdMembersController(
		AlivenessChecker,
		operatorClient,
		etcdClient,
		controllerContext.EventRecorder,
	)

	bootstrapTeardownController := bootstrapteardown.NewBootstrapTeardownController(
		AlivenessChecker,
		operatorClient,
		kubeInformersForNamespaces,
		etcdClient,
		controllerContext.EventRecorder,
		configInformers.Config().V1().Infrastructures().Lister(),
	)

	scriptController := scriptcontroller.NewScriptControllerController(
		AlivenessChecker,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		envVarController,
		controllerContext.EventRecorder,
	)

	defragController := defragcontroller.NewDefragController(
		AlivenessChecker,
		operatorClient,
		etcdClient,
		configInformers.Config().V1().Infrastructures().Lister(),
		controllerContext.EventRecorder,
		kubeInformersForNamespaces,
	)

	unsupportedConfigOverridesController := unsupportedconfigoverridescontroller.NewUnsupportedConfigOverridesController(
		operatorClient,
		controllerContext.EventRecorder,
	)

	operatorInformers.Start(ctx.Done())
	kubeInformersForNamespaces.Start(ctx.Done())
	configInformers.Start(ctx.Done())
	dynamicInformers.Start(ctx.Done())

	go featureGateAccessor.Run(ctx)

	select {
	case <-featureGateAccessor.InitialFeatureGatesObserved():
		features, err := featureGateAccessor.CurrentFeatureGates()
		if err != nil {
			return fmt.Errorf("could not find FeatureGates, aborting controller start: %w", err)
		}

		enabled, disabled := getEnabledDisabledFeatures(features)
		klog.Info("FeatureGates initialized", "enabled", enabled, "disabled", disabled)
	case <-time.After(1 * time.Minute):
		return fmt.Errorf("timed out waiting for FeatureGate detection, aborting controller start")
	}

	// putting all backup related controllers after the feature flags to ensure flags are initialized already
	upgradeBackupController := upgradebackupcontroller.NewUpgradeBackupController(
		AlivenessChecker,
		operatorClient,
		configClient.ConfigV1(),
		kubeClient,
		etcdClient,
		kubeInformersForNamespaces,
		configInformers.Config().V1().ClusterVersions(),
		configInformers.Config().V1().ClusterOperators(),
		controllerContext.EventRecorder,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
	)

	backupController := backupcontroller.NewBackupController(
		AlivenessChecker,
		operatorConfigClientv1Alpha1,
		kubeClient,
		controllerContext.EventRecorder,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		featureGateAccessor)

	go masterMachineInformer.Run(ctx.Done())
	go masterNodeInformer.Run(ctx.Done())
	go fsyncMetricController.Run(ctx, 1)
	go staticResourceController.Run(ctx, 1)
	go targetConfigReconciler.Run(ctx, 1)
	go etcdCertSignerController.Run(ctx, 1)
	go etcdEndpointsController.Run(ctx, 1)
	go resourceSyncController.Run(ctx, 1)
	go statusController.Run(ctx, 1)
	go configObserver.Run(ctx, 1)
	go clusterMemberController.Run(ctx, 1)
	go clusterMemberRemovalController.Run(ctx, 1)
	go machineDeletionHooksController.Run(ctx, 1)
	go etcdMembersController.Run(ctx, 1)
	go bootstrapTeardownController.Run(ctx, 1)
	go unsupportedConfigOverridesController.Run(ctx, 1)
	go scriptController.Run(ctx, 1)
	go defragController.Run(ctx, 1)
	go upgradeBackupController.Run(ctx, 1)
	go backupController.Run(ctx, 1)

	go envVarController.Run(1, ctx.Done())
	go staticPodControllers.Start(ctx)

	<-ctx.Done()
	return nil
}

// GetReleaseVersion gets the release version string from the env
func GetReleaseVersion(c v1.ClusterOperatorInterface) (string, error) {
	etcd, err := c.Get(context.Background(), "etcd", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("could not get etcd cluster operator CRD: %w", err)
	}

	klog.Infof("found etcd versions: %v", etcd.Status.Versions)
	for _, version := range etcd.Status.Versions {
		if version.Name == "operator" {
			return version.Version, nil
		}
	}

	// if we found nothing on the CRD, we can resort to the older unsupported env variable
	releaseVersion := os.Getenv(releaseVersionEnvVariableName)
	if len(releaseVersion) == 0 {
		return "", fmt.Errorf("%s environment variable is missing", releaseVersionEnvVariableName)
	}
	return releaseVersion, nil
}

func getEnabledDisabledFeatures(features featuregates.FeatureGate) ([]string, []string) {
	var enabled []string
	var disabled []string

	for _, feature := range features.KnownFeatures() {
		if features.Enabled(feature) {
			enabled = append(enabled, string(feature))
		} else {
			disabled = append(disabled, string(feature))
		}
	}

	return enabled, disabled
}

// RevisionConfigMaps is a list of configmaps that are directly copied for the current values.  A different actor/controller modifies these.
// the first element should be the configmap that contains the static pod manifest
var RevisionConfigMaps = []revision.RevisionResource{
	{Name: "etcd-pod"},

	{Name: "etcd-serving-ca"},
	{Name: "etcd-peer-client-ca"},
	{Name: "etcd-metrics-proxy-serving-ca"},
	{Name: "etcd-metrics-proxy-client-ca"},
	{Name: "etcd-endpoints"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	{Name: "etcd-all-certs"},
}

var CertConfigMaps = []installer.UnrevisionedResource{
	{Name: "restore-etcd-pod"},
	{Name: "etcd-scripts"},
	{Name: "etcd-serving-ca"},
	{Name: "etcd-peer-client-ca"},
	{Name: "etcd-metrics-proxy-serving-ca"},
	{Name: "etcd-metrics-proxy-client-ca"},
}

var CertSecrets = []installer.UnrevisionedResource{
	// these are also copied to certs to have a constant file location so we can refer to them in various recovery scripts
	// and in the PDB
	{Name: "etcd-all-certs"},
}
