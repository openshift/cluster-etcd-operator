package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/externaletcdsupportcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
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
	runPacemakerControllers(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer, controlPlaneNodeInformer)

	// we need to update fencing config when fencing secrets change
	// adding a secret informer to the jobcontroller would trigger fencing setup for *every* secret change,
	// that's why we do it with an event handler
	klog.Infof("watching for secrets...")
	_, err = kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			go handleFencingSecretChange(ctx, nil, obj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, controlPlaneNodeInformer)
		},
		UpdateFunc: func(oldObj, newObj any) {
			go handleFencingSecretChange(ctx, oldObj, newObj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, controlPlaneNodeInformer)
		},
		DeleteFunc: func(obj any) {
			// nothing to do
			// removing orphaned fence devices is handled by update-setup job
			// when handling node replacements
		},
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to secrets informer: %v", err)
		return false, err
	}

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

func runPacemakerControllers(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, etcdInformer operatorv1informers.EtcdInformer, nodeInformer cache.SharedIndexInformer) {
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
		for {
			err = wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
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
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				klog.Infof("context done while waiting for PacemakerCluster CRD: %v", err)
				return
			}
			klog.Warningf("PacemakerCluster CRD not established yet, will retry in 30s: %v", err)
			select {
			case <-time.After(30 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}

		klog.Infof("PacemakerCluster CRD is established")

		// Prerequisites met: create and start lifecycle manager controller.
		lifecycleController, lifecycleManager, pacemakerInformer, err := NewPacemakerLifecycleManager(
			operatorClient,
			kubeClient,
			controllerContext.EventRecorder,
			controllerContext.KubeConfig,
			nodeInformer,
			controllerContext,
			kubeInformersForNamespaces,
			etcdInformer,
		)
		if err != nil {
			klog.Errorf("Failed to create Pacemaker lifecycle manager: %v", err)
			return
		}

		// Start the PacemakerCluster informer (controller waits for sync before processing events).
		go pacemakerInformer.Run(ctx.Done())

		// Start the lifecycle manager controller.
		// PacemakerLifecycleManager.sync() handles both bootstrap and post-transition:
		// - Bootstrap: StartJobControllers() drives external etcd transition
		// - Post-transition: MonitorHealth(), ReconcilePacemakerConfig(), CleanupOrphanedJobs()
		go lifecycleController.Run(ctx, 1)

		// Create shared validNodeFunc for both update-setup job and status collector CronJob
		// Returns intersection of K8s (ready) nodes and pacemaker nodes, or all ready nodes if CR unavailable
		validNodeFunc := func() ([]*corev1.Node, error) {
			return lifecycleManager.getActivePacemakerNodes()
		}

		// Start the status collector CronJob with the same valid node logic
		runPacemakerStatusCollectorCronJob(ctx, controllerContext, operatorClient, kubeClient, validNodeFunc)

		klog.Infof("started Pacemaker controllers (lifecycle manager, status collector)")
	}()
}

// createFencingJobConfigFunc creates a JobConfigFunc for the fencing job.
// Returns a function that captures update-setup generation, node UIDs, and fencing secret UIDs for drift detection.
// Drift triggers fencing job restart:
// - Generation change: membership changed (node add/remove)
// - Secret UID change: BMC credentials rotated
// - Node UID change: node replaced (same name, different UID)
func createFencingJobConfigFunc(nodeList []*corev1.Node, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) jobs.JobConfigFunc {
	return func() (string, error) {
		// Get latest update-setup ConfigMap generation (detects membership changes)
		configMapsLister := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister()
		configMaps, err := configMapsLister.List(labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/component": tools.TnfUpdateSetupComponentValue,
		}))
		if err != nil {
			return "", fmt.Errorf("failed to list update-setup ConfigMaps: %w", err)
		}

		var latestGeneration int64 = 0
		for _, cm := range configMaps {
			genStr := cm.Data["generation"]
			gen, _ := strconv.ParseInt(genStr, 10, 64)
			if gen > latestGeneration {
				latestGeneration = gen
			}
		}

		// Collect node UIDs (detects node replacements)
		nodeUIDs := make([]string, len(nodeList))
		for i, node := range nodeList {
			nodeUIDs[i] = string(node.UID)
		}
		sort.Strings(nodeUIDs)

		// Collect fencing secret UIDs from informer (detects credential rotation)
		secretsLister := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister()
		allSecrets, err := secretsLister.List(labels.Everything())
		if err != nil {
			return "", fmt.Errorf("failed to list secrets: %w", err)
		}

		var secretUIDs []string
		for _, secret := range allSecrets {
			if tools.IsFencingSecret(secret.Name) {
				secretUIDs = append(secretUIDs, string(secret.UID))
			}
		}
		sort.Strings(secretUIDs)

		// Create config with generation + UIDs
		config := map[string]interface{}{
			"generation": latestGeneration,
			"nodeUIDs":   nodeUIDs,
			"secretUIDs": secretUIDs,
		}

		configJSON, err := json.Marshal(config)
		if err != nil {
			return "", fmt.Errorf("failed to marshal fencing config: %w", err)
		}

		return string(configJSON), nil
	}
}

func handleFencingSecretChange(
	ctx context.Context,
	oldObj, obj any,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	nodeInformer cache.SharedIndexInformer,
) {

	// obj can be nil, always restart fencing job in that case
	if obj != nil {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			klog.Warningf("failed to convert added / modified / deleted object to Secret %+v", obj)
			return
		}
		if !tools.IsFencingSecret(secret.GetName()) {
			// nothing to do
			return
		}

		if oldObj != nil {
			oldSecret, ok := oldObj.(*corev1.Secret)
			if !ok {
				klog.Warningf("failed to convert old object to Secret %+v", oldObj)
			}
			// check if data changed
			changed := false
			if len(oldSecret.Data) != len(secret.Data) {
				changed = true
			} else {
				for key, oldValue := range oldSecret.Data {
					newValue, exists := secret.Data[key]
					if !exists || !bytes.Equal(oldValue, newValue) {
						changed = true
						break
					}
				}
			}
			if !changed {
				return
			}
			klog.Infof("handling modified fencing secret %s", secret.GetName())
		} else {
			klog.Infof("handling added or deleted fencing secret %s", secret.GetName())
		}
	}

	// OCPBUGS-84695: before handoff, ignore secret-driven fencing (install orders fencing via startTnfJobcontrollers).
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
	if err != nil {
		klog.Errorf("failed to check external etcd transition for fencing secret handler: %v", err)
		return
	}
	if !transitionComplete {
		klog.V(4).Infof("skipping fencing job restart from fencing secret change until %s is True",
			ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition)
		return
	}

	// Get control plane nodes for jobConfigFunc
	nodes, err := ceohelpers.ListNodesFromInformer(nodeInformer)
	if err != nil {
		klog.Errorf("failed to list control plane nodes for fencing job config: %v", err)
		return
	}

	// Create jobConfigFunc to capture node UIDs + fencing secret UIDs for drift detection
	fencingJobConfigFunc := createFencingJobConfigFunc(nodes, kubeInformersForNamespaces)

	// Restart fencing job with drift detection
	err = jobs.RestartJobOrRunController(ctx, tools.JobTypeFencing, nil, nil, fencingJobConfigFunc, 3, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.FencingJobCompletedTimeout)
	if err != nil {
		klog.Errorf("failed to restart fencing job: %v", err)
		return
	}
}
