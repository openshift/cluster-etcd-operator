package ceohelpers

import (
	"context"
	"fmt"

	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
)

type QuorumChecker interface {
	// IsSafeToUpdateRevision checks the current etcd cluster and returns true if the cluster can tolerate the
	// loss of a single etcd member. Such loss is common during new static pod revision.
	// Returns True when it is absolutely safe, false if not. Error otherwise, which always indicates it is unsafe.
	IsSafeToUpdateRevision() (bool, error)
	// IsSafeToRestartMember returns true when restarting the etcd member on targetNodeName cannot break quorum:
	// the etcd cluster is currently quorum fault-tolerant AND no control plane node other than the target is
	// cordoned or not ready.  A cordoned control plane node signals an imminent drain/reboot (for instance a
	// machine-config update); restarting a member while another node is about to go down can leave the cluster
	// without quorum (OCPBUGS-100060).  When unsafe, the returned reason explains why.
	IsSafeToRestartMember(ctx context.Context, targetNodeName string) (bool, string, error)
}

// AlwaysSafeQuorumChecker can be used for testing and always returns that it is safe to update a revision
type AlwaysSafeQuorumChecker struct {
}

// IsSafeToUpdateRevision always returns true, nil
func (c *AlwaysSafeQuorumChecker) IsSafeToUpdateRevision() (bool, error) {
	return true, nil
}

// IsSafeToRestartMember always returns true
func (c *AlwaysSafeQuorumChecker) IsSafeToRestartMember(ctx context.Context, targetNodeName string) (bool, string, error) {
	return true, "", nil
}

// QuorumCheck is just a convenience struct around bootstrap.go
type QuorumCheck struct {
	namespaceLister corev1listers.NamespaceLister
	infraLister     configv1listers.InfrastructureLister
	operatorClient  v1helpers.StaticPodOperatorClient
	etcdClient      etcdcli.AllMemberLister
	nodeLister      corev1listers.NodeLister
}

func (c *QuorumCheck) IsSafeToUpdateRevision() (bool, error) {
	err := CheckSafeToScaleCluster(c.operatorClient, c.namespaceLister, c.infraLister, c.etcdClient)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (c *QuorumCheck) IsSafeToRestartMember(ctx context.Context, targetNodeName string) (bool, string, error) {
	scalingStrategy, err := GetBootstrapScalingStrategy(c.operatorClient, c.namespaceLister, c.infraLister)
	if err != nil {
		return false, "", fmt.Errorf("IsSafeToRestartMember failed to get bootstrap scaling strategy: %w", err)
	}
	if scalingStrategy == UnsafeScalingStrategy {
		return true, "", nil
	}

	// the cluster must currently tolerate the loss of one member
	memberHealth, err := c.etcdClient.MemberHealth(ctx)
	if err != nil {
		return false, "", fmt.Errorf("IsSafeToRestartMember couldn't determine member health: %w", err)
	}
	// Two Node OpenShift with Fencing protects etcd via pacemaker; treat it as an exception to the fault
	// tolerance rule, consistent with CheckSafeToScaleCluster.
	if err := etcdcli.IsQuorumFaultTolerantErr(memberHealth); err != nil &&
		!(len(memberHealth) == 2 && (scalingStrategy == TwoNodeScalingStrategy || scalingStrategy == DelayedTwoNodeScalingStrategy)) {
		return false, err.Error(), nil
	}

	// no control plane node other than the target may be cordoned or not ready.  Member health alone is not
	// enough: a node that is about to reboot keeps reporting healthy until it actually goes down, while the
	// cordon that precedes its drain is visible minutes in advance.
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return false, "", fmt.Errorf("IsSafeToRestartMember failed to list control plane nodes: %w", err)
	}
	for _, node := range nodes {
		if node.Name == targetNodeName {
			continue
		}
		if node.Spec.Unschedulable {
			return false, fmt.Sprintf("control plane node %q is cordoned, likely about to be drained and rebooted; restarting the etcd member on %q now could lose quorum", node.Name, targetNodeName), nil
		}
		if !isNodeReady(node) {
			return false, fmt.Sprintf("control plane node %q is not ready; restarting the etcd member on %q now could lose quorum", node.Name, targetNodeName), nil
		}
	}

	return true, "", nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func NewQuorumChecker(
	namespaceLister corev1listers.NamespaceLister,
	infraLister configv1listers.InfrastructureLister,
	operatorClient v1helpers.StaticPodOperatorClient,
	etcdClient etcdcli.AllMemberLister,
	nodeLister corev1listers.NodeLister,
) QuorumChecker {
	c := &QuorumCheck{
		namespaceLister,
		infraLister,
		operatorClient,
		etcdClient,
		nodeLister,
	}
	return c
}
