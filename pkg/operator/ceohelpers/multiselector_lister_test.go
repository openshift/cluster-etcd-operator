package ceohelpers

import (
	"context"
	"testing"
	time "time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

const (
	masterNodeLabelSelectorString  = "node-role.kubernetes.io/master"
	arbiterNodeLabelSelectorString = "node-role.kubernetes.io/arbiter"
	workerNodeLabelSelectorString  = "node-role.kubernetes.io/worker"
)

func TestMultiSelectors(t *testing.T) {
	testCases := []struct {
		name                string
		nodes               []corev1.Node
		expectedNodes       []corev1.Node
		addNodeAfterSync    *corev1.Node
		expectedErrorReason string
	}{
		{
			name: "control plane nodes should be returned",
			nodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master2",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
			},
			expectedNodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master2",
					},
				},
			},
		},
		{
			name: "control plane nodes should be returned with arbiter",
			nodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
						Labels: map[string]string{
							arbiterNodeLabelSelectorString: "",
						},
					},
				},
			},
			expectedNodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
					},
				},
			},
		},
		{
			name: "control plane nodes should be returned with arbiter and no workers",
			nodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
						Labels: map[string]string{
							arbiterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "worker0",
						Labels: map[string]string{
							workerNodeLabelSelectorString: "",
						},
					},
				},
			},
			expectedNodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
					},
				},
			},
		},
		{
			name: "control plane nodes should not include non labeled arbiter nodes",
			nodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
					},
				},
			},
			expectedNodes:       []corev1.Node{},
			expectedErrorReason: "NotFound",
		},
		{
			name: "control plane nodes should be returned with later add",
			nodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
						Labels: map[string]string{
							masterNodeLabelSelectorString: "",
						},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
						Labels: map[string]string{
							arbiterNodeLabelSelectorString: "",
						},
					},
				},
			},
			addNodeAfterSync: &corev1.Node{
				ObjectMeta: v1.ObjectMeta{
					Name: "master0",
					Labels: map[string]string{
						masterNodeLabelSelectorString: "",
					},
				},
			},
			expectedNodes: []corev1.Node{
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master0",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "master1",
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{
						Name: "arbiter0",
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			kubeClient := fake.NewClientset()
			for _, n := range tc.nodes {
				kubeClient.Tracker().Add(&n)
			}

			arbiterNodeLabelSelector, err := labels.Parse(arbiterNodeLabelSelectorString)
			require.Nil(t, err)
			masterNodeLabelSelector, err := labels.Parse(masterNodeLabelSelectorString)
			require.Nil(t, err)

			informer := corev1informers.NewNodeInformer(kubeClient, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

			lister := NewMultiSelectorNodeLister(informer.GetIndexer(), masterNodeLabelSelector, arbiterNodeLabelSelector)

			ctx, cancel := context.WithCancel(context.TODO())
			defer cancel()

			go informer.Run(ctx.Done())
			cache.WaitForCacheSync(ctx.Done(), informer.HasSynced)

			if tc.addNodeAfterSync != nil {
				err := informer.GetIndexer().Add(tc.addNodeAfterSync)
				require.NoError(t, err)
			}

			nodes, err := lister.List(labels.Everything())
			require.Nil(t, err)
			require.Equal(t, len(tc.expectedNodes), len(nodes))

			for _, n := range tc.expectedNodes {
				node, err := lister.Get(n.Name)
				if tc.expectedErrorReason != "" {
					require.Equal(t, tc.expectedErrorReason, string(errors.ReasonForError(err)))
				} else {
					require.Nil(t, err)
					require.NotEmpty(t, node)
				}
			}

		})
	}
}
