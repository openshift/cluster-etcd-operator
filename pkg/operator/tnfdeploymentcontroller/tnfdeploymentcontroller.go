package tnfdeploymentcontroller

import (
	"context"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/tnfdeployment_assets"
)

const (
	tnf_namespace = "openshift-tnf-operator"
)

type TnfDeploymentController struct {
	ctx                   context.Context
	operatorClient        v1helpers.StaticPodOperatorClient
	kubeClient            kubernetes.Interface
	eventRecorder         events.Recorder
	etcdImagePullSpec     string
	operatorImagePullSpec string
	resourceCache         resourceapply.ResourceCache
	envVarGetter          etcdenvvar.EnvVar
	enqueueFn             func()
}

func NewTnfDeploymentController(
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	envVarGetter etcdenvvar.EnvVar,
	eventRecorder events.Recorder,
	etcdImagePullSpec string,
	operatorImagePullSpec string,
) factory.Controller {
	c := &TnfDeploymentController{
		ctx:                   ctx,
		operatorClient:        operatorClient,
		kubeClient:            kubeClient,
		eventRecorder:         eventRecorder,
		etcdImagePullSpec:     etcdImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,
	}

	syncCtx := factory.NewSyncContext("TnfDeploymentController", eventRecorder.WithComponentSuffix("tnf-deployment-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}

	envVarGetter.AddListener(c)

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	//livenessChecker.Add("TargetConfigController", syncer)

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(time.Minute).
		WithSync(syncer.Sync).
		WithInformers(
			operatorClient.Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer(),
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer(),
			infrastructureInformer.Informer(),
		).ToController("TnfDeploymentController", syncCtx.Recorder())

}

func (c *TnfDeploymentController) sync(ctx context.Context, syncCtx factory.SyncContext) error {

	err := c.createTnf()
	if err != nil {
		condition := operatorv1.OperatorCondition{
			Type:    "TnfControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SynchronizationError",
			Message: err.Error(),
		}
		if _, _, err := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
			// this re-queues the sync loop to get the status updated correctly next invocation
			c.enqueueFn()
			return err
		}

		return err
	}

	condition := operatorv1.OperatorCondition{
		Type:   "TargetConfigControllerDegraded",
		Status: operatorv1.ConditionFalse,
	}
	if _, _, err := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
		// this re-queues the sync loop to get the status updated correctly next invocation
		c.enqueueFn()
		return err
	}

	return nil
}

func (c *TnfDeploymentController) createTnf() error {

	// namespace
	if _, _, err := c.manageNamespace(); err != nil {
		klog.Error("unable to manage namespace")
		return err
	}

	// service account
	if _, _, err := c.manageServiceAccount(); err != nil {
		klog.Error("unable to manage service account")
		return err
	}

	// cluster role
	if _, _, err := c.manageClusterRole(); err != nil {
		klog.Error("unable to manage cluster role")
		return err
	}

	// cluster role binding
	if _, _, err := c.manageClusterRoleBinding(); err != nil {
		klog.Error("unable to manage cluster role binding")
		return err
	}

	// leader election role
	if _, _, err := c.manageLeaderElectionRole(); err != nil {
		klog.Error("unable to manage leader election role")
		return err
	}

	// leader election role binding
	if _, _, err := c.manageLeaderElectionRoleBinding(); err != nil {
		klog.Error("unable to manage leader election role binding")
		return err
	}

	// deployment
	if _, _, err := c.manageDeployment(); err != nil {
		klog.Error("unable to manage deployment")
		return err
	}

	return nil
}

func (c *TnfDeploymentController) manageNamespace() (*corev1.Namespace, bool, error) {
	required := resourceread.ReadNamespaceV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/namespace.yaml"))
	return resourceapply.ApplyNamespace(c.ctx, c.kubeClient.CoreV1(), c.eventRecorder, required)
}

func (c *TnfDeploymentController) manageServiceAccount() (*corev1.ServiceAccount, bool, error) {
	required := resourceread.ReadServiceAccountV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/sa.yaml"))
	required.Namespace = tnf_namespace
	return resourceapply.ApplyServiceAccount(c.ctx, c.kubeClient.CoreV1(), c.eventRecorder, required)
}

func (c *TnfDeploymentController) manageClusterRole() (*rbacv1.ClusterRole, bool, error) {
	required := resourceread.ReadClusterRoleV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/clusterrole.yaml"))
	required.Namespace = tnf_namespace
	return resourceapply.ApplyClusterRole(c.ctx, c.kubeClient.RbacV1(), c.eventRecorder, required)
}

func (c *TnfDeploymentController) manageClusterRoleBinding() (*rbacv1.ClusterRoleBinding, bool, error) {
	required := resourceread.ReadClusterRoleBindingV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/clusterrole-binding.yaml"))
	required.Namespace = tnf_namespace
	return resourceapply.ApplyClusterRoleBinding(c.ctx, c.kubeClient.RbacV1(), c.eventRecorder, required)
}

func (c *TnfDeploymentController) manageLeaderElectionRole() (*rbacv1.Role, bool, error) {
	required := resourceread.ReadRoleV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/leaderelection-role.yaml"))
	required.Namespace = tnf_namespace
	return resourceapply.ApplyRole(c.ctx, c.kubeClient.RbacV1(), c.eventRecorder, required)
}

func (c *TnfDeploymentController) manageLeaderElectionRoleBinding() (*rbacv1.RoleBinding, bool, error) {
	required := resourceread.ReadRoleBindingV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/leaderelection-rolebinding.yaml"))
	required.Namespace = tnf_namespace
	return resourceapply.ApplyRoleBinding(c.ctx, c.kubeClient.RbacV1(), c.eventRecorder, required)
}

func (c *TnfDeploymentController) manageDeployment() (*appsv1.Deployment, bool, error) {
	required := resourceread.ReadDeploymentV1OrDie(tnfdeployment_assets.MustAsset("tnfdeployment/deployment.yaml"))
	required.Namespace = tnf_namespace

	// set image pullspec
	required.Spec.Template.Spec.Containers[0].Image = c.operatorImagePullSpec

	// set env var with etcd image pullspec
	env := required.Spec.Template.Spec.Containers[0].Env
	if env == nil {
		env = []corev1.EnvVar{}
	}
	env = append(env, corev1.EnvVar{
		Name:  "ETCD_IMAGE_PULLSPEC",
		Value: c.etcdImagePullSpec,
	})
	required.Spec.Template.Spec.Containers[0].Env = env
	// TODO handle expected generation
	return resourceapply.ApplyDeployment(c.ctx, c.kubeClient.AppsV1(), c.eventRecorder, required, -1)
}

func (c *TnfDeploymentController) Enqueue() {
	c.enqueueFn()
}
