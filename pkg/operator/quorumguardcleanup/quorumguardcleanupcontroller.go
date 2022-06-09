package quorumguardcleanup

import (
	"context"
	"fmt"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	policyv1listers "k8s.io/client-go/listers/policy/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	oldEtcdGuardDeploymentName    = "etcd-quorum-guard"
	newGuardLabelSelectorString   = "app=guard"
	masterNodeLabelSelectorString = "node-role.kubernetes.io/master"
)

// QuorumGuardCleanupController watches the old EtcdQuorumGuard deployment and removes it
// when the new staticpod controller guard pods have been rolled out
type QuorumGuardCleanupController struct {
	expectedOperatorVersion string
	operatorClient          v1helpers.OperatorClient
	kubeClient              kubernetes.Interface
	podLister               corev1listers.PodLister
	pdbLister               policyv1listers.PodDisruptionBudgetLister
	deploymentLister        appsv1listers.DeploymentLister
	masterNodeLister        corev1listers.NodeLister
	clusterOperatorLister   configv1listers.ClusterOperatorLister
	clusterVersionLister    configv1listers.ClusterVersionLister
	infrastructureInformer  configv1informers.InfrastructureInformer
}

func NewQuorumGuardCleanupController(
	operatorVersion string,
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	clusterVersionInformer configv1informers.ClusterVersionInformer,
	clusterOperatorInformer configv1informers.ClusterOperatorInformer,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	masterNodeInformer cache.SharedIndexInformer,
	infrastructureInformer configv1informers.InfrastructureInformer,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &QuorumGuardCleanupController{
		expectedOperatorVersion: operatorVersion,
		operatorClient:          operatorClient,
		kubeClient:              kubeClient,
		podLister:               kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		pdbLister:               kubeInformers.InformersFor(operatorclient.TargetNamespace).Policy().V1().PodDisruptionBudgets().Lister(),
		deploymentLister:        kubeInformers.InformersFor(operatorclient.TargetNamespace).Apps().V1().Deployments().Lister(),
		masterNodeLister:        corev1listers.NewNodeLister(masterNodeInformer.GetIndexer()),
		clusterOperatorLister:   clusterOperatorInformer.Lister(),
		clusterVersionLister:    clusterVersionInformer.Lister(),
		infrastructureInformer:  infrastructureInformer,
	}
	return factory.New().ResyncEvery(1*time.Minute).
		WithSync(c.sync).
		WithSyncDegradedOnError(operatorClient).
		WithBareInformers(masterNodeInformer).
		WithInformers(
			kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
			kubeInformers.InformersFor(operatorclient.TargetNamespace).Apps().V1().Deployments().Informer(),
			clusterOperatorInformer.Informer(),
		).ToController("QuorumGuardCleanupController", eventRecorder.WithComponentSuffix("quorum-guard-cleanup-controller"))
}

func (c *QuorumGuardCleanupController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	return c.cleanupOldQuorumGuard(ctx, syncCtx)
}

// cleanupOldQuorumGuard checks to see if the new static controller guard pods are ready
// and then deletes the "etcd-quorum-guard" deployment and PodDisruptionBudget
func (c *QuorumGuardCleanupController) cleanupOldQuorumGuard(ctx context.Context, syncCtx factory.SyncContext) error {
	// No need for cleanup on SNO(single node openshift) as neither the old and new quorum guards are deployed
	isSNO, precheckSucceeded, err := common.NewIsSingleNodePlatformFn(c.infrastructureInformer)()
	if err != nil {
		return fmt.Errorf("failed to check if cluster topology is SNO : %w", err)
	}
	if !precheckSucceeded {
		return fmt.Errorf("failed to check if cluster topology is SNO : waiting for infrastructure informer to sync")
	}
	if isSNO {
		return nil
	}

	isTargetVersion, err := c.checkClusterOperatorVersion()
	if err != nil {
		return fmt.Errorf("failed to ensure clusteroperator version: %w", err)
	}
	if !isTargetVersion {
		return nil
	}

	newGuardLabelSelector, err := labels.Parse(newGuardLabelSelectorString)
	if err != nil {
		return fmt.Errorf("failed to parse label selector: %w", err)
	}
	newGuardPods, err := c.podLister.Pods(operatorclient.TargetNamespace).List(newGuardLabelSelector)
	if err != nil {
		return fmt.Errorf("failed to get etcd static controller guard pods: %w", err)
	}
	masterNodeLabelSelector, err := labels.Parse(masterNodeLabelSelectorString)
	if err != nil {
		return fmt.Errorf("failed to parse label selector: %w", err)
	}
	masterNodes, err := c.masterNodeLister.List(masterNodeLabelSelector)
	if err != nil {
		return fmt.Errorf("failed to list master nodes: %w", err)
	}

	if len(masterNodes) == 0 {
		return nil
	}

	// Wait until all new guard pods are rolled out to all master nodes, and have the ready condition
	numReadyPods := countReadyPods(newGuardPods)
	if numReadyPods != len(masterNodes) {
		klog.V(2).Infof("%d/%d guard pods ready. Waiting until all new guard pods are ready", numReadyPods, len(masterNodes))
		return nil
	}

	if err := c.removeDeployment(ctx, syncCtx); err != nil {
		return err
	}
	if err := c.removePDB(ctx, syncCtx); err != nil {
		return err
	}
	return nil
}

// checkClusterOperatorVersion ensures the operator version in clusteroperator/etcd matches the desired version
// in clusterversion/version
// i.e all operands have been rolled out to the new image
func (c *QuorumGuardCleanupController) checkClusterOperatorVersion() (bool, error) {
	etcdClusterOperator, err := c.clusterOperatorLister.Get("etcd")
	if err != nil {
		return false, fmt.Errorf("failed to get clusteroperator/etcd: %w", err)
	}
	operatorVersion := ""
	for _, version := range etcdClusterOperator.Status.Versions {
		if version.Name == "operator" {
			operatorVersion = version.Version
		}
	}
	if len(operatorVersion) == 0 {
		return false, nil
	}

	if operatorVersion != c.expectedOperatorVersion {
		klog.V(2).Infof("clusterOperator/etcd's operator version (%s) and expected operator version (%s) do not match. Waiting on cleanup of old quorum guard deployment and PDB until operator reaches desired version.", operatorVersion, c.expectedOperatorVersion)
		return false, nil
	}

	return true, nil
}

// removePDB removes the etcd-quorum-guard Deployment if it exists
func (c *QuorumGuardCleanupController) removeDeployment(ctx context.Context, syncCtx factory.SyncContext) error {
	// Check if the old quorum guard deployment exists
	_, err := c.deploymentLister.Deployments(operatorclient.TargetNamespace).Get(oldEtcdGuardDeploymentName)
	if apierrors.IsNotFound(err) {
		// Old guard deployment already removed
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get etcd quorum guard deployment: %w", err)
	}

	err = c.kubeClient.AppsV1().Deployments(operatorclient.TargetNamespace).Delete(ctx, oldEtcdGuardDeploymentName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		// Old guard deployment already removed
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to delete %s deployment: %w", oldEtcdGuardDeploymentName, err)
	}

	klog.V(2).Infof("Deleted the old %s deployment", oldEtcdGuardDeploymentName)
	syncCtx.Recorder().Event("DeletedOldQuorumGuardDeployment", "etcd-quorum-guard deployment was deleted as it was replaced by the static controller guard pods")
	return nil
}

// removePDB removes the etcd-quorum-guard PodDisruptionBudget if it exists
func (c *QuorumGuardCleanupController) removePDB(ctx context.Context, syncCtx factory.SyncContext) error {
	_, err := c.pdbLister.PodDisruptionBudgets(operatorclient.TargetNamespace).Get(oldEtcdGuardDeploymentName)
	if apierrors.IsNotFound(err) {
		// PDB already removed
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get %s PodDisruptionBudget: %w", oldEtcdGuardDeploymentName, err)
	}

	c.kubeClient.PolicyV1().PodDisruptionBudgets(operatorclient.TargetNamespace).Delete(ctx, oldEtcdGuardDeploymentName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		// PDB already removed
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get %s PodDisruptionBudget: %w", oldEtcdGuardDeploymentName, err)
	}
	klog.V(2).Infof("Deleted the old %s PDB", oldEtcdGuardDeploymentName)
	syncCtx.Recorder().Event("DeletedOldQuorumGuardPDB", "etcd-quorum-guard PDB was deleted as it was replaced by the static controller guard PDB")
	return nil
}

func countReadyPods(pods []*corev1.Pod) int {
	numReadyPods := 0
	for _, pod := range pods {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				numReadyPods++
				break
			}
		}
	}
	return numReadyPods
}
