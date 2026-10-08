package operator

import (
	"context"
	"fmt"
	"os"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/externaletcdsupportcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/metriccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

// HandleDualReplicaClusters checks feature gate and control plane topology,
// and handles dual replica aka two node fencing clusters
func HandleDualReplicaClusters(
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	infrastructureInformer configv1informers.InfrastructureInformer,
	operatorClient v1helpers.StaticPodOperatorClient,
	envVarGetter etcdenvvar.EnvVar,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	etcdInformer operatorv1informers.EtcdInformer,
	kubeClient kubernetes.Interface,
	dynamicClient dynamic.Interface) (bool, error) {

	// Since HandleDualReplicaClusters isn't a controller, we need to ensure that the Infrastructure
	// informer is synced before we use it.
	if !cache.WaitForCacheSync(ctx.Done(), infrastructureInformer.Informer().HasSynced) {
		return false, fmt.Errorf("failed to sync Infrastructure informer")
	}
	isExternalEtcdCluster, err := ceohelpers.IsExternalEtcdCluster(ctx, infrastructureInformer.Lister())
	if err != nil {
		klog.Errorf("failed to check if external etcd cluster is enabled: %v", err)
		return false, err
	}
	if !isExternalEtcdCluster {
		return false, nil
	}

	klog.Infof("starting Two Node Fencing controllers")

	runExternalEtcdSupportController(ctx, controllerContext, operatorClient, envVarGetter, kubeInformersForNamespaces,
		infrastructureInformer, networkInformer, controlPlaneNodeInformer, etcdInformer, kubeClient)
	if err := runTnfResourceController(ctx, controllerContext, kubeClient, dynamicClient, operatorClient, kubeInformersForNamespaces); err != nil {
		return false, err
	}
	// Start pacemaker controllers (lifecycle manager, status collector)
	// PacemakerLifecycleManager handles ALL node lifecycle events:
	//  - UpdateFunc: Ready transitions for initial bootstrap
	//  - AddFunc/DeleteFunc: drift-driven reconciliation
	// Secret handler registration happens inside runPacemakerControllers after lifecycleManager is created
	runPacemakerControllers(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer, controlPlaneNodeInformer, dynamicClient)

	return true, nil
}

func runExternalEtcdSupportController(ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	envVarGetter etcdenvvar.EnvVar,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	etcdInformer operatorv1informers.EtcdInformer,
	kubeClient kubernetes.Interface) {

	klog.Infof("starting external etcd support controller")
	externalEtcdSupportController := externaletcdsupportcontroller.NewExternalEtcdEnablerController(
		operatorClient,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		envVarGetter,
		kubeInformersForNamespaces.InformersFor("openshift-etcd"),
		kubeInformersForNamespaces,
		infrastructureInformer,
		networkInformer,
		controlPlaneNodeInformer,
		etcdInformer,
		kubeClient,
		controllerContext.EventRecorder,
	)
	go externalEtcdSupportController.Run(ctx, 1)
}

func runTnfResourceController(ctx context.Context, controllerContext *controllercmd.ControllerContext, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, operatorClient v1helpers.StaticPodOperatorClient, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) error {
	klog.Infof("starting Two Node Fencing static resources controller")

	// Get the apiextensions client for CRD management
	apiextClient, err := apiextensionsclient.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	tnfResourceController := staticresourcecontroller.NewStaticResourceController(
		"TnfStaticResources",
		bindata.Asset,
		[]string{
			"tnfdeployment/sa.yaml",
			"tnfdeployment/role.yaml",
			"tnfdeployment/role-binding.yaml",
			"tnfdeployment/clusterrole.yaml",
			"tnfdeployment/clusterrole-binding.yaml",
			"etcd/pacemakercluster-crd.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient).WithDynamicClient(dynamicClient).WithAPIExtensionsClient(apiextClient),
		operatorClient,
		controllerContext.EventRecorder,
	).WithIgnoreNotFoundOnCreate().AddKubeInformers(kubeInformersForNamespaces)
	go tnfResourceController.Run(ctx, 1)
	return nil
}

func runPacemakerControllers(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, etcdInformer operatorv1informers.EtcdInformer, controlPlaneNodeInformer cache.SharedIndexInformer, dynamicClient dynamic.Interface) {
	// Pacemaker controllers start after PacemakerCluster CRD is established.
	// The lifecycle manager's sync() handles bootstrap vs post-transition modes internally.
	// This runs in a background goroutine to avoid blocking the main thread.
	go func() {
		klog.Infof("waiting for PacemakerCluster CRD to be established before starting Pacemaker controllers")

		// The PacemakerCluster CRD is applied by the static resource controller.
		// Wait for it to be established before starting the informer.
		apiextClient, err := apiextensionsclient.NewForConfig(controllerContext.KubeConfig)
		if err != nil {
			klog.Errorf("failed to create apiextensions client: %v", err)
			return
		}

		// Wait for CRD to be established.
		err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			crd, getErr := apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "pacemakerclusters.etcd.openshift.io", metav1.GetOptions{})
			if getErr != nil {
				klog.V(2).Infof("waiting for PacemakerCluster CRD: %v", getErr)
				return false, nil
			}
			for _, cond := range crd.Status.Conditions {
				if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
					return true, nil
				}
			}
			klog.V(2).Infof("PacemakerCluster CRD not yet established")
			return false, nil
		})
		if err != nil {
			klog.Infof("context done while waiting for PacemakerCluster CRD: %v", err)
			return
		}

		klog.Infof("PacemakerCluster CRD is established")

		// Prerequisites met: create and start lifecycle manager controller.
		lifecycleController, _, pacemakerInformer, err := newPacemakerLifecycleManager(
			operatorClient,
			kubeClient,
			controllerContext.EventRecorder,
			controllerContext.KubeConfig,
			controlPlaneNodeInformer,
			controllerContext,
			kubeInformersForNamespaces,
			etcdInformer,
		)
		if err != nil {
			klog.Fatalf("Failed to create Pacemaker lifecycle manager: %v", err)
		}

		// Start the PacemakerCluster informer (controller waits for sync before processing events).
		go pacemakerInformer.Run(ctx.Done())

		// Start the lifecycle manager controller.
		// PacemakerLifecycleManager.sync() handles both bootstrap and post-transition:
		// - Bootstrap: StartJobControllers() drives external etcd transition
		// - Post-transition: MonitorHealth(), ReconcilePacemakerConfig(), CleanupOrphanedJobs()
		go lifecycleController.Run(ctx, 1)

		// Create and start the metrics controller, sharing the same informer
		klog.Infof("creating Pacemaker metrics controller")
		metricsController := metriccontroller.NewPacemakerMetricsController(
			pacemakerInformer,
			controllerContext.EventRecorder,
			legacyregistry.DefaultGatherer.(metrics.KubeRegistry),
		)
		klog.Infof("starting Pacemaker metrics controller")
		go metricsController.Run(ctx, 1)

		// Create and start the console notification controller, sharing the same informer
		klog.Infof("creating Pacemaker console notification controller")
		notificationController := pacemaker.NewConsoleNotificationController(
			pacemakerInformer,
			dynamicClient,
			controllerContext.EventRecorder,
		)
		klog.Infof("starting Pacemaker console notification controller")
		go notificationController.Run(ctx, 1)

		// Note: Status collector is started by lifecycle manager only after transition is complete
		// (see startJobControllersWithLock in job_controllers.go)

		klog.Infof("started Pacemaker controllers (lifecycle manager, metrics controller, console notification)")
	}()
}
