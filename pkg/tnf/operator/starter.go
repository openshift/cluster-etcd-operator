package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

const (
	// pacemakerStatusCollectorName is the name of the Pacemaker status collector CronJob
	pacemakerStatusCollectorName = "pacemaker-status-collector"
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
			go handleFencingSecretChange(ctx, nil, obj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
		},
		UpdateFunc: func(oldObj, newObj any) {
			go handleFencingSecretChange(ctx, oldObj, newObj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
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
	// Pacemaker controllers start after: (1) external etcd transition completes, (2) PacemakerCluster CRD is established.
	// This runs in a background goroutine to avoid blocking the main thread.
	go func() {
		klog.Infof("waiting for prerequisites before starting Pacemaker controllers (etcd transition, CRD established)")

		// Wait for external etcd transition to complete.
		for {
			if err := ceohelpers.WaitForEtcdCondition(
				ctx, etcdInformer, operatorClient, ceohelpers.HasExternalEtcdCompletedTransition,
				10*time.Second, 30*time.Minute, "external etcd transition",
			); err != nil {
				if ctx.Err() != nil {
					klog.Infof("context done while waiting for external etcd transition: %v", err)
					return
				}
				klog.Warningf("external etcd transition not complete yet, will retry in 1m: %v", err)
				select {
				case <-time.After(time.Minute):
					continue
				case <-ctx.Done():
					return
				}
			}
			break
		}

		klog.Infof("etcd has transitioned to external; verifying PacemakerCluster CRD is established")

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
		lifecycleController, _, pacemakerInformer, err := NewPacemakerLifecycleManager(
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
		// PacemakerLifecycleManager handles: (1) Ready transitions for bootstrap, (2) Add/Delete for drift reconciliation.
		go lifecycleController.Run(ctx, 1)

		// Start the status collector CronJob.
		runPacemakerStatusCollectorCronJob(ctx, controllerContext, operatorClient, kubeClient)

		klog.Infof("started Pacemaker controllers (lifecycle manager, status collector)")
	}()
}

func runPacemakerStatusCollectorCronJob(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface) {
	// Start the cronjob controller to create a CronJob for periodic status collection.
	// The CronJob runs "tnf-monitor collect" which executes "sudo -n pcs status xml" and updates the PacemakerCluster CR.
	statusCronJobController := jobs.NewCronJobController(
		pacemakerStatusCollectorName,
		bindata.MustAsset("tnfdeployment/cronjob.yaml"),
		operatorClient,
		kubeClient,
		controllerContext.EventRecorder,
		func(_ *operatorv1.OperatorSpec, cronJob *batchv1.CronJob) error {
			// Set the name and namespace
			cronJob.SetName(pacemakerStatusCollectorName)
			cronJob.SetNamespace(operatorclient.TargetNamespace)

			// Set the schedule - run every minute
			cronJob.Spec.Schedule = "* * * * *"

			// Initialize labels maps if nil and set labels at all levels
			if cronJob.Labels == nil {
				cronJob.Labels = make(map[string]string)
			}
			cronJob.Labels["app.kubernetes.io/name"] = pacemakerStatusCollectorName

			if cronJob.Spec.JobTemplate.Labels == nil {
				cronJob.Spec.JobTemplate.Labels = make(map[string]string)
			}
			cronJob.Spec.JobTemplate.Labels["app.kubernetes.io/name"] = pacemakerStatusCollectorName

			if cronJob.Spec.JobTemplate.Spec.Template.Labels == nil {
				cronJob.Spec.JobTemplate.Spec.Template.Labels = make(map[string]string)
			}
			cronJob.Spec.JobTemplate.Spec.Template.Labels["app.kubernetes.io/name"] = pacemakerStatusCollectorName

			// Configure the container
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command = []string{"tnf-monitor", "collect", "-v=4"}

			return nil
		},
	)
	go statusCronJobController.Run(ctx, 1)
}

func handleFencingSecretChange(
	ctx context.Context,
	oldObj, obj any,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
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

	err = jobs.RestartJobOrRunController(ctx, tools.JobTypeFencing, nil, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.FencingJobCompletedTimeout)
	if err != nil {
		klog.Errorf("failed to restart fencing job: %v", err)
		return
	}
}
