package operator

import (
	"context"
	"fmt"
	"os"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configv1informersfactory "github.com/openshift/client-go/config/informers/externalversions"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
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

const (
	conditionTypePacemakerLifecycleManagerDegraded = "PacemakerLifecycleManagerDegraded"
	reasonPacemakerLifecycleManagerStartupFailed   = "StartupFailed"
)

func setPacemakerLifecycleManagerStartupCondition(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient, startupErr error) error {
	condition := operatorv1.OperatorCondition{
		Type:   conditionTypePacemakerLifecycleManagerDegraded,
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}
	if startupErr != nil {
		condition.Status = operatorv1.ConditionTrue
		condition.Reason = reasonPacemakerLifecycleManagerStartupFailed
		condition.Message = fmt.Sprintf("Failed to create Pacemaker lifecycle manager: %v", startupErr)
	}
	_, _, err := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(condition))
	return err
}

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
	dynamicClient dynamic.Interface,
	featureGate featuregates.FeatureGate) (bool, error) {

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

	alertAgentStatusEnabled := pacemaker.AlertAgentStatusEnabled(featureGate)
	klog.Infof("starting Two Node Fencing controllers (TNFObservability=%t)", alertAgentStatusEnabled)

	runExternalEtcdSupportController(ctx, controllerContext, operatorClient, envVarGetter, kubeInformersForNamespaces,
		infrastructureInformer, networkInformer, controlPlaneNodeInformer, etcdInformer, kubeClient)
	if err := runTnfResourceController(ctx, controllerContext, kubeClient, dynamicClient, operatorClient, kubeInformersForNamespaces, alertAgentStatusEnabled); err != nil {
		return false, err
	}
	// Start pacemaker controllers (lifecycle manager, status collector)
	// Node and Pacemaker informer events enqueue lifecycle reconciliation.
	runPacemakerControllers(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer, controlPlaneNodeInformer, dynamicClient, alertAgentStatusEnabled)

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

func runTnfResourceController(ctx context.Context, controllerContext *controllercmd.ControllerContext, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, operatorClient v1helpers.StaticPodOperatorClient, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, alertAgentStatusEnabled bool) error {
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
			pacemakerCRDAsset(alertAgentStatusEnabled),
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient).WithDynamicClient(dynamicClient).WithAPIExtensionsClient(apiextClient),
		operatorClient,
		controllerContext.EventRecorder,
	).WithIgnoreNotFoundOnCreate().AddKubeInformers(kubeInformersForNamespaces)
	go tnfResourceController.Run(ctx, 1)
	return nil
}

func runPacemakerControllers(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, etcdInformer operatorv1informers.EtcdInformer, controlPlaneNodeInformer cache.SharedIndexInformer, dynamicClient dynamic.Interface, alertAgentStatusEnabled bool) {
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
		lastWaitWarning := time.Now()
		err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			crd, getErr := apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "pacemakerclusters.etcd.openshift.io", metav1.GetOptions{})
			if getErr == nil && pacemakerCRDReady(crd, alertAgentStatusEnabled) {
				return true, nil
			}
			message := fmt.Sprintf("waiting for established PacemakerCluster CRD with TNFObservability=%t", alertAgentStatusEnabled)
			if getErr != nil {
				message = fmt.Sprintf("waiting for PacemakerCluster CRD: %v", getErr)
			}
			klog.V(2).Info(message)
			// Allow normal CRD convergence before warning, then repeat at most every five minutes.
			if time.Since(lastWaitWarning) >= 5*time.Minute {
				klog.Warningf("Pacemaker controllers have not started: %s; check TnfStaticResources controller errors and the PacemakerCluster CRD", message)
				lastWaitWarning = time.Now()
			}
			return false, nil
		})
		if err != nil {
			klog.Infof("context done while waiting for PacemakerCluster CRD: %v", err)
			return
		}

		klog.Infof("PacemakerCluster CRD is established")

		// ClusterVersion notifications feed the console controller; lifecycle
		// admission uses control-plane Node MCO annotations.
		configClient, err := configv1client.NewForConfig(controllerContext.KubeConfig)
		if err != nil {
			klog.Errorf("failed to create config client: %v", err)
			return
		}
		configInformers := configv1informersfactory.NewSharedInformerFactory(configClient, 10*time.Minute)
		clusterVersions := configInformers.Config().V1().ClusterVersions()
		clusterVersionLister := clusterVersions.Lister()
		configInformers.Start(ctx.Done())

		// Prerequisites met: create and start lifecycle manager controller.
		// Retry with backoff to handle transient errors (e.g., API server unavailable).
		var lifecycleController factory.Controller
		var pacemakerInformer cache.SharedIndexInformer
		err = wait.PollUntilContextCancel(ctx, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			var createErr error
			lifecycleController, _, pacemakerInformer, createErr = newPacemakerLifecycleManager(
				ctx,
				operatorClient,
				kubeClient,
				controllerContext.EventRecorder,
				controllerContext.KubeConfig,
				controlPlaneNodeInformer,
				controllerContext,
				kubeInformersForNamespaces,
				etcdInformer,
				alertAgentStatusEnabled,
			)
			if createErr != nil {
				klog.Errorf("Failed to create Pacemaker lifecycle manager, will retry: %v", createErr)
				if statusErr := setPacemakerLifecycleManagerStartupCondition(ctx, operatorClient, createErr); statusErr != nil {
					klog.Errorf("Failed to report Pacemaker lifecycle manager startup failure: %v", statusErr)
				}
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			klog.Errorf("Context cancelled while creating Pacemaker lifecycle manager: %v", err)
			return
		}

		// Start the PacemakerCluster informer (controller waits for sync before processing events).
		go pacemakerInformer.Run(ctx.Done())
		// The lifecycle constructor may register additional namespace informers
		// after the main operator has started the shared factory.
		kubeInformersForNamespaces.Start(ctx.Done())

		// Start the lifecycle manager controller.
		// PacemakerLifecycleManager.sync() handles both bootstrap and post-transition:
		// - Bootstrap: StartJobControllers() drives external etcd transition
		// - Post-transition: start runtime controllers and reconcile recorded generations
		go lifecycleController.Run(ctx, 1)
		go func() {
			_ = wait.PollUntilContextCancel(ctx, 30*time.Second, true, func(ctx context.Context) (bool, error) {
				if err := setPacemakerLifecycleManagerStartupCondition(ctx, operatorClient, nil); err != nil {
					klog.Errorf("Failed to clear Pacemaker lifecycle manager startup condition, will retry: %v", err)
					return false, nil
				}
				return true, nil
			})
		}()

		// Create and start the metrics controller, sharing the same informer
		klog.Infof("creating Pacemaker metrics controller")
		metricsController := metriccontroller.NewPacemakerMetricsController(
			pacemakerInformer,
			controllerContext.EventRecorder,
			legacyregistry.DefaultGatherer.(metrics.KubeRegistry),
		)
		klog.Infof("starting Pacemaker metrics controller")
		go metricsController.Run(ctx, 1)

		configInformers.WaitForCacheSync(ctx.Done())

		// Create and start the console notification controller, sharing the same informer
		klog.Infof("creating Pacemaker console notification controller")
		notificationController := pacemaker.NewConsoleNotificationController(
			pacemakerInformer,
			dynamicClient,
			clusterVersionLister,
			controllerContext.EventRecorder,
			alertAgentStatusEnabled,
		)
		klog.Infof("starting Pacemaker console notification controller")
		go notificationController.Run(ctx, 1)

		// Note: Status collector is started by lifecycle manager only after transition is complete
		// (see startJobControllersWithLock in job_controllers.go)

		klog.Infof("started Pacemaker controllers (lifecycle manager, metrics controller, console notification)")
	}()
}

// Select by the resolved gate, including CustomNoUpgrade overrides, rather than the feature-set name.
func pacemakerCRDAsset(alertAgentStatusEnabled bool) string {
	if alertAgentStatusEnabled {
		return "etcd/pacemakercluster-tnf-observability-crd.yaml"
	}
	return "etcd/pacemakercluster-crd.yaml"
}

// Established may still describe the previous release's schema during an upgrade.
// Start the controllers only after the served v1 schema matches the resolved gate.
func pacemakerCRDReady(crd *apiextensionsv1.CustomResourceDefinition, alertAgentStatusEnabled bool) bool {
	established := false
	for _, condition := range crd.Status.Conditions {
		if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
			established = true
		}
	}
	if !established {
		return false
	}
	for _, version := range crd.Spec.Versions {
		if version.Name != "v1" || !version.Served || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		nodes := version.Schema.OpenAPIV3Schema.Properties["status"].Properties["nodes"]
		if nodes.Items == nil || nodes.Items.Schema == nil {
			return false
		}
		_, hasAlertAgents := nodes.Items.Schema.Properties["alertAgents"]
		return hasAlertAgents == alertAgentStatusEnabled
	}
	return false
}
