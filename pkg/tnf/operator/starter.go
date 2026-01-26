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
	apiextclientv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/externaletcdsupportcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
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
	runTnfResourceController(ctx, controllerContext, kubeClient, dynamicClient, operatorClient, kubeInformersForNamespaces)
	runPacemakerControllers(ctx, controllerContext, operatorClient, kubeClient, etcdInformer)

	// we need node names for assigning auth and after-setup jobs to specific nodes
	controlPlaneNodeLister := corev1listers.NewNodeLister(controlPlaneNodeInformer.GetIndexer())
	klog.Infof("watching for nodes...")
	_, err = controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert added object to Node %+v", obj)
				return
			}

			// ignore nodes which are not ready yet
			if !tools.IsNodeReady(node) {
				klog.Infof("added node %s is not ready yet, skipping handling", node.GetName())
				return
			}

			// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
			// to not block the event handler
			klog.Infof("node added and ready: %s", node.GetName())
			go handleNodesWithRetry(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, oldOk := oldObj.(*corev1.Node)
			newNode, newOk := newObj.(*corev1.Node)
			if !oldOk || !newOk {
				klog.Warningf("failed to convert updated object to Node, old=%+v, new=%+v", oldObj, newObj)
				return
			}

			// only handle if node transitioned from not ready to ready
			oldReady := tools.IsNodeReady(oldNode)
			newReady := tools.IsNodeReady(newNode)
			if !oldReady && newReady {
				klog.Infof("node %s transitioned to ready state", newNode.GetName())
				// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
				// to not block the event handler
				go handleNodesWithRetry(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert deleted object to Node %+v", obj)
				return
			}
			klog.Infof("node deleted: %s", node.GetName())

			// always handle node deletion
			// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
			// to not block the event handler
			go handleNodesWithRetry(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		},
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to control plane informer: %v", err)
		return false, err
	}

	// we need to update fencing config when fencing secrets change
	// adding a secret informer to the jobcontroller would trigger fencing setup for *every* secret change,
	// that's why we do it with an event handler
	klog.Infof("watching for secrets...")
	_, err = kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			go handleFencingSecretChange(ctx, nil, obj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			go handleFencingSecretChange(ctx, oldObj, newObj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
		},
		DeleteFunc: func(obj interface{}) {
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

func runTnfResourceController(ctx context.Context, controllerContext *controllercmd.ControllerContext, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, operatorClient v1helpers.StaticPodOperatorClient, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing static resources controller")
	tnfResourceController := staticresourcecontroller.NewStaticResourceController(
		"TnfStaticResources",
		bindata.Asset,
		[]string{
			"tnfdeployment/sa.yaml",
			"tnfdeployment/role.yaml",
			"tnfdeployment/role-binding.yaml",
			"tnfdeployment/clusterrole.yaml",
			"tnfdeployment/clusterrole-binding.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient).WithDynamicClient(dynamicClient),
		operatorClient,
		controllerContext.EventRecorder,
	).WithIgnoreNotFoundOnCreate().AddKubeInformers(kubeInformersForNamespaces)
	go tnfResourceController.Run(ctx, 1)
}

func runPacemakerControllers(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, etcdInformer operatorv1informers.EtcdInformer) {
	// Wait for external etcd transition before creating any Pacemaker controllers
	// This runs in a background goroutine to avoid blocking the main thread.
	go func() {
		klog.Infof("waiting for etcd to transition to external before creating Pacemaker controllers")

		// Wait for external etcd transition to complete
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
			// External etcd transition is complete, break out of the wait loop
			break
		}

		klog.Infof("etcd has transitioned to external; applying PacemakerCluster CRD")

		// Apply the PacemakerCluster CRD first before creating any controllers
		// Both the informer and collector will need this CRD to be registered
		// Retry until successful or context is cancelled
		for {
			if err := applyPacemakerStatusCRD(ctx, controllerContext); err != nil {
				if ctx.Err() != nil {
					klog.Infof("context done while applying PacemakerCluster CRD: %v", err)
					return
				}
				klog.Errorf("failed to apply PacemakerCluster CRD, will retry in 30s: %v", err)
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
			// CRD applied successfully, break out of the retry loop
			klog.Infof("PacemakerCluster CRD applied successfully")
			break
		}

		// Now create the healthcheck controller after transition is complete
		klog.Infof("creating Pacemaker healthcheck controller")
		healthCheckController, pacemakerInformer, err := pacemaker.NewHealthCheck(
			operatorClient,
			kubeClient,
			controllerContext.EventRecorder,
			controllerContext.KubeConfig,
		)
		if err != nil {
			klog.Errorf("Failed to create Pacemaker healthcheck controller: %v", err)
			return
		}

		// Start the PacemakerCluster informer now that the CRD exists
		// The controller will wait for this informer to sync before processing events
		klog.Infof("starting PacemakerCluster informer")
		go pacemakerInformer.Run(ctx.Done())

		// Start the healthcheck controller
		klog.Infof("starting Pacemaker healthcheck controller")
		go healthCheckController.Run(ctx, 1)

		// Start the status collector CronJob
		klog.Infof("starting Pacemaker status collector CronJob")
		runPacemakerStatusCollectorCronJob(ctx, controllerContext, operatorClient, kubeClient)
	}()
}

func runPacemakerStatusCollectorCronJob(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface) {
	// Start the cronjob controller to create a CronJob for periodic status collection
	// The CronJob runs "tnf-monitor collect" which executes "sudo -n pcs status xml"
	// and updates the PacemakerCluster CR
	klog.Infof("starting Pacemaker status collector CronJob controller")
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
	oldObj, obj interface{},
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

	err := jobs.RestartJobOrRunController(ctx, tools.JobTypeFencing, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.FencingJobCompletedTimeout)
	if err != nil {
		klog.Errorf("failed to restart fencing job: %v", err)
		return
	}
}

// applyPacemakerStatusCRD applies the PacemakerCluster CRD to the cluster.
// This must be called after external etcd transition is complete and before
// starting the status collector CronJob, as the CronJob will need to create
// PacemakerCluster custom resources.
func applyPacemakerStatusCRD(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	klog.Infof("applying PacemakerCluster CRD")

	// Read the CRD manifest from bindata
	crdBytes, err := bindata.Asset("etcd/pacemakercluster-crd.yaml")
	if err != nil {
		return fmt.Errorf("failed to read PacemakerCluster CRD from bindata: %w", err)
	}

	// Decode the CRD
	crd := &apiextensionsv1.CustomResourceDefinition{}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(crdBytes), 4096)
	if err := decoder.Decode(crd); err != nil {
		return fmt.Errorf("failed to decode PacemakerCluster CRD: %w", err)
	}

	// Get the apiextensions client
	apiextClient, err := apiextclientv1.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	// Apply the CRD
	_, updated, err := resourceapply.ApplyCustomResourceDefinitionV1(
		ctx,
		apiextClient,
		controllerContext.EventRecorder,
		crd,
	)
	if err != nil {
		return fmt.Errorf("failed to apply PacemakerCluster CRD: %w", err)
	}
	if updated {
		klog.Infof("PacemakerCluster CRD created or updated successfully")
	} else {
		klog.V(2).Infof("PacemakerCluster CRD already exists and is up to date")
	}

	// Wait for CRD to be Established before proceeding
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		got, getErr := apiextClient.CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
		if getErr != nil {
			klog.V(2).Infof("waiting for CRD %s: %v", crd.Name, getErr)
			return false, nil
		}
		for _, cond := range got.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		klog.V(2).Infof("CRD %s not yet established", crd.Name)
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("PacemakerCluster CRD not established: %w", err)
	}
	return nil
}
