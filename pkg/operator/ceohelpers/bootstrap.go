package ceohelpers

import (
	"context"
	"fmt"

	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

// BootstrapScalingStrategy describes the invariants which will be enforced when
// scaling the etcd cluster.
type BootstrapScalingStrategy string

const (
	// HAScalingStrategy means the etcd cluster will only be scaled up when at least
	// 3 node are available so that HA is enforced at all times. This rule applies
	// during bootstrapping and the steady state.
	//
	// This is the default strategy.
	HAScalingStrategy BootstrapScalingStrategy = "HAScalingStrategy"

	// DelayedHAScalingStrategy means that during bootstrapping, the etcd cluster will
	// be allowed to scale when at least 2 members are available (which is not HA),
	// but after bootstrapping any further scaling will require 3 nodes in the same
	// way as HAScalingStrategy.
	//
	// This strategy is selected by adding the `openshift.io/delayed-ha-bootstrap`
	// annotation to the openshift-etcd namesapce.
	DelayedHAScalingStrategy BootstrapScalingStrategy = "DelayedHAScalingStrategy"

	// BootstrapInPlaceStrategy means that the bootstrap node will never exist
	// during the lifecycle of the cluster. Bootkube will run on a live iso
	// afterwards the node will pivot into the manifests generated during that
	// process.
	//
	// This strategy is selected by observing the existence of `bootstrapInPlace`
	// root key in the install-config.
	BootstrapInPlaceStrategy BootstrapScalingStrategy = "BootstrapInPlaceStrategy"

	// UnsafeScalingStrategy means scaling will occur without regards to nodes and
	// any effect on quorum. Use of this strategy isn't officially tested or supported,
	// but is made available for ad-hoc use.
	//
	// This strategy is selected by setting unsupportedConfigOverrides on the
	// operator config.
	UnsafeScalingStrategy BootstrapScalingStrategy = "UnsafeScalingStrategy"
)

const (
	// DelayedHABootstrapScalingStrategyAnnotation is an annotation on the openshift-etcd
	// namespace which, if present indicates the DelayedHAScalingStrategy strategy
	// should be used.
	DelayedHABootstrapScalingStrategyAnnotation = "openshift.io/delayed-ha-bootstrap"
)

// GetBootstrapScalingStrategy determines the scaling strategy to use.
func GetBootstrapScalingStrategy(staticPodClient v1helpers.StaticPodOperatorClient, namespaceLister corev1listers.NamespaceLister, infraLister configv1listers.InfrastructureLister) (BootstrapScalingStrategy, error) {
	var strategy BootstrapScalingStrategy

	operatorSpec, _, _, err := staticPodClient.GetStaticPodOperatorState()
	if err != nil {
		return strategy, fmt.Errorf("failed to get operator state: %w", err)
	}

	isUnsupportedUnsafeEtcd, err := isUnsupportedUnsafeEtcd(operatorSpec)
	if err != nil {
		return strategy, fmt.Errorf("couldn't determine etcd unsupported override status, assuming default HA scaling strategy: %w", err)
	}

	etcdNamespace, err := namespaceLister.Get(operatorclient.TargetNamespace)
	if err != nil {
		return strategy, fmt.Errorf("failed to get %s namespace: %w", operatorclient.TargetNamespace, err)
	}
	_, hasDelayedHAAnnotation := etcdNamespace.Annotations[DelayedHABootstrapScalingStrategyAnnotation]

	singleNode, err := IsSingleNodeTopology(infraLister)
	if err != nil {
		return strategy, fmt.Errorf("failed to get control plane topology: %w", err)
	}

	switch {
	case isUnsupportedUnsafeEtcd || singleNode:
		strategy = UnsafeScalingStrategy
	case hasDelayedHAAnnotation:
		strategy = DelayedHAScalingStrategy
	default:
		strategy = HAScalingStrategy
	}
	return strategy, nil
}

// CheckSafeToScaleCluster is used to implement the bootstrap scaling strategy invariants.
// This function returns nil if cluster conditions are such that it's safe to scale
// the etcd cluster based on the scaling strategy in use, and otherwise will return
// an error explaining why it's unsafe to scale.
func CheckSafeToScaleCluster(
	configmapLister corev1listers.ConfigMapLister,
	staticPodClient v1helpers.StaticPodOperatorClient,
	namespaceLister corev1listers.NamespaceLister,
	infraLister configv1listers.InfrastructureLister,
	etcdClient etcdcli.EtcdClient) error {

	bootstrapComplete, err := IsBootstrapComplete(configmapLister, staticPodClient)
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster failed to determine bootstrap status: %w", err)
	}

	// while bootstrapping, scaling should be considered safe always
	if !bootstrapComplete {
		return nil
	}

	_, operatorStatus, _, err := staticPodClient.GetStaticPodOperatorState()
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster failed to get operator state: %w", err)
	}

	scalingStrategy, err := GetBootstrapScalingStrategy(staticPodClient, namespaceLister, infraLister)
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster failed to get bootstrap scaling strategy: %w", err)
	}

	var minimumNodes int
	switch scalingStrategy {
	case HAScalingStrategy:
		minimumNodes = 3
	case UnsafeScalingStrategy:
		minimumNodes = 1
	case DelayedHAScalingStrategy:
		minimumNodes = 3
	default:
		return fmt.Errorf("CheckSafeToScaleCluster unrecognized scaling strategy %q", scalingStrategy)
	}

	nodeCount := len(operatorStatus.NodeStatuses)
	if nodeCount < minimumNodes {
		return fmt.Errorf("CheckSafeToScaleCluster %d nodes are required, but only %d are available", minimumNodes, nodeCount)
	}

	memberHealth, err := etcdClient.MemberHealth(context.Background())
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster couldn't determine member health: %w", err)
	}

	err = etcdcli.IsQuorumFaultTolerantErr(memberHealth)
	if err != nil {
		return err
	}

	klog.V(4).Infof("node count %d satisfies minimum of %d required by the %s bootstrap scaling strategy", nodeCount, minimumNodes, scalingStrategy)
	return nil
}

// IsBootstrapComplete returns true if bootstrap has completed.
func IsBootstrapComplete(configMapClient corev1listers.ConfigMapLister, staticPodClient v1helpers.StaticPodOperatorClient) (bool, error) {
	// do a cheap check to see if the annotation is already gone.
	// check to see if bootstrapping is complete
	bootstrapFinishedConfigMap, err := configMapClient.ConfigMaps("kube-system").Get("bootstrap")
	if err != nil {
		if errors.IsNotFound(err) {
			// If the resource was deleted (e.g. by an admin) after bootstrap is actually complete,
			// this is a false negative.
			klog.V(4).Infof("bootstrap considered incomplete because the kube-system/bootstrap configmap wasn't found")
			return false, nil
		}
		// We don't know, give up quickly.
		return false, fmt.Errorf("failed to get configmap %s/%s: %w", "kube-system", "bootstrap", err)
	}

	if status, ok := bootstrapFinishedConfigMap.Data["status"]; !ok || status != "complete" {
		// do nothing, not torn down
		klog.V(4).Infof("bootstrap considered incomplete because status is %q", status)
		return false, nil
	}

	// now run check to stability of revisions
	_, status, _, err := staticPodClient.GetStaticPodOperatorState()
	if err != nil {
		return false, fmt.Errorf("failed to get static pod operator state: %w", err)
	}
	if status.LatestAvailableRevision == 0 {
		return false, nil
	}
	for _, curr := range status.NodeStatuses {
		if curr.CurrentRevision != status.LatestAvailableRevision {
			klog.V(4).Infof("bootstrap considered incomplete because revision %d is still in progress", status.LatestAvailableRevision)
			return false, nil
		}
	}
	return true, nil
}
