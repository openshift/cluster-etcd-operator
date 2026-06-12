package ceohelpers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

func TestListNodesFromInformer(t *testing.T) {
	tests := []struct {
		name        string
		nodes       []*corev1.Node
		expectError bool
		expectCount int
	}{
		{
			name: "list all nodes",
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "master-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"}},
			},
			expectError: false,
			expectCount: 3,
		},
		{
			name:        "empty node list",
			nodes:       []*corev1.Node{},
			expectError: false,
			expectCount: 0,
		},
		{
			name: "single node",
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}},
			},
			expectError: false,
			expectCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake informer with nodes
			informer := createFakeNodeInformer(t, tt.nodes)

			// Execute
			nodes, err := ListNodesFromInformer(informer)

			// Verify
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Len(t, nodes, tt.expectCount)
			}
		})
	}
}

func TestListNodesFromInformer_NilInformer(t *testing.T) {
	nodes, err := ListNodesFromInformer(nil)

	require.Error(t, err)
	require.Nil(t, nodes)
	require.Contains(t, err.Error(), "informer is nil")
}

func TestListNodesBySelector(t *testing.T) {
	tests := []struct {
		name        string
		nodes       []*corev1.Node
		selector    labels.Selector
		expectError bool
		expectCount int
		expectNames []string
	}{
		{
			name: "select master nodes only",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "master-0",
						Labels: map[string]string{ControlPlaneNodeLabelSelector: ""},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "master-1",
						Labels: map[string]string{ControlPlaneNodeLabelSelector: ""},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "worker-0",
						Labels: map[string]string{"node-role.kubernetes.io/worker": ""},
					},
				},
			},
			selector:    labels.SelectorFromSet(map[string]string{ControlPlaneNodeLabelSelector: ""}),
			expectError: false,
			expectCount: 2,
			expectNames: []string{"master-0", "master-1"},
		},
		{
			name: "select arbiter nodes only",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "master-0",
						Labels: map[string]string{ControlPlaneNodeLabelSelector: ""},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "arbiter-0",
						Labels: map[string]string{ArbiterNodeLabelSelector: ""},
					},
				},
			},
			selector:    labels.SelectorFromSet(map[string]string{ArbiterNodeLabelSelector: ""}),
			expectError: false,
			expectCount: 1,
			expectNames: []string{"arbiter-0"},
		},
		{
			name: "no matching nodes",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "worker-0",
						Labels: map[string]string{"node-role.kubernetes.io/worker": ""},
					},
				},
			},
			selector:    labels.SelectorFromSet(map[string]string{ControlPlaneNodeLabelSelector: ""}),
			expectError: false,
			expectCount: 0,
			expectNames: []string{},
		},
		{
			name: "select all nodes with Everything() selector",
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"}},
			},
			selector:    labels.Everything(),
			expectError: false,
			expectCount: 2,
			expectNames: []string{"master-0", "worker-0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake informer with nodes
			informer := createFakeNodeInformer(t, tt.nodes)

			// Execute
			nodes, err := ListNodesBySelector(informer, tt.selector)

			// Verify
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Len(t, nodes, tt.expectCount)

				// Verify node names match
				actualNames := make([]string, len(nodes))
				for i, node := range nodes {
					actualNames[i] = node.Name
				}
				require.ElementsMatch(t, tt.expectNames, actualNames)
			}
		})
	}
}

func TestListNodesBySelector_NilInformer(t *testing.T) {
	selector := labels.SelectorFromSet(map[string]string{ControlPlaneNodeLabelSelector: ""})
	nodes, err := ListNodesBySelector(nil, selector)

	require.Error(t, err)
	require.Nil(t, nodes)
	require.Contains(t, err.Error(), "informer is nil")
}

// createFakeNodeInformer creates a fake SharedIndexInformer populated with the given nodes
func createFakeNodeInformer(t *testing.T, nodes []*corev1.Node) cache.SharedIndexInformer {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, node := range nodes {
		if err := indexer.Add(node); err != nil {
			t.Fatalf("indexer.Add failed: %v", err)
		}
	}

	return &fakeNodeInformer{indexer: indexer}
}

// fakeNodeInformer implements cache.SharedIndexInformer for testing
type fakeNodeInformer struct {
	indexer cache.Indexer
}

func (f *fakeNodeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeNodeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeNodeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeNodeInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}
func (f *fakeNodeInformer) GetStore() cache.Store              { return f.indexer }
func (f *fakeNodeInformer) GetController() cache.Controller    { return nil }
func (f *fakeNodeInformer) Run(stopCh <-chan struct{})         {}
func (f *fakeNodeInformer) RunWithContext(ctx context.Context) {}
func (f *fakeNodeInformer) HasSynced() bool                    { return true }
func (f *fakeNodeInformer) LastSyncResourceVersion() string    { return "" }
func (f *fakeNodeInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}
func (f *fakeNodeInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (f *fakeNodeInformer) SetTransform(handler cache.TransformFunc) error { return nil }
func (f *fakeNodeInformer) IsStopped() bool                                { return false }
func (f *fakeNodeInformer) AddIndexers(indexers cache.Indexers) error      { return nil }
func (f *fakeNodeInformer) GetIndexer() cache.Indexer                      { return f.indexer }
