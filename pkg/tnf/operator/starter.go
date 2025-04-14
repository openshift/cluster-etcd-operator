package operator

import (
	"context"
	"fmt"
	"os"

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
	tnflibgo "github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/library-go"
)

// HandleDualReplicaClusters checks feature gate and control plane topology,
// and handles dual replica aka two node fencing clusters
func HandleDualReplicaClusters(
	ctx context.Context, controllerContext *controllercmd.ControllerContext,
	featureGateAccessor featuregates.FeatureGateAccess,
	configInformers configv1informers.SharedInformerFactory,
	operatorClient v1helpers.StaticPodOperatorClient,
	envVarController *etcdenvvar.EnvVarController,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	networkInformer v1.NetworkInformer,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	kubeClient *kubernetes.Clientset,
	dynamicClient *dynamic.DynamicClient) error {

	if isDualReplicaTopology, err := isDualReplicaTopoly(ctx, featureGateAccessor, configInformers); err != nil {
		return err
	} else if !isDualReplicaTopology {
		return nil
	}

	klog.Infof("detected DualReplica topology")

	runExternalEtcdSupportController(ctx, controllerContext, operatorClient, envVarController, kubeInformersForNamespaces, configInformers, networkInformer, controlPlaneNodeInformer, kubeClient)
	runTnfResourceController(ctx, controllerContext, kubeClient, dynamicClient, operatorClient, kubeInformersForNamespaces)

	// we need node names for assigning auth jobs to specific nodes
	klog.Infof("watching for nodes...")
	_, err := controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert node to Node %+v", obj)
			}
			runTnfAuthJobController(ctx, node.GetName(), controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
		},
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to control plane informer: %v", err)
	}

	runTnfSetupJobController(ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)

	return nil
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
	operatorClient v1helpers.StaticPodOperatorClient, envVarController *etcdenvvar.EnvVarController,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, configInformers configv1informers.SharedInformerFactory,
	networkInformer v1.NetworkInformer, controlPlaneNodeInformer cache.SharedIndexInformer, kubeClient *kubernetes.Clientset) {

	klog.Infof("starting external etcd support controller")
	externalEtcdSupportController := externaletcdsupportcontroller.NewExternalEtcdEnablerController(
		operatorClient,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		envVarController,
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

func runTnfResourceController(ctx context.Context, controllerContext *controllercmd.ControllerContext, kubeClient *kubernetes.Clientset, dynamicClient *dynamic.DynamicClient, operatorClient v1helpers.StaticPodOperatorClient, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
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

func runTnfAuthJobController(ctx context.Context, nodeName string, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient *kubernetes.Clientset, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing auth job controller")
	tnfJobController := tnflibgo.NewJobController(
		"TnfJob-"+nodeName,
		tnf_assets.MustAsset("tnfdeployment/authjob.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		[]factory.Informer{},
		[]tnflibgo.JobHookFunc{
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

func runTnfSetupJobController(ctx context.Context, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient *kubernetes.Clientset, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing setup job controller")
	tnfJobController := tnflibgo.NewJobController(
		"TnfSetupJob",
		tnf_assets.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		// TODO add secret informer here for rerunning setup with modified fencing credentials
		[]factory.Informer{},
		[]tnflibgo.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				// set operator image pullspec
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")

				// set env var with etcd image pullspec
				env := job.Spec.Template.Spec.Containers[0].Env
				if env == nil {
					env = []corev1.EnvVar{}
				}
				env = append(env, corev1.EnvVar{
					Name:  "ETCD_IMAGE_PULLSPEC",
					Value: os.Getenv("IMAGE"),
				})
				job.Spec.Template.Spec.Containers[0].Env = env

				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}
