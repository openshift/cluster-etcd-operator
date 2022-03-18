package quorumguardcontroller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/ghodss/yaml"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	resourceapplylg "github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/lib/resourceapply"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	EtcdGuardDeploymentName   = "etcd-quorum-guard"
	infrastructureClusterName = "cluster"
	clusterConfigName         = "cluster-config-v1"
	clusterConfigKey          = "install-config"
	clusterConfigNamespace    = "kube-system"
)

type replicaCountDecoder struct {
	ControlPlane struct {
		Replicas string `yaml:"replicas,omitempty"`
	} `yaml:"controlPlane,omitempty"`
}

var pdb = &policyv1.PodDisruptionBudget{
	ObjectMeta: metav1.ObjectMeta{
		Name:      EtcdGuardDeploymentName,
		Namespace: operatorclient.TargetNamespace,
	},
	Spec: policyv1.PodDisruptionBudgetSpec{
		MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: int32(2)},
		Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": EtcdGuardDeploymentName}},
	},
}

// QuorumGuardController watches the etcd quorum guard deployment, create if not exists.
type QuorumGuardController struct {
	operatorClient       v1helpers.OperatorClient
	kubeClient           kubernetes.Interface
	podLister            corev1listers.PodLister
	nodeLister           corev1listers.NodeLister
	configMapLister      corev1listers.ConfigMapLister
	infrastructureLister configv1listers.InfrastructureLister
	clusterTopology      configv1.TopologyMode
	replicaCount         int
	etcdQuorumGuard      *appsv1.Deployment
	cliImagePullSpec     string
}

func NewQuorumGuardController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
	infrastructureLister configv1listers.InfrastructureLister,
	cliImagePullSpec string,
) factory.Controller {
	c := &QuorumGuardController{
		operatorClient:       operatorClient,
		kubeClient:           kubeClient,
		podLister:            kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		nodeLister:           kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		configMapLister:      kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister(),
		infrastructureLister: infrastructureLister,
		replicaCount:         0,
		cliImagePullSpec:     cliImagePullSpec,
	}
	return factory.New().ResyncEvery(10*time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ToController("QuorumGuardController", eventRecorder.WithComponentSuffix("quorum-guard-controller"))
}

func (c *QuorumGuardController) getTopologyMode() (configv1.TopologyMode, error) {
	// Today cluster topology cannot change, infrastructure cr is immutable,
	// so we set it once in order not to run api call each time.
	if c.clusterTopology != "" {
		klog.V(4).Infof("HA mode is: %s", c.clusterTopology)
		return c.clusterTopology, nil
	}

	var err error
	c.clusterTopology, err = ceohelpers.GetControlPlaneTopology(c.infrastructureLister)
	if err != nil {
		klog.Errorf("Failed to get topology mode %w ", err)
		return "", err
	}

	klog.Infof("HA mode is: %s", c.clusterTopology)

	return c.clusterTopology, nil
}

func (c *QuorumGuardController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.ensureEtcdGuard(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "QuorumGuardControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *QuorumGuardController) ensureEtcdGuard(ctx context.Context, recorder events.Recorder) error {
	topologyMode, err := c.getTopologyMode()
	if err != nil {
		return err
	}

	if topologyMode != configv1.HighlyAvailableTopologyMode {
		klog.V(4).Infof("quorum guard controller disabled in topology mode: %s", topologyMode)
		return nil
	}

	if err := c.ensureEtcdGuardDeployment(ctx, recorder); err != nil {
		return err
	}

	if err := c.ensureEtcdGuardPDB(ctx, recorder); err != nil {
		return err
	}

	return nil
}

// ensureEtcdGuardDeployment if etcd quorum guard deployment doesn't exist or was changed - apply it.
func (c *QuorumGuardController) ensureEtcdGuardDeployment(ctx context.Context, recorder events.Recorder) error {
	replicaCount, err := c.getMastersReplicaCount()
	if err != nil {
		return err
	}

	if c.etcdQuorumGuard == nil {
		c.etcdQuorumGuard = resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
		if c.etcdQuorumGuard.Spec.Template.Spec.Affinity.PodAffinity != nil {
			return fmt.Errorf("quorum-guard deployment pod affinity can not be defined in bindata")
		}
	}
	c.etcdQuorumGuard.Spec.Replicas = &replicaCount
	// Use image from release payload.
	c.etcdQuorumGuard.Spec.Template.Spec.Containers[0].Image = c.cliImagePullSpec

	// Set pod affinity only in HA topology mode.
	affinity := c.etcdQuorumGuard.Spec.Template.Spec.Affinity
	affinity.PodAffinity = &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "k8s-app",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"etcd"},
						},
					},
				},
				TopologyKey: "kubernetes.io/hostname",
			},
		},
	}
	c.etcdQuorumGuard.Spec.Template.Spec.Affinity = affinity
	klog.V(4).Infof("quorum guard controller applied podAffinity")

	// If restart occurred, we will apply etcd guard deployment but if it is the same, nothing will happen.
	actual, modified, err := resourceapply.ApplyDeploymentv1(ctx, c.kubeClient.AppsV1(), c.etcdQuorumGuard)
	if err != nil {
		klog.Errorf("failed to verify/apply %s, error %w", EtcdGuardDeploymentName, err)
		return err
	}
	// If deployment was modified need to save spec from modified one cause there are
	// some fields added on creation or apply, for example permissions on volumes
	// and we want to save them to be able to verify that deployment was not changed.
	if modified {
		c.etcdQuorumGuard.Spec = actual.Spec
		msg := fmt.Sprintf("%s was modified", EtcdGuardDeploymentName)
		klog.Infof(msg)
		recorder.Event("ModifiedQuorumGuardDeployment", msg)
	}

	return nil
}

// ensureEtcdGuardPDB if etcd quorum guard PDB doesn't exist or was changed, apply one.
func (c *QuorumGuardController) ensureEtcdGuardPDB(ctx context.Context, recorder events.Recorder) error {

	// If restart occurred, we will apply PDB but if it is the same, nothing will happen.
	_, modified, err := resourceapplylg.ApplyPodDisruptionBudget(ctx, c.kubeClient.PolicyV1(), recorder, pdb)
	if err != nil {
		klog.Errorf("failed to verify/apply %s pdb, error %w", EtcdGuardDeploymentName, err)
		return err
	}

	// Log if PDB was modified, modified event is created as part of ApplyPodDisruptionBudget above.
	if modified {
		klog.Infof("%s pdb was modified", EtcdGuardDeploymentName)
	}

	return nil
}

// getMastersReplicaCount get number of expected masters statically defined by the controlPlane replicas in the install-config.
func (c *QuorumGuardController) getMastersReplicaCount() (int32, error) {
	if c.replicaCount != 0 {
		return int32(c.replicaCount), nil
	}

	klog.Infof("Getting number of expected masters from %s", clusterConfigName)
	clusterConfig, err := c.configMapLister.ConfigMaps(operatorclient.TargetNamespace).Get(clusterConfigName)
	if err != nil {
		klog.Errorf("failed to get ConfigMap %s, err %w", clusterConfigName, err)
		return 0, err
	}

	rcD := replicaCountDecoder{}
	if err := yaml.Unmarshal([]byte(clusterConfig.Data[clusterConfigKey]), &rcD); err != nil {
		err := fmt.Errorf("%s key doesn't exist in configmap/%s, err %w", clusterConfigKey, clusterConfigName, err)
		klog.Error(err)
		return 0, err
	}

	c.replicaCount, err = strconv.Atoi(rcD.ControlPlane.Replicas)
	if err != nil {
		klog.Errorf("failed to convert replica %s, err %w", rcD.ControlPlane.Replicas, err)
		return 0, err
	}
	return int32(c.replicaCount), nil
}
