package operator

/*
TEST COVERAGE SUMMARY - lifecycle_reconciliation_test.go
=========================================================

This file tests drift detection and reconciliation decision logic for Two-Node Fencing clusters.

WHAT'S TESTED
-------------

Drift Detection and Reconciliation:
└── TestDetermineReconciliationActions - Reconciliation decision logic (single source of truth)
    ├── ✅ No drift (both nodes match name+IP)
    ├── ✅ Node deleted from K8s (remove from pacemaker)
    ├── ✅ Node added to K8s (add to pacemaker)
    ├── ✅ Node replaced (different name, remove old + add new)
    ├── ✅ Node IP changed (same name, remove + re-add)
    ├── ✅ Pacemaker empty (add all K8s nodes)
    └── ✅ K8s empty (remove all pacemaker nodes)
*/

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// TestDetermineReconciliationActions tests the reconciliation action decision logic.
// This is the single source of truth for reconciliation decisions.
func TestDetermineReconciliationActions(t *testing.T) {
	tests := []struct {
		name           string
		k8sNodes       map[string]string // name -> IP
		pacemakerNodes map[string]string // name -> IP
		expectRemove   []string
		expectAdd      []string
	}{
		{
			name: "no drift - both nodes match (name+IP)",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectRemove: nil,
			expectAdd:    nil,
		},
		{
			name: "node deleted from k8s - remove from pacemaker",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectRemove: []string{"master-1"},
			expectAdd:    nil,
		},
		{
			name: "node added to k8s - add to pacemaker",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
			},
			expectRemove: nil,
			expectAdd:    []string{"master-1"},
		},
		{
			name: "node replaced (different name) - remove old, add new",
			k8sNodes: map[string]string{
				"master-0":     "192.168.1.10",
				"new-master-1": "192.168.1.20",
			},
			pacemakerNodes: map[string]string{
				"master-0":     "192.168.1.10",
				"old-master-1": "192.168.1.11",
			},
			expectRemove: []string{"old-master-1"},
			expectAdd:    []string{"new-master-1"},
		},
		{
			name: "node IP changed (same name) - remove old, add new",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.20", // IP changed
			},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11", // Old IP
			},
			expectRemove: []string{"master-1"},
			expectAdd:    []string{"master-1"},
		},
		{
			name: "pacemaker empty - add all k8s nodes",
			k8sNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			pacemakerNodes: map[string]string{},
			expectRemove:   nil,
			expectAdd:      []string{"master-0", "master-1"},
		},
		{
			name:     "k8s empty - remove all pacemaker nodes",
			k8sNodes: map[string]string{},
			pacemakerNodes: map[string]string{
				"master-0": "192.168.1.10",
				"master-1": "192.168.1.11",
			},
			expectRemove: []string{"master-0", "master-1"},
			expectAdd:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodesToRemove, nodesToAdd := tools.DetermineReconciliationActions(tt.k8sNodes, tt.pacemakerNodes)

			// Use ElementsMatch for order-independent comparison
			require.ElementsMatch(t, tt.expectRemove, nodesToRemove,
				"Expected to remove %v but got %v", tt.expectRemove, nodesToRemove)
			require.ElementsMatch(t, tt.expectAdd, nodesToAdd,
				"Expected to add %v but got %v", tt.expectAdd, nodesToAdd)
		})
	}
}
