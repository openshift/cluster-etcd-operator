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

	var filteredTaints []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if isOutOfServiceTaint(taint) {
			continue
		}
		filteredTaints = append(filteredTaints, taint)
	}

	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				OutOfServiceAnnotationKey: nil,
			},
		},
		"spec": map[string]any{
			"taints": filteredTaints,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch for node %s: %w", node.Name, err)
	}

	_, err = kubeClient.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.Errorf("failed to patch node %s to remove out-of-service taint and annotation: %v", node.Name, err)
		return fmt.Errorf("failed to patch node %s to remove out-of-service taint and annotation: %w", node.Name, err)
	}

	klog.Infof("successfully removed out-of-service taint and pacemaker annotation from node %s", node.Name)
	return nil
}
