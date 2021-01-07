package quorumguardcontroller

import (
	"context"
	"fmt"
	"gopkg.in/yaml.v2"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strconv"
	"time"

	configclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	EtcdGuardDeploymentName     = "etcd-quorum-guard"
	infrastructureClusterName   = "cluster"
	clusterConfigName           = "cluster-config-v1"
	clusterConfigKey            = "install-config"
	clusterConfigNamespace      = "kube-system"
	pdbDeploymentMaxUnavailable = 1
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

// watches the etcd quorum guard deployment, create if not exists
type QuorumGuardController struct {
	operatorClient v1helpers.OperatorClient
	kubeClient     kubernetes.Interface
	podLister      corev1listers.PodLister
	nodeLister     corev1listers.NodeLister
	infraClient    configclientv1.InfrastructuresGetter
	haMode         configv1.HighAvailabilityMode
	replicaCount   int
}

func NewQuorumGuardController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
	infraClient configclientv1.InfrastructuresGetter,
) factory.Controller {
	c := &QuorumGuardController{
		operatorClient: operatorClient,
		kubeClient:     kubeClient,
		podLister:      kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:     kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		infraClient:    infraClient,
		replicaCount:   0,
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ResyncEvery(time.Minute).ToController("QuorumGuardController", eventRecorder.WithComponentSuffix("quorum-guard-controller"))
}

func (c *QuorumGuardController) isInHAMode(ctx context.Context) (bool, error) {
	// update once
	if c.haMode != "" {
		return c.haMode == configv1.FullHighAvailabilityMode, nil
	}

	infraData, err := c.infraClient.Infrastructures().Get(ctx, infrastructureClusterName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	// Added to make sure that iif ha mode is not set in infrastructure object
	// we will get the default value configv1.FullHighAvailabilityMode
	if infraData.Status.HighAvailabilityMode == "" {
		klog.Infof("HA mode was not set in infrastructure resource setting it to default value %s", configv1.FullHighAvailabilityMode)
		infraData.Status.HighAvailabilityMode = configv1.FullHighAvailabilityMode
	}
	c.haMode = infraData.Status.HighAvailabilityMode
	klog.Infof("HA mode is: %s", c.haMode)

	return c.haMode == configv1.FullHighAvailabilityMode, nil
}

func (c *QuorumGuardController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.ensureEtcdGuardDeployment(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "QuorumGuardControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("QuorumGuardControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "QuorumGuardControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *QuorumGuardController) ensureEtcdGuardDeployment(ctx context.Context, recorder events.Recorder) error {
	haMode, err := c.isInHAMode(ctx)
	if err != nil {
		klog.Infof("Failed to validate ha mode %v ", err)
		return err
	}
	if !haMode {
		return nil
	}

	replicaCount, err := c.getMastersReplicaCount(ctx)
	if err != nil {
		return err
	}

	if err := c.ensureEtcdGuardDeployment2(ctx, replicaCount, recorder); err != nil {
		return err
	}

	if err := c.ensureEtcdGuardPdbDeployment(ctx, recorder); err != nil {
		return err
	}

	return nil
}

func (c *QuorumGuardController) ensureEtcdGuardDeployment2(ctx context.Context, replicaCount int32, recorder events.Recorder) error {
	_, err := c.kubeClient.AppsV1().Deployments(operatorclient.TargetNamespace).Get(ctx,
		EtcdGuardDeploymentName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		recorder.Eventf("NoEtcdGuardDeployment", "%s was not found, creating one", EtcdGuardDeploymentName)
		return c.applyDeployment(replicaCount, recorder)
	case err != nil:
		klog.Warningf("failed to get %s deployment err %v", EtcdGuardDeploymentName, err.Error())
		return err
	default:
		return nil
	}
}

func (c *QuorumGuardController) ensureEtcdGuardPdbDeployment(ctx context.Context, recorder events.Recorder) error {
	_, err := c.kubeClient.PolicyV1beta1().PodDisruptionBudgets(operatorclient.TargetNamespace).Get(ctx,
		EtcdGuardDeploymentName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		recorder.Eventf("NoEtcdGuardDeployment", "%s was not found, creating one", EtcdGuardDeploymentName)
		return c.applyPdbDeployment(ctx, recorder)
	case err != nil:
		klog.Warningf("failed to get %s deployment err %v", EtcdGuardDeploymentName, err.Error())
		return err
	default:
		return nil
	}
}

// Applying etcd guard deployment
func (c *QuorumGuardController) applyDeployment(replicaCount int32, recorder events.Recorder) error {
	klog.Infof("Going to create new %s deployment with replica count %d", EtcdGuardDeploymentName, replicaCount)

	deployment := resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	deployment.Spec.Replicas = &replicaCount
	newDeployment, _, err := resourceapply.ApplyDeployment(c.kubeClient.AppsV1(), recorder, deployment, -1)
	klog.Infof("New deployment is %v", newDeployment)
	return err
}

// Applying etcd guard pdb
func (c *QuorumGuardController) applyPdbDeployment(ctx context.Context, recorder events.Recorder) error {
	klog.Infof("Going to create new %s pdb", EtcdGuardDeploymentName)
	maxUnavailable := intstr.FromInt(pdbDeploymentMaxUnavailable)
	pdb := &policyv1beta1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EtcdGuardDeploymentName,
			Namespace: operatorclient.TargetNamespace,
			Annotations: map[string]string{"include.release.openshift.io/self-managed-high-availability": "true",
				"include.release.openshift.io/single-node-developer": "true"},
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": EtcdGuardDeploymentName}},
		},
	}

	if _, err := c.kubeClient.PolicyV1beta1().PodDisruptionBudgets(operatorclient.TargetNamespace).Create(ctx, pdb, metav1.CreateOptions{}); err != nil {
		return err
	}

	recorder.Eventf("CreatedEtcdGuardPDBDeployment", "%s was created", EtcdGuardDeploymentName)
	return nil
}

// Get number of expected masters
func (c *QuorumGuardController) getMastersReplicaCount(ctx context.Context) (int32, error) {
	if c.replicaCount != 0 {
		return int32(c.replicaCount), nil
	}

	klog.Infof("Getting number of expected masters from %s", clusterConfigName)
	clusterConfig, err := c.kubeClient.CoreV1().ConfigMaps(clusterConfigNamespace).Get(ctx, clusterConfigName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get ConfigMap %s, err %v", clusterConfigName, err)
		return 0, err
	}

	rcD := replicaCountDecoder{}
	if err := yaml.Unmarshal([]byte(clusterConfig.Data[clusterConfigKey]), &rcD); err != nil {
		err := fmt.Errorf("%s key doesn't exist in ConfigMap %s", clusterConfigKey, clusterConfigName)
		klog.Error(err)
		return 0, err
	}

	c.replicaCount, err = strconv.Atoi(rcD.ControlPlane.Replicas)
	if err != nil {
		klog.Errorf("failed to convert replica %s, err %v", rcD.ControlPlane.Replicas, err)
		return 0, err
	}
	return int32(c.replicaCount), nil
}
