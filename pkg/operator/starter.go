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
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	machineinformersv1beta1 "github.com/openshift/client-go/machine/informers/externalversions/machine/v1beta1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
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
	"github.com/openshift/cluster-etcd-operator/pkg/operator/metriccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/quorumguardcleanup"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/resourcesynccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/scriptcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/targetconfigcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/upgradebackupcontroller"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staleconditions"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/guard"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/unsupportedconfigoverridescontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// masterMachineLabelSelectorString allows for getting only the master machines, it matters in larger installations with many worker nodes
const masterMachineLabelSelectorString = "machine.openshift.io/cluster-api-machine-role=master"

// masterNodeLabelSelectorString allows for getting only the master nodes, it matters in larger installations with many worker nodes
const masterNodeLabelSelectorString = "node-role.kubernetes.io/master"

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
	machineClientSet, err := machineclient.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}

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
	// Don't set operator version. library-go will take care of it after setting operands.
	versionRecorder.SetVersion("raw-internal", status.VersionForOperatorFromEnv())

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
		isSNO, precheckSucceeded, err := guard.IsSNOCheckFnc(configInformers.Config().V1().Infrastructures())()
		return !isSNO, precheckSucceeded, err
	}

	staticPodControllers, err := staticpod.NewBuilder(operatorClient, kubeClient, kubeInformersForNamespaces).
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
		coreClient,
		operatorClient,
		kubeInformersForNamespaces,
		controllerContext.EventRecorder,
	)

	etcdEndpointsController := etcdendpointscontroller.NewEtcdEndpointsController(
		operatorClient,
		etcdClient,
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

	clusterMemberRemovalController := clustermemberremovalcontroller.NewClusterMemberRemovalController(
		operatorClient,
		etcdClient,
		ceohelpers.NewMachineAPI(masterMachineInformer, machinelistersv1beta1.NewMachineLister(masterMachineInformer.GetIndexer()), masterMachineLabelSelector),
		masterMachineLabelSelector, masterNodeLabelSelector,
		kubeInformersForNamespaces,
		masterNodeInformer,
		masterMachineInformer,
		configInformers.Config().V1().Networks(),
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
		configInformers.Config().V1().Infrastructures().Lister(),
	)

	scriptController := scriptcontroller.NewScriptControllerController(
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		envVarController,
		controllerContext.EventRecorder,
	)

	quorumGuardCleanupController := quorumguardcleanup.NewQuorumGuardCleanupController(
		status.VersionForOperatorFromEnv(),
		operatorClient,
		kubeClient,
		configInformers.Config().V1().ClusterVersions(),
		configInformers.Config().V1().ClusterOperators(),
		kubeInformersForNamespaces,
		masterNodeInformer,
		configInformers.Config().V1().Infrastructures(),
		controllerContext.EventRecorder,
	)

	defragController := defragcontroller.NewDefragController(
		operatorClient,
		etcdClient,
		configInformers.Config().V1().Infrastructures().Lister(),
		controllerContext.EventRecorder,
	)

	upgradeBackupController := upgradebackupcontroller.NewUpgradeBackupController(
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

	unsupportedConfigOverridesController := unsupportedconfigoverridescontroller.NewUnsupportedConfigOverridesController(
		operatorClient,
		controllerContext.EventRecorder,
	)

	staleConditions := staleconditions.NewRemoveStaleConditionsController(
		[]string{
			// QuorumGuardController was removed in 4.11, remove its stale conditions to avoid blocking upgrades from older clusters
			// that might have this condition
			"QuorumGuardControllerDegraded",
		},
		operatorClient,
		controllerContext.EventRecorder,
	)

	operatorInformers.Start(ctx.Done())
	kubeInformersForNamespaces.Start(ctx.Done())
	configInformers.Start(ctx.Done())
	dynamicInformers.Start(ctx.Done())
	go masterMachineInformer.Run(ctx.Done())
	go masterNodeInformer.Run(ctx.Done())

	go staleConditions.Run(ctx, 1)
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
	go etcdMembersController.Run(ctx, 1)
	go bootstrapTeardownController.Run(ctx, 1)
	go unsupportedConfigOverridesController.Run(ctx, 1)
	go scriptController.Run(ctx, 1)
	go quorumGuardCleanupController.Run(ctx, 1)
	go defragController.Run(ctx, 1)
	go upgradeBackupController.Run(ctx, 1)

	go envVarController.Run(1, ctx.Done())
	go staticPodControllers.Start(ctx)

	<-ctx.Done()
	return nil
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
