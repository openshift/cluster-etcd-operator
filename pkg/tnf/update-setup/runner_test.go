package updatesetup

/*
TEST COVERAGE SUMMARY - runner_test.go
=======================================

This file tests the update-setup runner's ConfigMap handling for TNF reconciliation.

WHAT'S TESTED
-------------

ConfigMap Selection (cluster-wide model):
└── TestPickLatestUpdateSetupConfigMap - Cluster-wide ConfigMap selection by generation
    ├── ✅ Picks highest generation among multiple ConfigMaps
    ├── ✅ Handles single ConfigMap
    ├── ✅ Returns error when no ConfigMaps exist
    └── ✅ Skips invalid generation strings

WHAT'S NOT TESTED (covered elsewhere)
--------------------------------------
- Reconciliation logic: tested in nodehandler_test.go (single source of truth)
- Helper functions (decodeStringList): implicitly tested through business logic
- Pacemaker command execution: integration tests
- ConfigMap creation: tested in lifecycle_manager_test.go
*/

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPickLatestUpdateSetupConfigMap verifies that the runner picks the ConfigMap
// with the highest generation number (cluster-wide, no node filtering)
func TestPickLatestUpdateSetupConfigMap(t *testing.T) {
	tests := []struct {
		name           string
		items          []corev1.ConfigMap
		expectName     string
		expectGen      string
		expectError    bool
		expectErrorMsg string
	}{
		{
			name: "picks highest generation",
			items: []corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "old"},
					Data: map[string]string{
						"generation": "3",
						"eventType":  "add",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "newest"},
					Data: map[string]string{
						"generation": "10",
						"eventType":  "drift-reconciliation",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "middle"},
					Data: map[string]string{
						"generation": "7",
						"eventType":  "delete",
					},
				},
			},
			expectName:  "newest",
			expectGen:   "10",
			expectError: false,
		},
		{
			name: "single ConfigMap",
			items: []corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "only"},
					Data: map[string]string{
						"generation": "5",
						"eventType":  "add",
					},
				},
			},
			expectName:  "only",
			expectGen:   "5",
			expectError: false,
		},
		{
			name:           "no ConfigMaps returns error",
			items:          []corev1.ConfigMap{},
			expectError:    true,
			expectErrorMsg: "no update-setup ConfigMap found",
		},
		{
			name: "skips invalid generation",
			items: []corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "valid"},
					Data: map[string]string{
						"generation": "5",
						"eventType":  "add",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "invalid"},
					Data: map[string]string{
						"generation": "not-a-number",
						"eventType":  "add",
					},
				},
			},
			expectName:  "valid",
			expectGen:   "5",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm, err := pickLatestUpdateSetupConfigMap(tt.items)

			if tt.expectError {
				require.Error(t, err)
				if tt.expectErrorMsg != "" {
					require.Contains(t, err.Error(), tt.expectErrorMsg)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectName, cm.Name)
				require.Equal(t, tt.expectGen, cm.Data["generation"])
			}
		})
	}
}

// TestDecodeStringList verifies JSON decoding of string lists from ConfigMap
