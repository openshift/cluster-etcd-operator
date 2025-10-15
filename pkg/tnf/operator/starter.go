package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclientv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// pacemakerStatusCollectorName is the name of the Pacemaker status collector CronJob
	pacemakerStatusCollectorName = "pacemaker-status-collector"
)

var (
	// fencingUpdateTriggered is set to true when a fencing update is already triggered
	fencingUpdateTriggered bool
	// fencingUpdateMutex is used to make usage of fencingUpdateTriggered thread safe
	fencingUpdateMutex sync.Mutex

	// handleNodesFunc is a variable to allow mocking in tests
	handleNodesFunc = handleNodes

	// retryBackoffConfig allows customizing retry behavior for tests
	retryBackoffConfig = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Steps:    9, // ~10 minutes total: 5s + 10s + 20s + 40s + 80s + 120s + 120s + 120s + 120s
		Cap:      2 * time.Minute,
	}
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
	dynamicClient dynamic.Interface,
	alivenessChecker *health.MultiAlivenessChecker) (bool, error) {

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
	runPacemakerControllers(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, alivenessChecker, etcdInformer)

	controlPlaneNodeLister := corev1listers.NewNodeLister(controlPlaneNodeInformer.GetIndexer())

	// we need node names for assigning auth and after-setup jobs to specific nodes
	klog.Infof("watching for nodes...")
	var once sync.Once
	_, err = controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: handleAddedNode(controllerContext, controlPlaneNodeLister, &once, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer),
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
			handleFencingSecretChange(ctx, kubeClient, nil, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			handleFencingSecretChange(ctx, kubeClient, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			handleFencingSecretChange(ctx, kubeClient, nil, obj)
		},
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to secrets informer: %v", err)
		return false, err
	}

	return true, nil
}

func handleAddedNode(
	controllerContext *controllercmd.ControllerContext,
	controlPlaneNodeLister corev1listers.NodeLister,
	once *sync.Once,
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) func(obj interface{}) {

	return func(obj interface{}) {
		node, ok := obj.(*corev1.Node)
		if !ok {
			klog.Warningf("failed to convert added object to Node %+v", obj)
			return
		}
		klog.Infof("node added: %s", node.GetName())

		// ensure we have both control plane nodes before creating jobs
		nodeList, err := controlPlaneNodeLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list control plane nodes while waiting to create TNF jobs: %v", err)
			return
		}
		if len(nodeList) != 2 {
			klog.Info("not starting TNF jobs yet, waiting for 2 control plane nodes to exist")
			return
		}
		klog.Infof("found 2 control plane nodes (%q, %q)", nodeList[0].GetName(), nodeList[1].GetName())

		// we can have 2 nodes on the first call of AddFunc already, ensure we create job controllers once only
		once.Do(func() {
			// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
			// to not block the event handler
			go handleNodesWithRetry(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		})
	}
}

func handleNodesWithRetry(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) {

	// Retry with exponential backoff to handle transient failures
	var setupErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoffConfig, func(ctx context.Context) (bool, error) {
		setupErr = handleNodesFunc(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		if setupErr != nil {
			klog.Warningf("failed to setup TNF job controllers, will retry: %v", setupErr)
			return false, nil
		}
		return true, nil
	})

	if err != nil || setupErr != nil {
		klog.Errorf("failed to setup TNF job controllers after 10 minutes of retries: %v", setupErr)

		// Degrade the operator to indicate TNF job controller setup failed
		_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "TNFJobControllersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SetupFailed",
			Message: fmt.Sprintf("Failed to setup TNF job controllers after retries: %v", setupErr),
		}))
		if updateErr != nil {
			klog.Errorf("failed to update operator status to degraded: %v", updateErr)
		}
	} else {
		// Clear any previous degraded condition on success
		_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "TNFJobControllersDegraded",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "TNF job controllers setup completed successfully",
		}))
		if updateErr != nil {
			klog.Errorf("failed to update operator status: %v", updateErr)
		}
	}
}

func handleNodes(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) error {

	if err := ceohelpers.WaitForEtcdCondition(
		ctx,
		etcdInformer,
		operatorClient,
		ceohelpers.IsEtcdRunningInCluster,
		10*time.Second,
		30*time.Minute,
		"etcd bootstrap completion",
	); err != nil {
		return fmt.Errorf("failed to wait for etcd bootstrap: %w", err)
	}
	klog.Infof("bootstrap completed, creating TNF job controllers")

	// the order of job creation does not matter, the jobs wait on each other as needed
	for _, node := range nodeList {
		runJobController(ctx, tools.JobTypeAuth, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		runJobController(ctx, tools.JobTypeAfterSetup, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
	}

	hasExternalEtcdCompletedTransition, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
	if err != nil {
		klog.Errorf("failed to get external etcd transition status; proceeding as though it has not transitioned: %v", err)
		hasExternalEtcdCompletedTransition = false
	}

	setupConditions := jobs.DefaultConditions
	if !hasExternalEtcdCompletedTransition {
		setupConditions = jobs.AllConditions
	}

	runJobController(ctx, tools.JobTypeSetup, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, setupConditions)
	runJobController(ctx, tools.JobTypeFencing, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

	return nil
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

func runPacemakerControllers(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, alivenessChecker *health.MultiAlivenessChecker, etcdInformer operatorv1informers.EtcdInformer) {
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
			alivenessChecker,
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
	// The CronJob runs "tnf-runtime monitor" which executes "sudo -n pcs status xml"
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
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command = []string{"tnf-runtime", "monitor", "-v=4"}

			return nil
		},
	)
	go statusCronJobController.Run(ctx, 1)
}

func runJobController(ctx context.Context, jobType tools.JobType, nodeName *string, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	nodeNameForLogs := "n/a"
	if nodeName != nil {
		nodeNameForLogs = *nodeName
	}
	klog.Infof("starting Two Node Fencing job controller for command %q on node %q", jobType.GetSubCommand(), nodeNameForLogs)
	tnfJobController := jobs.NewJobController(
		jobType.GetJobName(nodeName),
		bindata.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		conditions,
		[]factory.Informer{},
		[]jobs.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				if nodeName != nil {
					job.Spec.Template.Spec.NodeName = *nodeName
				}
				job.SetName(jobType.GetJobName(nodeName))
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()
				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func handleFencingSecretChange(ctx context.Context, client kubernetes.Interface, oldObj, obj interface{}) {
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

	// check if fencing update is triggered already
	fencingUpdateMutex.Lock()
	if fencingUpdateTriggered {
		klog.Infof("fencing update triggered already, skipping recreation of fencing job for secret %s", secret.GetName())
		fencingUpdateMutex.Unlock()
		return
	}
	// block further updates
	fencingUpdateTriggered = true
	fencingUpdateMutex.Unlock()

	// reset when done
	defer func() {
		fencingUpdateMutex.Lock()
		fencingUpdateTriggered = false
		fencingUpdateMutex.Unlock()
	}()

	// we need to recreate the fencing job if it exists, but don't interrupt running jobs
	klog.Infof("recreating fencing job in case it exists already, and when it's done")

	fencingJobName := tools.JobTypeFencing.GetJobName(nil)
	jobFound := false

	// helper func for waiting for a running job
	// finished = Complete, Failed, or not found
	isFencingJobFinished := func(context.Context) (finished bool, returnErr error) {
		var err error
		jobFound = false
		job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, fencingJobName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			klog.Errorf("failed to get fencing job, will retry: %v", err)
			return false, nil
		}
		jobFound = true
		if tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) || tools.IsConditionTrue(job.Status.Conditions, batchv1.JobFailed) {
			return true, nil
		}
		klog.Infof("fencing job still running, skipping recreation for now, will retry")
		return false, nil
	}

	// wait as long as the fencing job waits as well, plus some execution time
	err := wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.FencingJobCompletedTimeout, true, isFencingJobFinished)
	if err != nil {
		// if we set timeouts right, this should not happen...
		klog.Errorf("timed out waiting for fencing job to complete: %v", err)
		return
	}

	if !jobFound {
		klog.Errorf("fencing job not found, nothing to do")
		return
	}

	klog.Info("deleting fencing job for recreation")
	err = client.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, fencingJobName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		// TODO how to trigger a retry here...
		klog.Errorf("failed to delete fencing job: %v", err)
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

	return nil
}
