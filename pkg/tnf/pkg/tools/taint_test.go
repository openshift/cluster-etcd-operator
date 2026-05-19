package tools

import (
	"context"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHasOutOfServiceTaint(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{
			name: "node with matching taint",
			node: &corev1.Node{
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    OutOfServiceTaintKey,
							Value:  OutOfServiceTaintValue,
							Effect: corev1.TaintEffectNoExecute,
						},
					},
				},
			},
			want: true,
		},
		{
			name: "node with matching key but wrong value",
			node: &corev1.Node{
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    OutOfServiceTaintKey,
							Value:  "other",
							Effect: corev1.TaintEffectNoExecute,
						},
					},
				},
			},
			want: false,
		},
		{
			name: "node with matching key and value but wrong effect",
			node: &corev1.Node{
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    OutOfServiceTaintKey,
							Value:  OutOfServiceTaintValue,
							Effect: corev1.TaintEffectNoSchedule,
						},
					},
				},
			},
			want: false,
		},
		{
			name: "node with no taints",
			node: &corev1.Node{},
			want: false,
		},
		{
			name: "node with other taints only",
			node: &corev1.Node{
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    corev1.TaintNodeNotReady,
							Effect: corev1.TaintEffectNoSchedule,
						},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasOutOfServiceTaint(tt.node)
			require.Equal(t, tt.want, result)
		})
	}
}

func Test_hasOutOfServiceAnnotation(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{
			name: "node with matching annotation",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OutOfServiceAnnotationKey: OutOfServiceAnnotationValue,
					},
				},
			},
			want: true,
		},
		{
			name: "node with matching key but wrong value",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						OutOfServiceAnnotationKey: "other-controller",
					},
				},
			},
			want: false,
		},
		{
			name: "node with no annotations",
			node: &corev1.Node{},
			want: false,
		},
		{
			name: "node with other annotations only",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"some-other-annotation": "value",
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasOutOfServiceAnnotation(tt.node)
			require.Equal(t, tt.want, result)
		})
	}
}

func TestRemoveOutOfServiceTaintIfNeeded(t *testing.T) {
	tests := []struct {
		name           string
		node           *corev1.Node
		expectModified bool
	}{
		{
			name: "both taint and annotation present - removes both",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
					Annotations: map[string]string{
						OutOfServiceAnnotationKey: OutOfServiceAnnotationValue,
						"other-annotation":        "keep-me",
					},
				},
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    corev1.TaintNodeNotReady,
							Effect: corev1.TaintEffectNoSchedule,
						},
						{
							Key:    OutOfServiceTaintKey,
							Value:  OutOfServiceTaintValue,
							Effect: corev1.TaintEffectNoExecute,
						},
					},
				},
			},
			expectModified: true,
		},
		{
			name: "taint only, no annotation - no-op",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    OutOfServiceTaintKey,
							Value:  OutOfServiceTaintValue,
							Effect: corev1.TaintEffectNoExecute,
						},
					},
				},
			},
			expectModified: false,
		},
		{
			name: "annotation only, no taint - removes stale annotation",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
					Annotations: map[string]string{
						OutOfServiceAnnotationKey: OutOfServiceAnnotationValue,
					},
				},
			},
			expectModified: true,
		},
		{
			name: "neither taint nor annotation - no-op",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
				},
			},
			expectModified: false,
		},
		{
			name: "taint key matches but wrong value - removes stale annotation, preserves unrelated taint",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
					Annotations: map[string]string{
						OutOfServiceAnnotationKey: OutOfServiceAnnotationValue,
					},
				},
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    OutOfServiceTaintKey,
							Value:  "other-value",
							Effect: corev1.TaintEffectNoExecute,
						},
					},
				},
			},
			expectModified: true,
		},
		{
			name: "annotation key matches but wrong value - no-op",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "master-0",
					Annotations: map[string]string{
						OutOfServiceAnnotationKey: "other-controller",
					},
				},
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:    OutOfServiceTaintKey,
							Value:  OutOfServiceTaintValue,
							Effect: corev1.TaintEffectNoExecute,
						},
					},
				},
			},
			expectModified: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := fake.NewClientset(tt.node)
			operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&operatorv1.StaticPodOperatorStatus{},
				nil,
				nil,
			)

			RemoveOutOfServiceTaintIfNeeded(context.Background(), kubeClient, operatorClient, tt.node)

			actions := kubeClient.Actions()
			hasUpdate := false
			hasPatch := false
			for _, action := range actions {
				if action.GetResource().Resource == "nodes" {
					if action.GetVerb() == "update" {
						hasUpdate = true
					}
					if action.GetVerb() == "patch" {
						hasPatch = true
					}
				}
			}

			if tt.expectModified {
				require.True(t, hasUpdate, "expected a node update action for taint and annotation removal")
				require.False(t, hasPatch, "expected no separate patch — taint and annotation should be removed atomically in a single update")

				updatedNode, err := kubeClient.CoreV1().Nodes().Get(context.Background(), tt.node.Name, metav1.GetOptions{})
				require.NoError(t, err)

				require.False(t, hasOutOfServiceTaint(updatedNode),
					"expected out-of-service taint to be removed")
				for _, taint := range tt.node.Spec.Taints {
					if isOutOfServiceTaint(taint) {
						continue
					}
					found := false
					for _, remaining := range updatedNode.Spec.Taints {
						if remaining.Key == taint.Key && remaining.Value == taint.Value && remaining.Effect == taint.Effect {
							found = true
							break
						}
					}
					require.True(t, found, "expected taint %s=%s:%s to be preserved", taint.Key, taint.Value, taint.Effect)
				}

				_, exists := updatedNode.Annotations[OutOfServiceAnnotationKey]
				require.False(t, exists, "expected out-of-service annotation to be removed")
				for key, val := range tt.node.Annotations {
					if key != OutOfServiceAnnotationKey {
						require.Equal(t, val, updatedNode.Annotations[key],
							"expected annotation %s to be preserved", key)
					}
				}
			} else {
				require.False(t, hasUpdate, "expected no node update action")
				require.False(t, hasPatch, "expected no node patch action")
			}
		})
	}
}
