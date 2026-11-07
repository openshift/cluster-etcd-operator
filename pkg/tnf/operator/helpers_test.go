package operator

/*
TEST COVERAGE SUMMARY - helpers_test.go
========================================

This file tests pacemaker node selection logic for job targeting.

WHAT'S TESTED
-------------

Node Selection (getActivePacemakerNodes):
├── Happy path - K8s ∩ Pacemaker intersection
│   ├── Both nodes in K8s and Pacemaker → return intersection
│   └── One node in intersection → return it
├── CR unavailable cases
│   ├── PacemakerCluster CR doesn't exist → return all ready K8s nodes
│   └── CR is stale (>5min old) → return all ready K8s nodes
├── Edge cases
│   ├── No intersection (K8s nodes not in Pacemaker) → fall back to all ready nodes
│   ├── No ready nodes → error
│   └── Node informer not synced → error
└── Node readiness filtering
    ├── Some nodes not ready → filter to ready only
    └── All nodes ready → return all
*/

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

func TestGetActivePacemakerNodes(t *testing.T) {
	tests := []struct {
		name                           string
		k8sNodes                       []*corev1.Node
		pacemakerCR                    *pacmkrv1.PacemakerCluster
		controlPlaneNodeInformerSynced bool
		expectedNodeNames              []string
		expectError                    bool
		errorContains                  string
	}{
		{
			name: "happy path - both nodes in K8s and Pacemaker intersection",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-0", "10.0.0.1"),
				makeReadyNode("master-1", "10.0.0.2"),
			},
			pacemakerCR: makePacemakerCR(
				map[string]string{
					"master-0": "10.0.0.1",
					"master-1": "10.0.0.2",
				},
				time.Now(), // Fresh CR
			),
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              []string{"master-0", "master-1"},
			expectError:                    false,
		},
		{
			name: "one node in intersection - returns that node",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-0", "10.0.0.1"),
				makeReadyNode("master-1", "10.0.0.2"),
			},
			pacemakerCR: makePacemakerCR(
				map[string]string{
					"master-0": "10.0.0.1",
					// master-1 not in pacemaker yet
				},
				time.Now(),
			),
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              []string{"master-0"},
			expectError:                    false,
		},
		{
			name: "CR doesn't exist - returns all ready K8s nodes",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-0", "10.0.0.1"),
				makeReadyNode("master-1", "10.0.0.2"),
			},
			pacemakerCR:                    nil, // CR doesn't exist
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              []string{"master-0", "master-1"},
			expectError:                    false,
		},
		{
			name: "CR is stale - returns all ready K8s nodes",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-0", "10.0.0.1"),
				makeReadyNode("master-1", "10.0.0.2"),
			},
			pacemakerCR: makePacemakerCR(
				map[string]string{
					"master-0": "10.0.0.1",
				},
				time.Now().Add(-10*time.Minute), // Stale (>5min)
			),
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              []string{"master-0", "master-1"},
			expectError:                    false,
		},
		{
			name: "no intersection - falls back to all ready nodes",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-2", "10.0.0.3"), // Different nodes in K8s
				makeReadyNode("master-3", "10.0.0.4"),
			},
			pacemakerCR: makePacemakerCR(
				map[string]string{
					"master-0": "10.0.0.1", // Different nodes in Pacemaker
					"master-1": "10.0.0.2",
				},
				time.Now(),
			),
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              []string{"master-2", "master-3"}, // All ready nodes
			expectError:                    false,
		},
		{
			name: "filters out not-ready nodes",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-0", "10.0.0.1"),
				makeNotReadyNode("master-1", "10.0.0.2"),
			},
			pacemakerCR: makePacemakerCR(
				map[string]string{
					"master-0": "10.0.0.1",
					"master-1": "10.0.0.2",
				},
				time.Now(),
			),
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              []string{"master-0"}, // Only ready node
			expectError:                    false,
		},
		{
			name: "no ready nodes - error",
			k8sNodes: []*corev1.Node{
				makeNotReadyNode("master-0", "10.0.0.1"),
				makeNotReadyNode("master-1", "10.0.0.2"),
			},
			pacemakerCR:                    nil,
			controlPlaneNodeInformerSynced: true,
			expectedNodeNames:              nil,
			expectError:                    true,
			errorContains:                  "no ready control plane nodes found",
		},
		{
			name: "node informer not synced - error",
			k8sNodes: []*corev1.Node{
				makeReadyNode("master-0", "10.0.0.1"),
			},
			pacemakerCR:                    nil,
			controlPlaneNodeInformerSynced: false,
			expectedNodeNames:              nil,
			expectError:                    true,
			errorContains:                  "node informer not synced yet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup node informer
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, node := range tt.k8sNodes {
				require.NoError(t, nodeIndexer.Add(node))
			}
			controlPlaneNodeInformer := &mockNodeInformer{
				indexer: nodeIndexer,
				synced:  tt.controlPlaneNodeInformerSynced,
			}

			// Setup pacemaker informer
			pacemakerIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if tt.pacemakerCR != nil {
				require.NoError(t, pacemakerIndexer.Add(tt.pacemakerCR))
			}
			pacemakerInformer := &mockPacemakerInformer{
				indexer: pacemakerIndexer,
			}

			// Create manager
			manager := &pacemakerLifecycleManager{
				controlPlaneNodeInformer: controlPlaneNodeInformer,
				pacemakerInformer:        pacemakerInformer,
			}

			// Execute
			result, err := manager.getActivePacemakerNodes()

			// Verify
			if tt.expectError {
				require.Error(t, err)
				if tt.errorContains != "" {
					require.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err)
				actualNodeNames := make([]string, len(result))
				for i, node := range result {
					actualNodeNames[i] = node.Name
				}
				require.ElementsMatch(t, tt.expectedNodeNames, actualNodeNames)
			}
		})
	}
}

// Test helpers

func makeReadyNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ip},
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

func makeNotReadyNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ip},
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}
}

func makePacemakerCR(nodes map[string]string, lastUpdated time.Time) *pacmkrv1.PacemakerCluster {
	var statusNodes []pacmkrv1.PacemakerClusterNodeStatus
	for nodeName, ip := range nodes {
		statusNodes = append(statusNodes, pacmkrv1.PacemakerClusterNodeStatus{
			NodeName: nodeName,
			Addresses: []pacmkrv1.PacemakerNodeAddress{
				{Address: ip},
			},
		})
	}

	return &pacmkrv1.PacemakerCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: pacemaker.PacemakerClusterResourceName,
		},
		Status: pacmkrv1.PacemakerClusterStatus{
			Nodes:       &statusNodes,
			LastUpdated: metav1.NewTime(lastUpdated),
		},
	}
}

// Mock informers

type mockNodeInformer struct {
	indexer cache.Indexer
	synced  bool
}

func (m *mockNodeInformer) GetIndexer() cache.Indexer {
	return m.indexer
}

func (m *mockNodeInformer) HasSynced() bool {
	return m.synced
}

func (m *mockNodeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockNodeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockNodeInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}

func (m *mockNodeInformer) GetStore() cache.Store {
	return m.indexer
}

func (m *mockNodeInformer) GetController() cache.Controller {
	return nil
}

func (m *mockNodeInformer) Run(stopCh <-chan struct{}) {
}

func (m *mockNodeInformer) LastSyncResourceVersion() string {
	return ""
}

func (m *mockNodeInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}

func (m *mockNodeInformer) SetTransform(f cache.TransformFunc) error {
	return nil
}

func (m *mockNodeInformer) IsStopped() bool {
	return false
}

type mockPacemakerInformer struct {
	indexer cache.Indexer
}

func (m *mockPacemakerInformer) GetStore() cache.Store {
	return m.indexer
}

func (m *mockPacemakerInformer) GetIndexer() cache.Indexer {
	return m.indexer
}

func (m *mockPacemakerInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockPacemakerInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockPacemakerInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}

func (m *mockPacemakerInformer) HasSynced() bool {
	return true
}

func (m *mockPacemakerInformer) HasSyncedChecker() cache.DoneChecker {
	return nil
}

func (m *mockPacemakerInformer) Run(stopCh <-chan struct{}) {
}

func (m *mockPacemakerInformer) LastSyncResourceVersion() string {
	return ""
}

func (m *mockPacemakerInformer) GetController() cache.Controller {
	return nil
}

func (m *mockPacemakerInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}

func (m *mockPacemakerInformer) SetTransform(f cache.TransformFunc) error {
	return nil
}

func (m *mockPacemakerInformer) IsStopped() bool {
	return false
}

func (m *mockPacemakerInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockPacemakerInformer) AddIndexers(indexers cache.Indexers) error {
	return nil
}

func (m *mockPacemakerInformer) RunWithContext(ctx context.Context) {
}

func (m *mockPacemakerInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}

func (m *mockNodeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (m *mockNodeInformer) AddIndexers(indexers cache.Indexers) error {
	return nil
}

func (m *mockNodeInformer) RunWithContext(ctx context.Context) {
}

func (m *mockNodeInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}

func (m *mockNodeInformer) HasSyncedChecker() cache.DoneChecker {
	return mockDoneChecker{synced: m.synced}
}

// mockDoneChecker is a cache.DoneChecker whose Done channel is closed only when
// synced is true, mirroring the informer mock's HasSynced result.
type mockDoneChecker struct {
	synced bool
}

func (mockDoneChecker) Name() string { return "mockInformer" }
func (c mockDoneChecker) Done() <-chan struct{} {
	ch := make(chan struct{})
	if c.synced {
		close(ch)
	}
	return ch
}
