package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	v1 "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/externaletcdsupportcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	tnf_assets "github.com/openshift/cluster-etcd-operator/pkg/tnf/assets"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/operator/dualreplicahelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

var (
	// fencingUpdateTriggered is set to true when a fencing update is already triggered
	fencingUpdateTriggered bool
	// fencingUpdateMutex is used to make usage of fencingUpdateTriggered thread safe
	fencingUpdateMutex sync.Mutex
)

// HandleDualReplicaClusters checks feature gate and control plane topology,
// and handles dual replica aka two node fencing clusters
func HandleDualReplicaClusters(
	ctx context.Context, controllerContext *controllercmd.ControllerContext,
	featureGateAccessor featuregates.FeatureGateAccess,
	configInformers configv1informers.SharedInformerFactory,
	operatorClient v1helpers.StaticPodOperatorClient,
	envVarGetter etcdenvvar.EnvVar,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	networkInformer v1.NetworkInformer,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	kubeClient kubernetes.Interface,
	dynamicClient dynamic.Interface) (bool, error) {

	if isDualReplicaTopology, err := isDualReplicaTopoly(ctx, featureGateAccessor, configInformers); err != nil {
		return false, err
	} else if !isDualReplicaTopology {
		return false, nil
	}

	klog.Infof("detected DualReplica topology")

	runExternalEtcdSupportController(ctx, controllerContext, operatorClient, envVarGetter, kubeInformersForNamespaces, configInformers, networkInformer, controlPlaneNodeInformer, kubeClient)
	runTnfResourceController(ctx, controllerContext, kubeClient, dynamicClient, operatorClient, kubeInformersForNamespaces)

	// we need node names for assigning auth and after-setup jobs to specific nodes
	klog.Infof("watching for nodes...")
	_, err := controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert added object to Node %+v", obj)
			}
			runTnfAuthJobController(ctx, node.GetName(), controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
			runTnfAfterSetupJobController(ctx, node.GetName(), controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
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

	runTnfSetupJobController(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
	runTnfFencingJobController(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)

	return true, nil
}

func isDualReplicaTopoly(ctx context.Context, featureGateAccessor featuregates.FeatureGateAccess, configInformers configv1informers.SharedInformerFactory) (bool, error) {
	if isDualReplicaTopology, err := ceohelpers.IsDualReplicaTopology(ctx, configInformers.Config().V1().Infrastructures().Lister()); err != nil {
		return false, fmt.Errorf("could not determine DualReplicaTopology, aborting controller start: %w", err)
	} else if !isDualReplicaTopology {
		return false, nil
	}
	// dual replica currently topology has to be enabled by a feature gate
	if enabledDualReplicaFeature, err := dualreplicahelpers.DualReplicaFeatureGateEnabled(featureGateAccessor); err != nil {
		return false, fmt.Errorf("could not determine DualReplicaFeatureGateEnabled, aborting controller start: %w", err)
	} else if !enabledDualReplicaFeature {
		return false, fmt.Errorf("detected dual replica topology, but dual replica feature gate is disabled, aborting controller start")
	}
	return true, nil
}

func runExternalEtcdSupportController(ctx context.Context, controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient, envVarGetter etcdenvvar.EnvVar,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, configInformers configv1informers.SharedInformerFactory,
	networkInformer v1.NetworkInformer, controlPlaneNodeInformer cache.SharedIndexInformer, kubeClient kubernetes.Interface) {

	klog.Infof("starting external etcd support controller")
	externalEtcdSupportController := externaletcdsupportcontroller.NewExternalEtcdEnablerController(
		operatorClient,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		envVarGetter,
		kubeInformersForNamespaces.InformersFor("openshift-etcd"),
		kubeInformersForNamespaces,
		configInformers.Config().V1().Infrastructures(),
		networkInformer,
		controlPlaneNodeInformer,
		kubeClient,
		controllerContext.EventRecorder,
	)
	go externalEtcdSupportController.Run(ctx, 1)
}

func runTnfResourceController(ctx context.Context, controllerContext *controllercmd.ControllerContext, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, operatorClient v1helpers.StaticPodOperatorClient, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing static resources controller")
	tnfResourceController := staticresourcecontroller.NewStaticResourceController(
		"TnfStaticResources",
		tnf_assets.Asset,
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

func runTnfAuthJobController(ctx context.Context, nodeName string, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing auth job controller for node %s", nodeName)
	tnfJobController := jobs.NewJobController(
		"TnfAuthJob-"+nodeName,
		tnf_assets.MustAsset("tnfdeployment/authjob.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		[]factory.Informer{},
		[]jobs.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				// set operator image pullspec
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")

				// assign to node
				job.SetName(job.GetName() + "-" + nodeName)
				job.Spec.Template.Spec.NodeName = nodeName

				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func runTnfSetupJobController(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing setup job controller")
	tnfJobController := jobs.NewJobController(
		"TnfSetupJob",
		tnf_assets.MustAsset("tnfdeployment/setupjob.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		[]factory.Informer{},
		[]jobs.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				// set operator image pullspec
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func runTnfAfterSetupJobController(ctx context.Context, nodeName string, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing after setup job controller for node %s", nodeName)
	tnfJobController := jobs.NewJobController(
		"TnfAfterSetupJob-"+nodeName,
		tnf_assets.MustAsset("tnfdeployment/aftersetupjob.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		[]factory.Informer{},
		[]jobs.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				// set operator image pullspec
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")

				// assign to node
				job.SetName(job.GetName() + "-" + nodeName)
				job.Spec.Template.Spec.NodeName = nodeName

				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func runTnfFencingJobController(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing pacemaker fencing job controller")
	tnfJobController := jobs.NewJobController(
		"TnfFencingJob",
		tnf_assets.MustAsset("tnfdeployment/fencingjob.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		[]factory.Informer{},
		[]jobs.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				// set operator image pullspec
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func handleFencingSecretChange(ctx context.Context, client kubernetes.Interface, oldObj, obj interface{}) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		klog.Warningf("failed to convert added / modifed / deleted object to Secret %+v", obj)
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

	fencingJobName := "tnf-fencing"
	jobFound := false

	isFencingJobRunning := func(context.Context) (done bool, returnErr error) {
		var err error
		jobFound = false
		job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, fencingJobName, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				klog.Errorf("failed to get fencing job, will retry: %v", err)
				return false, nil
			}
			klog.Infof("fencing job not found, skipping recreation")
			return true, nil
		}
		jobFound = true
		if tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) || tools.IsConditionTrue(job.Status.Conditions, batchv1.JobFailed) {
			return true, nil
		}

		klog.Infof("fencing job still running, skipping recreation for now, will retry")
		return false, nil
	}

	// wait as long as the fencing job waits as well, plus some execution time
	err := wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.FencingJobCompletedTimeout, true, isFencingJobRunning)
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
