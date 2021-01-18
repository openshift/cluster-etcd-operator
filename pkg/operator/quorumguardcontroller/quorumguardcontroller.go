package quorumguardcontroller

import (
	"context"
	"fmt"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"gopkg.in/yaml.v2"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strconv"
	"time"

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

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	EtcdGuardDeploymentName            = "etcd-quorum-guard"
	infrastructureClusterName          = "cluster"
	clusterConfigName                  = "cluster-config-v1"
	clusterConfigKey                   = "install-config"
	clusterConfigNamespace             = "kube-system"
	pdbDeploymentMaxUnavailableDefault = 1
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

// QuorumGuardController watches the etcd quorum guard deployment, create if not exists
type QuorumGuardController struct {
	operatorClient       v1helpers.OperatorClient
	kubeClient           kubernetes.Interface
	podLister            corev1listers.PodLister
	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister
	clusterTopology      configv1.TopologyMode
	replicaCount         int
}

func NewQuorumGuardController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
	infrastructureLister configv1listers.InfrastructureLister,
) factory.Controller {
	c := &QuorumGuardController{
		operatorClient:       operatorClient,
		kubeClient:           kubeClient,
		podLister:            kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:           kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		infrastructureLister: infrastructureLister,
		replicaCount:         0,
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ResyncEvery(time.Minute).ToController("QuorumGuardController", eventRecorder.WithComponentSuffix("quorum-guard-controller"))
}

func (c *QuorumGuardController) isInHATopologyMode() (bool, error) {
	// update once
	if c.clusterTopology != "" {
		return c.clusterTopology == configv1.HighlyAvailableTopologyMode, nil
	}

	var err error
	c.clusterTopology, err = ceohelpers.GetControlPlaneTopology(c.infrastructureLister)
	if err != nil {
		return false, err
	}

	klog.Infof("HA mode is: %s", c.clusterTopology)

	return c.clusterTopology == configv1.HighlyAvailableTopologyMode, nil
}

func (c *QuorumGuardController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.ensureEtcdGuard(ctx, syncCtx.Recorder())
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

func (c *QuorumGuardController) ensureEtcdGuard(ctx context.Context, recorder events.Recorder) error {
	haTopologyMode, err := c.isInHATopologyMode()
	if err != nil {
		klog.Infof("Failed to validate ha mode %v ", err)
		return err
	}
	if !haTopologyMode {
		return nil
	}

	replicaCount, err := c.getMastersReplicaCount(ctx)
	if err != nil {
		return err
	}

	if err := c.ensureEtcdGuardDeployment(ctx, replicaCount, recorder); err != nil {
		return err
	}

	if err := c.ensureEtcdGuardPdbDeployment(ctx, recorder); err != nil {
		return err
	}

	return nil
}

// ensureEtcdGuardDeployment if etcd quorum guard deployment doesn't exist - add it, else skip
func (c *QuorumGuardController) ensureEtcdGuardDeployment(ctx context.Context, replicaCount int32, recorder events.Recorder) error {
	_, err := c.kubeClient.AppsV1().Deployments(operatorclient.TargetNamespace).Get(ctx,
		EtcdGuardDeploymentName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		recorder.Eventf("NoEtcdQuorumGuardDeployment", "%s was not found, creating one", EtcdGuardDeploymentName)
		return c.applyDeployment(replicaCount, recorder)
	case err != nil:
		klog.Warningf("failed to get %s deployment err %v", EtcdGuardDeploymentName, err.Error())
		return err
	default:
		return nil
	}
}

// ensureEtcdGuardDeployment if etcd quorum guard pdb deployment doesn't exist - add it, else skip
func (c *QuorumGuardController) ensureEtcdGuardPdbDeployment(ctx context.Context, recorder events.Recorder) error {
	_, err := c.kubeClient.PolicyV1beta1().PodDisruptionBudgets(operatorclient.TargetNamespace).Get(ctx,
		EtcdGuardDeploymentName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		recorder.Eventf("NoEtcdQuorumGuardPDBDeployment", "%s was not found, creating one", EtcdGuardDeploymentName)
		return c.applyPdbDeployment(ctx, recorder)
	case err != nil:
		klog.Warningf("failed to get %s deployment err %v", EtcdGuardDeploymentName, err.Error())
		return err
	default:
		return nil
	}
}

// applyDeployment applies new etcd quorum guard deployment
func (c *QuorumGuardController) applyDeployment(replicaCount int32, recorder events.Recorder) error {
	klog.Infof("Going to create new %s deployment with replica count %d", EtcdGuardDeploymentName, replicaCount)

	deployment := resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	deployment.Spec.Replicas = &replicaCount
	newDeployment, _, err := resourceapply.ApplyDeployment(c.kubeClient.AppsV1(), recorder, deployment, -1)
	if err != nil {
		klog.Errorf("Failed to deploy %s, error: %v", deployment.Name, err)
		return err
	}

	klog.Infof("New etcd quorum guard deployment is %s", newDeployment.Name)
	return err
}

// applyPdbDeployment applies new etcd quorum guard pdb
func (c *QuorumGuardController) applyPdbDeployment(ctx context.Context, recorder events.Recorder) error {
	klog.Infof("Creating new PDB %q", EtcdGuardDeploymentName)
	maxUnavailable := intstr.FromInt(pdbDeploymentMaxUnavailableDefault)
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
		klog.Errorf("Failed to deploy %s, error: %v", pdb.Name, err)
		return err
	}

	klog.Infof("New etcd quorum guard pdb deployment is %s", pdb.Name)
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
