package node

import (
	"context"
	"fmt"
	"strings"

	coreapiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/informers"
	corelisterv1 "k8s.io/client-go/listers/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	"github.com/openshift/library-go/pkg/apiserver/jsonpatch"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// NodeController watches for new master nodes and adds them to the node status list in the operator config status.
type NodeController struct {
	controllerInstanceName string
	operatorClient         v1helpers.StaticPodOperatorClient
	nodeLister             corelisterv1.NodeLister
}

// NewNodeController creates a new node controller.
func NewNodeController(
	instanceName string,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersClusterScoped informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &NodeController{
		controllerInstanceName: factory.ControllerInstanceName(instanceName, "Node"),
		operatorClient:         operatorClient,
		nodeLister:             kubeInformersClusterScoped.Core().V1().Nodes().Lister(),
	}
	return factory.New().
		WithInformers(
			operatorClient.Informer(),
			kubeInformersClusterScoped.Core().V1().Nodes().Informer(),
		).
		WithSync(c.sync).
		WithControllerInstanceName(c.controllerInstanceName).
		ToController(
			c.controllerInstanceName,
			eventRecorder,
		)
}

func (c *NodeController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	_, originalOperatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	selector, err := labels.NewRequirement("node-role.kubernetes.io/master", selection.Equals, []string{""})
	if err != nil {
		panic(err)
	}
	nodes, err := c.nodeLister.List(labels.NewSelector().Add(*selector))
	if err != nil {
		return err
	}

	jsonPatch := jsonpatch.New()
	var removedNodeStatusesCounter int
	newTargetNodeStates := []*applyoperatorv1.NodeStatusApplyConfiguration{}
	// remove entries for missing nodes
	for i, nodeState := range originalOperatorStatus.NodeStatuses {
		found := false
		for _, node := range nodes {
			if nodeState.NodeName == node.Name {
				found = true
			}
		}
		if found {
			newTargetNodeState := applyoperatorv1.NodeStatus().
				WithNodeName(originalOperatorStatus.NodeStatuses[i].NodeName).
				WithCurrentRevision(originalOperatorStatus.NodeStatuses[i].CurrentRevision).
				WithTargetRevision(originalOperatorStatus.NodeStatuses[i].TargetRevision).
				WithLastFailedRevision(originalOperatorStatus.NodeStatuses[i].LastFailedRevision).
				WithLastFailedReason(originalOperatorStatus.NodeStatuses[i].LastFailedReason).
				WithLastFailedCount(originalOperatorStatus.NodeStatuses[i].LastFailedCount).
				WithLastFallbackCount(originalOperatorStatus.NodeStatuses[i].LastFallbackCount).
				WithLastFailedRevisionErrors(originalOperatorStatus.NodeStatuses[i].LastFailedRevisionErrors...)
			if originalOperatorStatus.NodeStatuses[i].LastFailedTime != nil {
				newTargetNodeState = newTargetNodeState.WithLastFailedTime(*originalOperatorStatus.NodeStatuses[i].LastFailedTime)
			}
			newTargetNodeStates = append(newTargetNodeStates, newTargetNodeState)
		} else {
			syncCtx.Recorder().Warningf("MasterNodeRemoved", "Observed removal of master node %s", nodeState.NodeName)
			// each delete operation is applied to the object,
			// which modifies the array. Thus, we need to
			// adjust the indices to find the correct node to remove.
			removeAtIndex := i
			if !jsonPatch.IsEmpty() {
				removeAtIndex = removeAtIndex - removedNodeStatusesCounter
			}
			jsonPatch.WithRemove(fmt.Sprintf("/status/nodeStatuses/%d", removeAtIndex), jsonpatch.NewTestCondition(fmt.Sprintf("/status/nodeStatuses/%d/nodeName", removeAtIndex), nodeState.NodeName))
			removedNodeStatusesCounter++
		}
	}

	// add entries for new nodes
	for _, node := range nodes {
		found := false
		for _, nodeState := range originalOperatorStatus.NodeStatuses {
			if nodeState.NodeName == node.Name {
				found = true
			}
		}
		if found {
			continue
		}

		syncCtx.Recorder().Eventf("MasterNodeObserved", "Observed new master node %s", node.Name)
		newTargetNodeState := applyoperatorv1.NodeStatus().WithNodeName(node.Name)
		newTargetNodeStates = append(newTargetNodeStates, newTargetNodeState)
	}

	degradedCondition := applyoperatorv1.OperatorCondition().WithType(condition.NodeControllerDegradedConditionType)
	if !jsonPatch.IsEmpty() {
		if err = c.operatorClient.PatchStaticOperatorStatus(ctx, jsonPatch); err != nil {
			degradedCondition = degradedCondition.
				WithStatus(operatorv1.ConditionTrue).
				WithReason("MasterNodeNotRemoved").
				WithMessage(fmt.Sprintf("failed applying JSONPatch, err: %v", err.Error()))

			status := applyoperatorv1.StaticPodOperatorStatus().
				WithConditions(degradedCondition).
				WithNodeStatuses(newTargetNodeStates...)

			return c.operatorClient.ApplyStaticPodOperatorStatus(ctx, c.controllerInstanceName, status)
		}
	}

	// detect and report master nodes that are not ready
	notReadyNodes := []string{}
	for _, node := range nodes {
		nodeReadyCondition := nodeConditionFinder(&node.Status, coreapiv1.NodeReady)

		// If a "Ready" condition is not found, that node should be deemed as not Ready by default.
		if nodeReadyCondition == nil {
			notReadyNodes = append(notReadyNodes, fmt.Sprintf("node %q not ready, no Ready condition found in status block", node.Name))
			continue
		}

		if nodeReadyCondition.Status != coreapiv1.ConditionTrue {
			notReadyNodes = append(notReadyNodes, fmt.Sprintf("node %q not ready since %s because %s (%s)", node.Name, nodeReadyCondition.LastTransitionTime, nodeReadyCondition.Reason, nodeReadyCondition.Message))
		}
	}

	if len(notReadyNodes) > 0 {
		degradedCondition = degradedCondition.
			WithStatus(operatorv1.ConditionTrue).
			WithReason("MasterNodesReady").
			WithMessage(fmt.Sprintf("The master nodes not ready: %s", strings.Join(notReadyNodes, ", ")))
	} else {
		degradedCondition = degradedCondition.
			WithStatus(operatorv1.ConditionFalse).
			WithReason("MasterNodesReady").
			WithMessage("All master nodes are ready")
	}
	status := applyoperatorv1.StaticPodOperatorStatus().
		WithConditions(degradedCondition).
		WithNodeStatuses(newTargetNodeStates...)

	if err = c.operatorClient.ApplyStaticPodOperatorStatus(ctx, c.controllerInstanceName, status); err != nil {
		return err
	}

	oldNodeDegradedCondition := v1helpers.FindOperatorCondition(originalOperatorStatus.Conditions, condition.NodeControllerDegradedConditionType)
	if oldNodeDegradedCondition == nil || oldNodeDegradedCondition.Message != *degradedCondition.Message {
		syncCtx.Recorder().Eventf("MasterNodesReadyChanged", *degradedCondition.Message)
	}
	return nil
}

func nodeConditionFinder(status *coreapiv1.NodeStatus, condType coreapiv1.NodeConditionType) *coreapiv1.NodeCondition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}

	return nil
}
