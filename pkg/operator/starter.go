package operator

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configversionedclientv1alpha1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	machineinformersv1beta1 "github.com/openshift/client-go/machine/informers/externalversions/machine/v1beta1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorversionedclientv1alpha1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/backupcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermemberremovalcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/configobservercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/defragcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdcertcleaner"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdcertsigner"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdendpointscontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdmemberscontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/machinedeletionhooks"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/metriccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/periodicbackupcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/resourcesynccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/scriptcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/targetconfigcontroller"
	tnf "github.com/openshift/cluster-etcd-operator/pkg/tnf/operator"
)

// masterMachineLabelSelectorString allows for getting only the master machines, it matters in larger installations with many worker nodes
const masterMachineLabelSelectorKeyString = "machine.openshift.io/cluster-api-machine-role"
const masterMachineLabelSelectorValueString = "master"
const arbiterMachineLabelSelectorValueString = "arbiter"

// masterNodeLabelSelectorString allows for getting only the master nodes, it matters in larger installations with many worker nodes
const masterNodeLabelSelectorString = "node-role.kubernetes.io/master"

const arbiterNodeLabelSelectorString = "node-role.kubernetes.io/arbiter"

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
	configClientv1Alpha1, err := configversionedclientv1alpha1.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}
	configClient, err := configv1client.NewForConfig(controllerContext.KubeConfig)
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
		listOptions.LabelSelector = fmt.Sprintf("%s in (%s,%s)", masterMachineLabelSelectorKeyString, masterMachineLabelSelectorValueString, arbiterMachineLabelSelectorValueString)
	})
	masterMachineLabelSelector, err := labels.Parse(fmt.Sprintf("%s in (%s,%s)", masterMachineLabelSelectorKeyString, masterMachineLabelSelectorValueString, arbiterMachineLabelSelectorValueString))
	if err != nil {
		return err
	}

	arbiterNodeLabelSelector, err := labels.Parse(arbiterNodeLabelSelectorString)
	if err != nil {
		return err
	}

	// we create a new informer directly because we are only interested in observing changes to the master nodes
	// primarily to avoid reconciling on every update in large clusters (~2K nodes)
	controlPlaneNodeInformer := corev1informers.NewFilteredNodeInformer(kubeClient, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, func(listOptions *metav1.ListOptions) {
		listOptions.LabelSelector = masterNodeLabelSelectorString
	})
	controlPlaneNodeLister := corev1listers.NewNodeLister(controlPlaneNodeInformer.GetIndexer())
	controlPlaneNodeLabelSelector, err := labels.Parse(masterNodeLabelSelectorString)
	if err != nil {
		return err
	}

	hasArbiterTopology, err := ceohelpers.IsArbiterNodeTopology(ctx, configClient.ConfigV1().Infrastructures())
	if err != nil {
		return err
	}

	if hasArbiterTopology {
		// In arbiter deployments we have master and arbiter nodes, we can not reliably use a custom informer for two different API calls
		// since that causes issues with two different ResourceVersion and a race condition between when one node object is added and the order of the
		// watch events start coming in, leading to missing an object being added. It is more correct to inform on all nodes and filter in the lister.
		// Note that this should only be used in arbiter deployments where the use case is small edge deployments with limited resources and handful of nodes.
		controlPlaneNodeInformer = corev1informers.NewNodeInformer(kubeClient, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		controlPlaneNodeLister = ceohelpers.NewMultiSelectorNodeLister(controlPlaneNodeInformer.GetIndexer(), controlPlaneNodeLabelSelector, arbiterNodeLabelSelector)
	}

	etcdNamespaceInformer := corev1informers.NewFilteredNamespaceInformer(kubeClient, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, func(listOptions *metav1.ListOptions) {
		listOptions.FieldSelector = "metadata.name=" + operatorclient.TargetNamespace
	})
	etcdNamespaceLister := corev1listers.NewNamespaceLister(etcdNamespaceInformer.GetIndexer())

	operatorInformers := operatorv1informers.NewSharedInformerFactory(operatorConfigClient, 10*time.Minute)
	etcdsInformer := operatorInformers.Operator().V1().Etcds()
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
		kubeClient,
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
		"kube-system",
	)
	clusterInformers := v1helpers.NewKubeInformersForNamespaces(kubeClient, "")

	configInformers := configv1informers.NewSharedInformerFactory(configClient, 10*time.Minute)
	clusterVersions := configInformers.Config().V1().ClusterVersions()
	networkInformer := configInformers.Config().V1().Networks()
	jobsInformer := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs().Informer()

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
		clusterVersions,
		configInformers.Config().V1().FeatureGates(),
		controllerContext.EventRecorder)
	// we keep the default behavior of exiting the controller once a gate changes
	featureGateAccessor.SetChangeHandler(featuregates.ForceExit)

	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(controllerContext.Clock, controllerContext.KubeConfig, operatorv1.GroupVersion.WithResource("etcds"), operatorv1.GroupVersion.WithKind("Etcd"), ExtractStaticPodOperatorSpec, ExtractStaticPodOperatorStatus)
	if err != nil {
		return err
	}

	etcdClient := etcdcli.NewEtcdClient(
		kubeInformersForNamespaces,
		controlPlaneNodeInformer,
		controlPlaneNodeLister,
		networkInformer,
		controllerContext.EventRecorder)

	cachedMemberClient := etcdcli.NewMemberCache(etcdClient)

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
		controlPlaneNodeInformer,
		controlPlaneNodeLister,
		resourceSyncController,
		controllerContext.EventRecorder,
	)

	staticResourceController := staticresourcecontroller.NewStaticResourceController(
		"EtcdStaticResources",
		bindata.Asset,
		[]string{
			"etcd/ns.yaml",
			"etcd/sa.yaml",
			"etcd/svc.yaml",
			"etcd/sm.yaml",
			"etcd/minimal-sm.yaml",
			"etcd/prometheus-role.yaml",
			"etcd/prometheus-rolebinding.yaml",
			"etcd/backups-sa.yaml",
			"etcd/backups-cr.yaml",
			"etcd/backups-crb.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient).WithDynamicClient(dynamicClient),
		operatorClient,
		controllerContext.EventRecorder,
	).WithIgnoreNotFoundOnCreate().AddKubeInformers(kubeInformersForNamespaces)

	envVarController := etcdenvvar.NewEnvVarController(
		os.Getenv("IMAGE"),
		operatorClient,
		kubeInformersForNamespaces,
		controlPlaneNodeInformer,
		controlPlaneNodeLister,
		controlPlaneNodeLabelSelector,
		configInformers.Config().V1().Infrastructures(),
		networkInformer,
		controllerContext.EventRecorder,
		etcdsInformer,
		featureGateAccessor,
	)

	quorumChecker := ceohelpers.NewQuorumChecker(
		etcdNamespaceLister,
		configInformers.Config().V1().Infrastructures().Lister(),
		operatorClient,
		cachedMemberClient)

	targetConfigReconciler := targetconfigcontroller.NewTargetConfigController(
		AlivenessChecker,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		operatorClient,
		kubeInformersForNamespaces.InformersFor("openshift-etcd"),
		kubeInformersForNamespaces,
		configInformers.Config().V1().Infrastructures(),
		networkInformer,
		controlPlaneNodeInformer,
		etcdsInformer,
		kubeClient,
		envVarController,
		controllerContext.EventRecorder,
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

	quorumSafe := func(ctx context.Context) (bool, error) {
		return quorumChecker.IsSafeToUpdateRevision()
	}

	staticPodBuilderControllers := staticpod.NewBuilder(operatorClient, kubeClient, kubeInformersForNamespaces, clusterInformers.InformersFor(""), configInformers, controllerContext.Clock).
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
			// etcd should use a default UnhealthyPodEvictionPolicy behavior corresponding to the
			// IfHealthyBudget policy. This policy achieves the least amount of disruption, as it
			// does not allow eviction when multiple etcd pods do not report readiness.
			// This can block node drain/maintenance. The cluster administrator should then
			// analyze these pods and decide which one to bring down manually.
			nil,
			guardRolloutPreCheck,
		).
		WithRevisionControllerPrecondition(quorumSafe).
		WithOperandPodLabelSelector(labels.Set{"etcd": "true"}.AsSelector())
	if hasArbiterTopology {
		staticPodBuilderControllers = staticPodBuilderControllers.WithExtraNodeSelector(arbiterNodeLabelSelector)
	}

	staticPodControllers, err := staticPodBuilderControllers.ToControllers()
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
		clock.RealClock{},
	).WithDegradedInertia(status.MustNewInertia(
		2*time.Minute,
		status.InertiaCondition{
			ConditionTypeMatcher: regexp.MustCompile("^(NodeController|EtcdMembers|DefragController)Degraded$"),
			Duration:             5 * time.Minute,
		},
	).Inertia)

	coreClient := kubeClient

	// the configInformer has to run before the machine and backup feature checks
	configInformers.Start(ctx.Done())
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

	etcdCertSignerController, err := etcdcertsigner.NewEtcdCertSignerController(
		AlivenessChecker,
		coreClient,
		operatorClient,
		kubeInformersForNamespaces,
		controlPlaneNodeInformer,
		controlPlaneNodeLister,
		controlPlaneNodeLabelSelector,
		controllerContext.EventRecorder,
		legacyregistry.DefaultGatherer.(metrics.KubeRegistry),
		false,
		featureGateAccessor,
	)
	if err != nil {
		return fmt.Errorf("could not start etcdCertSignerController, aborting controller start: %w", err)
	}

	etcdCertCleanerController := etcdcertcleaner.NewEtcdCertCleanerController(
		AlivenessChecker,
		coreClient,
		operatorClient,
		kubeInformersForNamespaces,
		controlPlaneNodeLister,
		controlPlaneNodeLabelSelector,
		controllerContext.EventRecorder,
	)

	etcdEndpointsController := etcdendpointscontroller.NewEtcdEndpointsController(
		AlivenessChecker,
		operatorClient,
		etcdClient,
		controllerContext.EventRecorder,
		coreClient,
		kubeInformersForNamespaces,
	)

	etcdMembersController := etcdmemberscontroller.NewEtcdMembersController(
		AlivenessChecker,
		operatorClient,
		cachedMemberClient,
		controllerContext.EventRecorder,
	)

	bootstrapTeardownController := bootstrapteardown.NewBootstrapTeardownController(
		AlivenessChecker,
		operatorClient,
		kubeInformersForNamespaces,
		etcdNamespaceInformer,
		etcdNamespaceLister,
		etcdClient,
		controllerContext.EventRecorder,
		configInformers.Config().V1().Infrastructures().Lister(),
	)

	scriptController := scriptcontroller.NewScriptControllerController(
		AlivenessChecker,
		operatorClient,
		kubeClient,
		envVarController,
		controllerContext.EventRecorder,
	)

	defragController := defragcontroller.NewDefragController(
		AlivenessChecker,
		operatorClient,
		cachedMemberClient, // for cached List/Health calls
		etcdClient,         // for status calls
		etcdClient,         // for defrag calls
		configInformers.Config().V1().Infrastructures().Lister(),
		controllerContext.EventRecorder,
		kubeInformersForNamespaces,
	)

	unsupportedConfigOverridesController := unsupportedconfigoverridescontroller.NewUnsupportedConfigOverridesController(
		"etcd",
		operatorClient,
		controllerContext.EventRecorder,
	)

	enabledAutoBackupFeature, err := backuphelpers.AutoBackupFeatureGateEnabled(featureGateAccessor)
	if err != nil {
		return fmt.Errorf("could not determine AutoBackupFeatureGateEnabled, aborting controller start: %w", err)
	}

	if enabledAutoBackupFeature {
		etcdBackupInformer := operatorInformers.Operator().V1alpha1().EtcdBackups().Informer()
		configBackupInformer := configInformers.Config().V1alpha1().Backups().Informer()

		klog.Infof("found automated backup feature to be enabled, starting controllers...")
		periodicBackupController := periodicbackupcontroller.NewPeriodicBackupController(
			AlivenessChecker,
			operatorClient,
			configClientv1Alpha1,
			kubeClient,
			controllerContext.EventRecorder,
			os.Getenv("OPERATOR_IMAGE"),
			featureGateAccessor,
			configBackupInformer,
			kubeInformersForNamespaces)

		backupController := backupcontroller.NewBackupController(
			AlivenessChecker,
			operatorConfigClientv1Alpha1,
			kubeClient,
			controllerContext.EventRecorder,
			os.Getenv("OPERATOR_IMAGE"),
			featureGateAccessor,
			etcdBackupInformer,
			jobsInformer)

		backupRemovalController := backupcontroller.NewBackupRemovalController(
			AlivenessChecker,
			operatorConfigClientv1Alpha1,
			kubeClient,
			controllerContext.EventRecorder,
			featureGateAccessor,
			etcdBackupInformer,
			jobsInformer)

		go etcdBackupInformer.Run(ctx.Done())
		go configBackupInformer.Run(ctx.Done())
		go jobsInformer.Run(ctx.Done())

		go periodicBackupController.Run(ctx, 1)
		go backupController.Run(ctx, 1)
		go backupRemovalController.Run(ctx, 1)
	}

	// we have to wait for the definitive result of the cluster version informer to make the correct machine API decision
	klog.Infof("waiting for cluster version informer sync...")
	versionTimeoutCtx, versionTimeoutCancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
	clusterVersionInformerSynced := cache.WaitForCacheSync(versionTimeoutCtx.Done(), clusterVersions.Informer().HasSynced)
	versionTimeoutCancelFunc()

	if !clusterVersionInformerSynced {
		return fmt.Errorf("could not sync ClusterVersion, aborting operator start")
	}

	clusterMemberControllerInformers := []factory.Informer{controlPlaneNodeInformer}
	machineLister := machinelistersv1beta1.NewMachineLister(masterMachineInformer.GetIndexer())
	machineAPI := ceohelpers.NewMachineAPI(masterMachineInformer, machineLister, masterMachineLabelSelector, clusterVersions, dynamicClient)
	machineAPIEnabled, err := machineAPI.IsEnabled()
	if err != nil {
		return fmt.Errorf("could not determine whether machine API is enabled, aborting controller start")
	}

	machineAPIAvailable, err := machineAPI.IsAvailable()
	if err != nil {
		return fmt.Errorf("could not determine whether machine API is available, aborting controller start")
	}

	if machineAPIEnabled || machineAPIAvailable {
		klog.Infof("Detected available machine API, starting vertical scaling related controllers and informers...")
		clusterMemberControllerInformers = append(clusterMemberControllerInformers, masterMachineInformer)
		clusterMemberRemovalController := clustermemberremovalcontroller.NewClusterMemberRemovalController(
			AlivenessChecker,
			operatorClient,
			etcdClient,
			machineAPI,
			masterMachineLabelSelector, controlPlaneNodeLabelSelector,
			kubeInformersForNamespaces,
			controlPlaneNodeInformer,
			masterMachineInformer,
			networkInformer,
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

		go masterMachineInformer.Run(ctx.Done())
		go clusterMemberRemovalController.Run(ctx, 1)
		go machineDeletionHooksController.Run(ctx, 1)
	}

	// clusterMemberController uses the machineAPI component, but can run entirely without it
	clusterMemberController := clustermembercontroller.NewClusterMemberController(
		AlivenessChecker,
		operatorClient,
		machineAPI,
		controlPlaneNodeLister,
		controlPlaneNodeLabelSelector,
		machineLister,
		masterMachineLabelSelector,
		kubeInformersForNamespaces,
		networkInformer,
		etcdClient,
		controllerContext.EventRecorder,
		clusterMemberControllerInformers...,
	)

	_, err = tnf.HandleDualReplicaClusters(ctx, controllerContext, featureGateAccessor, configInformers, operatorClient,
		envVarController, kubeInformersForNamespaces, networkInformer, controlPlaneNodeInformer, etcdsInformer,
		kubeClient, dynamicClient)
	if err != nil {
		return err
	}

	go controlPlaneNodeInformer.Run(ctx.Done())
	go etcdNamespaceInformer.Run(ctx.Done())
	dynamicInformers.Start(ctx.Done())
	operatorInformers.Start(ctx.Done())
	clusterInformers.Start(ctx.Done())
	kubeInformersForNamespaces.Start(ctx.Done())

	go fsyncMetricController.Run(ctx, 1)
	go staticResourceController.Run(ctx, 1)
	go targetConfigReconciler.Run(ctx, 1)
	go etcdCertSignerController.Run(ctx, 1)
	go etcdCertCleanerController.Run(ctx, 1)
	go etcdEndpointsController.Run(ctx, 1)
	go resourceSyncController.Run(ctx, 1)
	go statusController.Run(ctx, 1)
	go configObserver.Run(ctx, 1)
	go clusterMemberController.Run(ctx, 1)
	go etcdMembersController.Run(ctx, 1)
	go bootstrapTeardownController.Run(ctx, 1)
	go unsupportedConfigOverridesController.Run(ctx, 1)
	go scriptController.Run(ctx, 1)
	go defragController.Run(ctx, 1)

	go envVarController.Run(1, ctx.Done())
	go staticPodControllers.Start(ctx)

	<-ctx.Done()
	return nil
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
	{Name: "etcd-endpoints"},
	{Name: "etcd-all-bundles"},
}

// RevisionSecrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
var RevisionSecrets = []revision.RevisionResource{
	{Name: "etcd-all-certs"},
}

var CertConfigMaps = []installer.UnrevisionedResource{
	{Name: "restore-etcd-pod"},
	{Name: "etcd-scripts"},
	{Name: "etcd-all-bundles"},
	{Name: "external-etcd-pod", Optional: true},
}

var CertSecrets = []installer.UnrevisionedResource{
	// these are also copied to certs to have a constant file location so we can refer to them in various recovery scripts
	// and in the PDB
	{Name: "etcd-all-certs"},
}

func ExtractStaticPodOperatorSpec(obj *unstructured.Unstructured, fieldManager string) (*applyoperatorv1.StaticPodOperatorSpecApplyConfiguration, error) {
	castObj := &operatorv1.Etcd{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, castObj); err != nil {
		return nil, fmt.Errorf("unable to convert to Etcd: %w", err)
	}
	ret, err := applyoperatorv1.ExtractEtcd(castObj, fieldManager)
	if err != nil {
		return nil, fmt.Errorf("unable to extract fields for %q: %w", fieldManager, err)
	}
	if ret.Spec == nil {
		return nil, nil
	}
	return &ret.Spec.StaticPodOperatorSpecApplyConfiguration, nil
}
func ExtractStaticPodOperatorStatus(obj *unstructured.Unstructured, fieldManager string) (*applyoperatorv1.StaticPodOperatorStatusApplyConfiguration, error) {
	castObj := &operatorv1.Etcd{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, castObj); err != nil {
		return nil, fmt.Errorf("unable to convert to Etcd: %w", err)
	}
	ret, err := applyoperatorv1.ExtractEtcdStatus(castObj, fieldManager)
	if err != nil {
		return nil, fmt.Errorf("unable to extract fields for %q: %w", fieldManager, err)
	}
	if ret.Status == nil {
		return nil, nil
	}
	return &ret.Status.StaticPodOperatorStatusApplyConfiguration, nil
}
