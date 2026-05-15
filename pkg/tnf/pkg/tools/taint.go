package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	OutOfServiceTaintKey        = "node.kubernetes.io/out-of-service"
	OutOfServiceTaintValue      = "nodeshutdown"
	OutOfServiceAnnotationKey   = "node.kubernetes.io/out-of-service-applied-by"
	OutOfServiceAnnotationValue = "pacemaker"
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

func RemoveOutOfServiceTaintIfNeeded(ctx context.Context, kubeClient kubernetes.Interface, node *corev1.Node) error {
	if !hasOutOfServiceTaint(node) || !hasOutOfServiceAnnotation(node) {
		klog.V(4).Infof("node %s does not have both out-of-service taint and pacemaker annotation, skipping", node.Name)
		return nil
	}

	klog.Infof("node %s has out-of-service taint and pacemaker annotation, removing both", node.Name)

	// Use read-modify-update with conflict retry for taints to avoid clobbering concurrent changes
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		freshNode, err := kubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node %s: %w", node.Name, err)
		}

		if !hasOutOfServiceTaint(freshNode) {
			klog.V(4).Infof("node %s no longer has out-of-service taint after re-read, skipping taint removal", node.Name)
			return nil
		}

		freshNode.Spec.Taints = slices.DeleteFunc(freshNode.Spec.Taints, isOutOfServiceTaint)

		_, err = kubeClient.CoreV1().Nodes().Update(ctx, freshNode, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		klog.Errorf("failed to remove out-of-service taint from node %s: %v", node.Name, err)
		return fmt.Errorf("failed to remove out-of-service taint from node %s: %w", node.Name, err)
	}

	// Remove the annotation via merge patch — annotations are a map so no clobber risk
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				OutOfServiceAnnotationKey: nil,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal annotation patch for node %s: %w", node.Name, err)
	}

	_, err = kubeClient.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.Errorf("failed to remove out-of-service annotation from node %s: %v", node.Name, err)
		return fmt.Errorf("failed to remove out-of-service annotation from node %s: %w", node.Name, err)
	}

	klog.Infof("successfully removed out-of-service taint and pacemaker annotation from node %s", node.Name)
	return nil
}
