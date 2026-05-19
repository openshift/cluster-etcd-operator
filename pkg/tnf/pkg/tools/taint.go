package tools

import (
	"context"
	"fmt"
	"slices"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	OutOfServiceTaintKey        = "node.kubernetes.io/out-of-service"
	OutOfServiceTaintValue      = "nodeshutdown"
	OutOfServiceAnnotationKey   = "node.kubernetes.io/out-of-service-applied-by"
	OutOfServiceAnnotationValue = "pacemaker"

	conditionTypeTNFTaintRemoval = "TNFTaintRemovalControllerDegraded"
	reasonTaintRemovalFailed     = "TaintRemovalFailed"
)

func isOutOfServiceTaint(taint corev1.Taint) bool {
	return taint.Key == OutOfServiceTaintKey &&
		taint.Value == OutOfServiceTaintValue &&
		taint.Effect == corev1.TaintEffectNoExecute
}

func hasOutOfServiceTaint(node *corev1.Node) bool {
	return slices.ContainsFunc(node.Spec.Taints, isOutOfServiceTaint)
}

func hasOutOfServiceAnnotation(node *corev1.Node) bool {
	if node.Annotations == nil {
		return false
	}
	return node.Annotations[OutOfServiceAnnotationKey] == OutOfServiceAnnotationValue
}

func RemoveOutOfServiceTaintIfNeeded(ctx context.Context, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient, node *corev1.Node) {
	if !hasOutOfServiceAnnotation(node) {
		klog.V(4).Infof("node %s does not have %s=%s annotation, skipping (taint not ours to remove)", node.Name, OutOfServiceAnnotationKey, OutOfServiceAnnotationValue)
		return
	}

	klog.Infof("node %s has pacemaker annotation, cleaning up out-of-service taint and annotation", node.Name)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		freshNode, err := kubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("node %s no longer exists, skipping cleanup", node.Name)
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to get node %s: %w", node.Name, err)
		}

		if !hasOutOfServiceTaint(freshNode) && !hasOutOfServiceAnnotation(freshNode) {
			klog.V(4).Infof("node %s no longer has out-of-service taint or annotation after re-read, skipping", node.Name)
			return nil
		}

		freshNode.Spec.Taints = slices.DeleteFunc(freshNode.Spec.Taints, isOutOfServiceTaint)
		delete(freshNode.Annotations, OutOfServiceAnnotationKey)

		_, err = kubeClient.CoreV1().Nodes().Update(ctx, freshNode, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("node %s no longer exists during update, skipping cleanup", node.Name)
			return nil
		}
		return err
	})
	if err != nil {
		klog.Errorf("failed to remove out-of-service taint and annotation from node %s: %v", node.Name, err)
		setTaintRemovalDegradedCondition(ctx, operatorClient, node.Name, err)
		return
	}

	klog.Infof("successfully removed out-of-service taint and pacemaker annotation from node %s", node.Name)
	clearTaintRemovalDegradedCondition(ctx, operatorClient)
}

func setTaintRemovalDegradedCondition(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient, nodeName string, err error) {
	condition := operatorv1.OperatorCondition{
		Type:    conditionTypeTNFTaintRemoval,
		Status:  operatorv1.ConditionTrue,
		Reason:  reasonTaintRemovalFailed,
		Message: fmt.Sprintf("failed to remove out-of-service taint from node %s: %v", nodeName, err),
	}
	_, _, updateErr := v1helpers.UpdateStaticPodStatus(ctx, operatorClient, v1helpers.UpdateStaticPodConditionFn(condition))
	if updateErr != nil {
		klog.Errorf("failed to set %s condition: %v", conditionTypeTNFTaintRemoval, updateErr)
	}
}

func clearTaintRemovalDegradedCondition(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) {
	condition := operatorv1.OperatorCondition{
		Type:   conditionTypeTNFTaintRemoval,
		Status: operatorv1.ConditionFalse,
	}
	_, _, updateErr := v1helpers.UpdateStaticPodStatus(ctx, operatorClient, v1helpers.UpdateStaticPodConditionFn(condition))
	if updateErr != nil {
		klog.Errorf("failed to clear %s condition: %v", conditionTypeTNFTaintRemoval, updateErr)
	}
}
