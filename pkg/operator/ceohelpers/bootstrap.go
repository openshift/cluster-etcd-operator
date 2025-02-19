package ceohelpers

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/bootstrap"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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

	// TwoNodeScalingStrategy means the etcd cluster will only be scaled up when at least
	// 2 nodes are available so that quorum is maintained at all times. This rule applies
	// during bootstrapping and the steady state.
	//
	// This strategy is used for deployments of Two Node OpenShift with Fencing.
	TwoNodeScalingStrategy BootstrapScalingStrategy = "TwoNodeScalingStrategy"

	// DelayedTwoNodeScalingStrategy means that during bootstrapping, the etcd cluster will
	// be allowed to scale when at least 1 member is available (which is unsafe),
	// but after bootstrapping any further scaling will require 2 nodes in the same
	// way as TwoNodeScalingStrategy.
	//
	// This strategy is intended for deploys of Two Node OpenShift with Fencing via
	// the assisted or agent-based installers.
	DelayedTwoNodeScalingStrategy BootstrapScalingStrategy = "DelayedTwoNodeScalingStrategy"

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
	// DelayedBootstrapScalingStrategyAnnotation is an annotation on the openshift-etcd
	// namespace which, if present, indicates that one of the delayed scaling strategies
	// should be used. This is generally used by the assisted installer to ensure that
	// the bootstrap node can reboot into a cluster node.
	//
	// For HA clusters, this will be set to DelayedHAScalingStrategy.
	//
	// For Two Node OpenShift with Fencing, this is set to DelayedTwoNodeScalingStrategy.
	DelayedBootstrapScalingStrategyAnnotation = "openshift.io/delayed-bootstrap"

	// DelayedHABootstrapScalingStrategyAnnotation performs the same function as the annotation
	// above, and is kept for backwards compatibility.
	DelayedHABootstrapScalingStrategyAnnotation = "openshift.io/delayed-ha-bootstrap"
)

// GetBootstrapScalingStrategy determines the scaling strategy to use
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

	// Check for both the delayed annotation and the legacy DelayedHABootrapScalingStrategyAnnotation
	_, hasDelayedAnnotation := etcdNamespace.Annotations[DelayedBootstrapScalingStrategyAnnotation]
	_, hasDelayedHAAnnotation := etcdNamespace.Annotations[DelayedHABootstrapScalingStrategyAnnotation]
	hasDelayedAnnotation = hasDelayedAnnotation || hasDelayedHAAnnotation

	topology, err := GetControlPlaneTopology(infraLister)
	if err != nil {
		return strategy, fmt.Errorf("failed to get control plane topology: %w", err)
	}

	switch {
	case isUnsupportedUnsafeEtcd || topology == configv1.SingleReplicaTopologyMode:
		strategy = UnsafeScalingStrategy
	case topology == configv1.DualReplicaTopologyMode && hasDelayedAnnotation:
		strategy = DelayedTwoNodeScalingStrategy
	case topology == configv1.DualReplicaTopologyMode && !hasDelayedAnnotation:
		strategy = TwoNodeScalingStrategy
	case hasDelayedAnnotation:
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
	staticPodClient v1helpers.StaticPodOperatorClient,
	namespaceLister corev1listers.NamespaceLister,
	infraLister configv1listers.InfrastructureLister,
	etcdClient etcdcli.AllMemberLister) error {

	revisionStable, err := IsRevisionStable(staticPodClient)
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster failed to determine stability of revisions: %w", err)
	}
	// when revision is stabilising, scaling should be considered safe always
	if !revisionStable {
		return nil
	}

	scalingStrategy, err := GetBootstrapScalingStrategy(staticPodClient, namespaceLister, infraLister)
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster failed to get bootstrap scaling strategy: %w", err)
	}

	if scalingStrategy == UnsafeScalingStrategy {
		return nil
	}

	var minimumNodes int
	switch scalingStrategy {
	case HAScalingStrategy, DelayedHAScalingStrategy:
		minimumNodes = 3
	case TwoNodeScalingStrategy, DelayedTwoNodeScalingStrategy:
		minimumNodes = 2
	default:
		return fmt.Errorf("CheckSafeToScaleCluster unrecognized scaling strategy %q", scalingStrategy)
	}

	memberHealth, err := etcdClient.MemberHealth(context.Background())
	if err != nil {
		return fmt.Errorf("CheckSafeToScaleCluster couldn't determine member health: %w", err)
	}

	if len(memberHealth.GetHealthyMembers()) < minimumNodes {
		return fmt.Errorf("CheckSafeToScaleCluster found %d healthy member(s) out of the %d required by the %s",
			len(memberHealth.GetHealthyMembers()), minimumNodes, scalingStrategy)
	}

	// Fault tolerance protection is only enforced by for HA topologies
	//
	// TwoNodeScalingStrategy and DelayedTwoNodeScalingStrategy are used by Two Node OpenShift with
	// Fencing (TNF), which protects etcd using a service called pacemaker that is running on the nodes.
	// This service will intercept the static pod rollout, have that member of etcd leave the cluster,
	// restart the static pod with the updates, and have it rejoin the cluster as a learner. We treat
	// this as a special exception to fault tolerance rule for this topology only.
	err = etcdcli.IsQuorumFaultTolerantErr(memberHealth)
	if err != nil && !(len(memberHealth) == 2 && (scalingStrategy == TwoNodeScalingStrategy || scalingStrategy == DelayedTwoNodeScalingStrategy)) {
		return err
	}

	klog.V(4).Infof("node count %d satisfies minimum of %d required by the %s bootstrap scaling strategy", len(memberHealth.GetHealthyMembers()), minimumNodes, scalingStrategy)
	return nil
}

// IsBootstrapComplete returns true if bootstrap has completed.
func IsBootstrapComplete(configmapLister corev1listers.ConfigMapLister, etcdClient etcdcli.AllMemberLister) (bool, error) {
	// do a cheap check to see if the installer has marked
	// bootstrapping as done by creating the configmap first.
	if isBootstrapComplete, err := bootstrap.IsBootstrapComplete(configmapLister); !isBootstrapComplete || err != nil {
		return isBootstrapComplete, err
	}

	// check if etcd-bootstrap member is still present within the etcd cluster membership
	membersList, err := etcdClient.MemberList(context.Background())
	if err != nil {
		return false, fmt.Errorf("IsBootstrapComplete couldn't list the etcd cluster members: %w", err)
	}
	for _, m := range membersList {
		if m.Name == "etcd-bootstrap" {
			klog.V(4).Infof("(etcd-bootstrap) member is still present in the etcd cluster membership")
			return false, nil
		}
	}

	return true, nil
}

// IsRevisionStable checks the stability of revisions and returns true if the revision is stable
func IsRevisionStable(staticPodClient v1helpers.StaticPodOperatorClient) (bool, error) {
	_, status, _, err := staticPodClient.GetStaticPodOperatorState()
	if err != nil {
		return false, fmt.Errorf("failed to get static pod operator state: %w", err)
	}
	if status.LatestAvailableRevision == 0 {
		return false, nil
	}
	for _, curr := range status.NodeStatuses {
		if curr.CurrentRevision != status.LatestAvailableRevision {
			klog.V(4).Infof("revision stability check failed because revision %d is still in progress", status.LatestAvailableRevision)
			return false, nil
		}
	}

	return true, nil
}
