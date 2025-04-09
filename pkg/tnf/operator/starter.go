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
	tnflibgo "github.com/openshift/cluster-etcd-operator/pkg/tnf/library-go"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/operator/dualreplicahelpers"
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

	if enabledDualReplicaFeature, err := dualreplicahelpers.DualReplicaFeatureGateEnabled(featureGateAccessor); err != nil {
		return fmt.Errorf("could not determine DualReplicaFeatureGateEnabled, aborting controller start: %w", err)
	} else if enabledDualReplicaFeature {
		if isDualReplicaTopology, err := ceohelpers.IsDualReplicaTopology(ctx, configInformers.Config().V1().Infrastructures().Lister()); err != nil {
			return fmt.Errorf("could not determine DualReplicaTopology, aborting controller start: %w", err)
		} else if isDualReplicaTopology {
			klog.Infof("detected DualReplica topology")

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

			klog.Infof("starting Two Node Fencing job controller")
			tnfJobController := tnflibgo.NewJobController(
				"TnfJob",
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
	}
	return nil
}
