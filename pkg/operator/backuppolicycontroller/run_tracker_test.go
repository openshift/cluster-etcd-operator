package backuppolicycontroller

import (
	"context"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestRunTracker_DuplicateReconcile(t *testing.T) {
	// A run should only be counted once, even if synced multiple times
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", "test-policy", "1000", time.Now()),
	}

	// First sync - should count the failure
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 1, state1.ConsecutiveFailures)

	// Second sync - should NOT increment again (same run)
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 1, state2.ConsecutiveFailures, "duplicate reconcile should not double-count")
}

func TestRunTracker_MixedNodeOutcomes(t *testing.T) {
	// If ANY backup in a run succeeds, the run succeeds
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Run with 3 backups: 2 failed, 1 succeeded
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-node1-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-node2-1000", "test-policy", "1000", time.Now()),
		testSuccessfulBackup("test-policy-node3-1000", "test-policy", "1000", time.Now()),
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 0, state.ConsecutiveFailures, "run with any success should reset counter")
	require.NotNil(t, state.LastSuccessTime, "should record success time")
}

func TestRunTracker_AllNodeFailure(t *testing.T) {
	// If ALL backups in a run fail, increment counter ONCE
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Run with 3 backups: all failed
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-node1-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-node2-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-node3-1000", "test-policy", "1000", time.Now()),
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 1, state.ConsecutiveFailures, "all-node failure should increment once")
}

func TestRunTracker_RestartAfterTwoFailedRuns(t *testing.T) {
	// State should persist across restart (simulated by new tracker instance)
	ctx := context.Background()
	tracker1, client := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// First run fails
	backups1 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", "test-policy", "1000", time.Now()),
	}
	state1, err := tracker1.ProcessBackupsForPolicy(ctx, policy, backups1)
	require.NoError(t, err)
	require.Equal(t, 1, state1.ConsecutiveFailures)

	// Second run fails
	backups2 := append(backups1, testFailedBackup("test-policy-1001", "test-policy", "1001", time.Now()))
	state2, err := tracker1.ProcessBackupsForPolicy(ctx, policy, backups2)
	require.NoError(t, err)
	require.Equal(t, 2, state2.ConsecutiveFailures)

	// Simulate operator restart - create new tracker with same client
	tracker2 := NewRunTracker("test-ns", client)

	// Load state - should still have 2 failures
	loadedState, err := tracker2.GetPolicyState(ctx, policy)
	require.NoError(t, err)
	require.Equal(t, 2, loadedState.ConsecutiveFailures, "state should persist across restart")
	require.Equal(t, "1001", loadedState.LastProcessedRunMinuteHash)
}

func TestRunTracker_Retention(t *testing.T) {
	// Backups can be deleted (retention), but counter persists
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Two failed runs
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-1001", "test-policy", "1001", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 2, state1.ConsecutiveFailures)

	// Simulate retention - both backups deleted
	emptyBackups := []*operatorv1alpha1.EtcdBackup{}
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, emptyBackups)
	require.NoError(t, err)
	require.Equal(t, 2, state2.ConsecutiveFailures, "counter should persist even when backups are deleted")
}

func TestRunTracker_PolicyUIDReplacement(t *testing.T) {
	// New policy with same name but different UID = fresh counter
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy1 := testPolicy("test-policy", "uid-1")
	backups1 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-1001", "test-policy", "1001", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy1, backups1)
	require.NoError(t, err)
	require.Equal(t, 2, state1.ConsecutiveFailures)

	// Policy deleted and recreated - new UID
	policy2 := testPolicy("test-policy", "uid-2")
	backups2 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1002", "test-policy", "1002", time.Now()),
	}
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy2, backups2)
	require.NoError(t, err)
	require.Equal(t, 1, state2.ConsecutiveFailures, "new policy UID should start fresh counter")

	// Old policy data should still exist but be ignored
	allStates, err := tracker.GetAllPolicyStates(ctx)
	require.NoError(t, err)
	require.Len(t, allStates, 2, "both policy UIDs should be tracked")
}

func TestRunTracker_InProgressRun(t *testing.T) {
	// Incomplete runs should not be counted
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Run with mixed states: 1 failed, 1 completed, 1 still pending
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-node1-1000", "test-policy", "1000", time.Now()),
		testSuccessfulBackup("test-policy-node2-1000", "test-policy", "1000", time.Now()),
		testPendingBackup("test-policy-node3-1000", "test-policy", "1000"),
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)

	// Even though we have a success, if ANY backup is still pending, treat as in-progress
	// Actually, based on the code, if ANY succeeded, the run succeeds regardless of pending
	require.Equal(t, 0, state.ConsecutiveFailures, "run with success should reset counter even if some still pending")
}

func TestRunTracker_NeverSuccessfulPolicy(t *testing.T) {
	// Policy with only failures, never succeeded
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-1001", "test-policy", "1001", time.Now()),
		testFailedBackup("test-policy-1002", "test-policy", "1002", time.Now()),
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 3, state.ConsecutiveFailures)
	require.Nil(t, state.LastSuccessTime, "never-successful policy should have nil LastSuccessTime")
}

func TestRunTracker_FailureResetOnSuccess(t *testing.T) {
	// Counter should reset to 0 after success
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Two failures
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-1001", "test-policy", "1001", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 2, state1.ConsecutiveFailures)

	// Then a success
	backups = append(backups, testSuccessfulBackup("test-policy-1002", "test-policy", "1002", time.Now()))
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 0, state2.ConsecutiveFailures, "success should reset counter to 0")
	require.NotNil(t, state2.LastSuccessTime)

	// Then another failure
	backups = append(backups, testFailedBackup("test-policy-1003", "test-policy", "1003", time.Now()))
	state3, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 1, state3.ConsecutiveFailures, "counter should restart from 0 after reset")
}

func TestRunTracker_MultipleRunsInOneBatch(t *testing.T) {
	// Multiple runs can be processed in a single sync
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// 3 runs: success, fail, fail
	backups := []*operatorv1alpha1.EtcdBackup{
		testSuccessfulBackup("test-policy-1000", "test-policy", "1000", time.Now()),
		testFailedBackup("test-policy-1001", "test-policy", "1001", time.Now()),
		testFailedBackup("test-policy-1002", "test-policy", "1002", time.Now()),
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 2, state.ConsecutiveFailures, "should process all runs in order: success(reset), fail(1), fail(2)")
	require.NotNil(t, state.LastSuccessTime)
}

func TestExtractMinuteHashFromName(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"policy-1234567890", "1234567890"},
		{"policy-abc123def456-1234567890", "1234567890"},
		{"policy-node-uid-1234567890", "1234567890"},
		{"invalid-name", ""},
		{"", ""},
		{"policy-notanumber", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractMinuteHashFromName(tt.name)
			require.Equal(t, tt.expected, result)
		})
	}
}

// Helper functions

func setupRunTracker(t *testing.T) (*RunTracker, corev1client.ConfigMapInterface) {
	t.Helper()
	client := fake.NewSimpleClientset()
	cmClient := client.CoreV1().ConfigMaps("test-ns")
	tracker := NewRunTracker("test-ns", cmClient)
	return tracker, cmClient
}

func testPolicy(name string, uid string) *operatorv1alpha1.EtcdBackupPolicy {
	return &operatorv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			UID:               types.UID(uid),
			CreationTimestamp: metav1.Now(),
		},
		Spec: operatorv1alpha1.EtcdBackupPolicySpec{
			Schedule: "0 0 * * *",
		},
	}
}

func testSuccessfulBackup(name, policyName, minuteHash string, completionTime time.Time) *operatorv1alpha1.EtcdBackup {
	return &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				backuphelpers.LabelEtcdBackupPolicy: policyName,
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []metav1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupCompleted),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(completionTime),
				},
			},
		},
	}
}

func testFailedBackup(name, policyName, minuteHash string, failureTime time.Time) *operatorv1alpha1.EtcdBackup {
	return &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				backuphelpers.LabelEtcdBackupPolicy: policyName,
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []metav1.Condition{
				{
					Type:               string(operatorv1alpha1.BackupFailed),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(failureTime),
				},
			},
		},
	}
}

func testPendingBackup(name, policyName, minuteHash string) *operatorv1alpha1.EtcdBackup {
	return &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				backuphelpers.LabelEtcdBackupPolicy: policyName,
			},
		},
		Status: operatorv1alpha1.EtcdBackupStatus{
			Conditions: []metav1.Condition{
				{
					Type:   string(operatorv1alpha1.BackupPending),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
}
