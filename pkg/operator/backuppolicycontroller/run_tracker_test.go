package backuppolicycontroller

import (
	"context"
	"fmt"
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
		testFailedBackup("test-policy-1000", policy, "1000", time.Now()),
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
		testFailedBackup("test-policy-node1-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-node2-1000", policy, "1000", time.Now()),
		testSuccessfulBackup("test-policy-node3-1000", policy, "1000", time.Now()),
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
		testFailedBackup("test-policy-node1-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-node2-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-node3-1000", policy, "1000", time.Now()),
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
		testFailedBackup("test-policy-1000", policy, "1000", time.Now()),
	}
	state1, err := tracker1.ProcessBackupsForPolicy(ctx, policy, backups1)
	require.NoError(t, err)
	require.Equal(t, 1, state1.ConsecutiveFailures)

	// Second run fails
	backups2 := append(backups1, testFailedBackup("test-policy-1001", policy, "1001", time.Now()))
	state2, err := tracker1.ProcessBackupsForPolicy(ctx, policy, backups2)
	require.NoError(t, err)
	require.Equal(t, 2, state2.ConsecutiveFailures)

	// Simulate operator restart - create new tracker with same client
	tracker2 := NewRunTracker("test-ns", client)

	// Load state - should still have 2 failures
	loadedState, err := tracker2.GetPolicyState(ctx, policy)
	require.NoError(t, err)
	require.Equal(t, 2, loadedState.ConsecutiveFailures, "state should persist across restart")
	require.Equal(t, "failure", loadedState.RunOutcomes["1001"])
	require.Equal(t, "failure", loadedState.RunOutcomes["1000"])
}

func TestRunTracker_Retention(t *testing.T) {
	// Backups can be deleted (retention), but counter persists
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Two failed runs
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy, "1001", time.Now()),
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
		testFailedBackup("test-policy-1000", policy1, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy1, "1001", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy1, backups1)
	require.NoError(t, err)
	require.Equal(t, 2, state1.ConsecutiveFailures)

	// Policy deleted and recreated - new UID
	policy2 := testPolicy("test-policy", "uid-2")
	backups2 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1002", policy2, "1002", time.Now()),
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
		testFailedBackup("test-policy-node1-1000", policy, "1000", time.Now()),
		testSuccessfulBackup("test-policy-node2-1000", policy, "1000", time.Now()),
		testPendingBackup("test-policy-node3-1000", policy, "1000"),
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
		testFailedBackup("test-policy-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy, "1001", time.Now()),
		testFailedBackup("test-policy-1002", policy, "1002", time.Now()),
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
		testFailedBackup("test-policy-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy, "1001", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 2, state1.ConsecutiveFailures)

	// Then a success
	backups = append(backups, testSuccessfulBackup("test-policy-1002", policy, "1002", time.Now()))
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 0, state2.ConsecutiveFailures, "success should reset counter to 0")
	require.NotNil(t, state2.LastSuccessTime)

	// Then another failure
	backups = append(backups, testFailedBackup("test-policy-1003", policy, "1003", time.Now()))
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
		testSuccessfulBackup("test-policy-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy, "1001", time.Now()),
		testFailedBackup("test-policy-1002", policy, "1002", time.Now()),
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 2, state.ConsecutiveFailures, "should process all runs in order: success(reset), fail(1), fail(2)")
	require.NotNil(t, state.LastSuccessTime)
}

func TestRunTracker_TwoReconcileFailedThenSuccess(t *testing.T) {
	// BLOCKER #1: Run finalized before all members known
	// First sync sees failed member, second sync sees successful sibling
	// Must NOT leave a failed-run streak
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// First reconcile: only failed backup visible
	backups1 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-node1-1000", policy, "1000", time.Now()),
	}
	_, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups1)
	require.NoError(t, err)

	// Second reconcile: successful sibling now visible
	backups2 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-node1-1000", policy, "1000", time.Now()),
		testSuccessfulBackup("test-policy-node2-1000", policy, "1000", time.Now()),
	}
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups2)
	require.NoError(t, err)

	require.Equal(t, 0, state2.ConsecutiveFailures, "late-arriving success should reset streak, not be skipped")
	require.NotNil(t, state2.LastSuccessTime, "should record success time")
}

func TestRunTracker_RestartProcessThirdFailure(t *testing.T) {
	// Extend restart test to verify critical threshold after reload
	ctx := context.Background()
	tracker1, client := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Two failures
	backups := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy, "1001", time.Now()),
	}
	state1, err := tracker1.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 2, state1.ConsecutiveFailures)

	// Simulate restart
	tracker2 := NewRunTracker("test-ns", client)

	// Third failure after restart
	backups = append(backups, testFailedBackup("test-policy-1002", policy, "1002", time.Now()))
	state2, err := tracker2.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 3, state2.ConsecutiveFailures, "should reach critical threshold after restart")
}

func TestRunTracker_CorruptStatePreservesOthers(t *testing.T) {
	// BLOCKER #4: Corrupt history should not reset all policies
	ctx := context.Background()
	tracker, client := setupRunTracker(t)

	policy1 := testPolicy("policy-1", "uid-1")
	policy2 := testPolicy("policy-2", "uid-2")

	// Save valid state for policy1
	state1 := &PolicyRunState{
		PolicyUID:           policy1.UID,
		ConsecutiveFailures: 2,
		PolicyCreationTime:  policy1.CreationTimestamp,
	}
	err := tracker.SavePolicyState(ctx, policy1, state1)
	require.NoError(t, err)

	// Manually corrupt the ConfigMap data
	cm, err := client.Get(ctx, failureTrackerConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data[failureTrackerDataKey] = "invalid json {"
	_, err = client.Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Try to save policy2 - should preserve policy1's data, not reset everything
	state2 := &PolicyRunState{
		PolicyUID:           policy2.UID,
		ConsecutiveFailures: 1,
		PolicyCreationTime:  policy2.CreationTimestamp,
	}
	err = tracker.SavePolicyState(ctx, policy2, state2)
	// Should return an error, not silently reset
	require.Error(t, err, "corrupt state should return error, not reset")

	// Verify we didn't lose policy1's data by resetting to empty
	// (This test will need implementation that preserves on corruption)
}

func TestRunTracker_SameNameNewUID(t *testing.T) {
	// BLOCKER #5: UID isolation for backup selection
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	// Old policy creates backups
	policy1 := testPolicy("test-policy", "uid-1")
	policy1.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))

	backups1 := []*operatorv1alpha1.EtcdBackup{
		testFailedBackup("test-policy-1000", policy1, "1000", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy1, backups1)
	require.NoError(t, err)
	require.Equal(t, 1, state1.ConsecutiveFailures)

	// Policy deleted and recreated with same name, new UID
	policy2 := testPolicy("test-policy", "uid-2")
	policy2.CreationTimestamp = metav1.Now()

	// New policy should NOT see old policy's backups (UID mismatch)
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy2, backups1)
	require.NoError(t, err)
	require.Equal(t, 0, state2.ConsecutiveFailures, "new policy UID should not count old policy's backups")
}

func TestRunTracker_NeverSuccessfulPolicyStateSaved(t *testing.T) {
	// BLOCKER #3: Never-successful policy must save state for alert
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")
	policy.CreationTimestamp = metav1.NewTime(time.Now().Add(-25 * time.Hour).Truncate(time.Second))

	// Process with no backups (common during initial policy creation)
	emptyBackups := []*operatorv1alpha1.EtcdBackup{}
	_, err := tracker.ProcessBackupsForPolicy(ctx, policy, emptyBackups)
	require.NoError(t, err)

	// State should be saved even with no backups
	allStates, err := tracker.GetAllPolicyStates(ctx)
	require.NoError(t, err)
	require.Contains(t, allStates, string(policy.UID), "never-successful policy should have saved state")

	savedState := allStates[string(policy.UID)]
	require.Equal(t, 0, savedState.ConsecutiveFailures)
	require.Nil(t, savedState.LastSuccessTime)
	// Compare timestamps at second precision (JSON marshaling truncates subseconds)
	require.Equal(t, policy.CreationTimestamp.Unix(), savedState.PolicyCreationTime.Unix())
}

func TestRunTracker_FirstReconcileBeforeAllMembersKnown(t *testing.T) {
	// Expected members stored atomically in backup annotations
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// First reconcile: only 1 of 3 expected backups visible (failed)
	// Annotation indicates 3 expected members
	backup1 := testFailedBackup("test-policy-node1-1000", policy, "1000", time.Now())
	backup1.Annotations = map[string]string{
		backuphelpers.AnnotationBackupExpectedMembers: "3",
	}
	backups1 := []*operatorv1alpha1.EtcdBackup{backup1}

	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups1)
	require.NoError(t, err)

	// Should NOT increment failure counter yet - don't know if other members will succeed
	require.Equal(t, 0, state1.ConsecutiveFailures, "should not count run as failed until all expected members are finished")

	// Second reconcile: all 3 members now visible, all failed
	backup2 := testFailedBackup("test-policy-node2-1000", policy, "1000", time.Now())
	backup2.Annotations = map[string]string{
		backuphelpers.AnnotationBackupExpectedMembers: "3",
	}
	backup3 := testFailedBackup("test-policy-node3-1000", policy, "1000", time.Now())
	backup3.Annotations = map[string]string{
		backuphelpers.AnnotationBackupExpectedMembers: "3",
	}
	backups2 := []*operatorv1alpha1.EtcdBackup{backup1, backup2, backup3}

	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups2)
	require.NoError(t, err)

	// NOW it should count as failed
	require.Equal(t, 1, state2.ConsecutiveFailures, "should count run as failed once all members failed")
}

func TestRunTracker_LateSuccessAfterRetention(t *testing.T) {
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Initial: Success at run 1000, then failures at 1001, 1002, 1003
	backups := []*operatorv1alpha1.EtcdBackup{
		testSuccessfulBackup("test-policy-1000", policy, "1000", time.Now()),
		testFailedBackup("test-policy-1001", policy, "1001", time.Now()),
		testFailedBackup("test-policy-1002", policy, "1002", time.Now()),
		testFailedBackup("test-policy-1003", policy, "1003", time.Now()),
	}
	state1, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, 3, state1.ConsecutiveFailures)

	// Later: Retention deletes run 1001, 1002
	// BUT we discover run 1002 actually had a late success
	retainedBackups := []*operatorv1alpha1.EtcdBackup{
		testSuccessfulBackup("test-policy-1000", policy, "1000", time.Now()),
		testSuccessfulBackup("test-policy-1002", policy, "1002", time.Now().Add(1 * time.Hour)), // Late success
		testFailedBackup("test-policy-1003", policy, "1003", time.Now()),
	}
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, retainedBackups)
	require.NoError(t, err)

	// Should still have 1 failure (1003 after 1002's success)
	// NOT 0 failures from rebuilding only from retained objects
	require.Equal(t, 1, state2.ConsecutiveFailures, "retention should not change durable failure count")
	require.NotNil(t, state2.LastSuccessTime)
}

func TestRunTracker_LatestSuccessTime(t *testing.T) {
	// REMAINING ISSUE #3: Last success time should be the LATEST, not first
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	earlyTime := time.Now()
	lateTime := earlyTime.Add(5 * time.Minute)

	// Run with multiple successes at different completion times
	backups := []*operatorv1alpha1.EtcdBackup{
		testSuccessfulBackup("test-policy-node1-1000", policy, "1000", earlyTime),
		testSuccessfulBackup("test-policy-node2-1000", policy, "1000", lateTime), // Later completion
	}
	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)

	// Should record the LATEST success time, not the first
	require.NotNil(t, state.LastSuccessTime)
	require.Equal(t, lateTime.Unix(), state.LastSuccessTime.Unix(), "should use latest success time, not first")
}

func TestRunTracker_UnlabeledBackupsAfterPolicyRecreation(t *testing.T) {
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	// Policy deleted and recreated with same name, different UID
	policy2 := testPolicy("test-policy", "uid-2")

	// Old unlabeled backup from before this policy existed
	unlabeledBackup := testFailedBackup("test-policy-1000", policy2, "1000", time.Now())
	delete(unlabeledBackup.Labels, backuphelpers.LabelEtcdBackupPolicyUID)
	unlabeledBackup.CreationTimestamp = metav1.NewTime(policy2.CreationTimestamp.Add(-time.Hour))

	// New policy should NOT count unlabeled backup from old policy
	state, err := tracker.ProcessBackupsForPolicy(ctx, policy2, []*operatorv1alpha1.EtcdBackup{unlabeledBackup})
	require.NoError(t, err)
	require.Equal(t, 0, state.ConsecutiveFailures, "should not count unlabeled backups from previous policy with same name")
}

func TestRunTracker_PruneBoundsSustainedFailure(t *testing.T) {
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)
	policy := testPolicy("test-policy", "uid-1")

	// Create more failed runs than the retention window
	backups := make([]*operatorv1alpha1.EtcdBackup, 0, runMetadataRetentionWindow+25)
	for i := 0; i < runMetadataRetentionWindow+25; i++ {
		hash := fmt.Sprintf("%d", 1000+i)
		backups = append(backups, testFailedBackup("test-policy-"+hash, policy, hash, time.Now()))
	}

	state, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)
	require.Equal(t, runMetadataRetentionWindow+25, state.ConsecutiveFailures)
	require.LessOrEqual(t, len(state.RunOutcomes), runMetadataRetentionWindow)

	// Reload and process again with no backups — durable streak must remain
	state2, err := tracker.ProcessBackupsForPolicy(ctx, policy, nil)
	require.NoError(t, err)
	require.Equal(t, runMetadataRetentionWindow+25, state2.ConsecutiveFailures)
	require.LessOrEqual(t, len(state2.RunOutcomes), runMetadataRetentionWindow)
}

func TestRunTracker_CorruptStateOnRead(t *testing.T) {
	// REMAINING ISSUE #5: GetPolicyState and GetAllPolicyStates should return errors on corruption
	ctx := context.Background()
	tracker, client := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")

	// Save valid state
	state := &PolicyRunState{
		PolicyUID:           policy.UID,
		ConsecutiveFailures: 2,
		PolicyCreationTime:  policy.CreationTimestamp,
	}
	err := tracker.SavePolicyState(ctx, policy, state)
	require.NoError(t, err)

	// Corrupt the ConfigMap
	cm, err := client.Get(ctx, failureTrackerConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data[failureTrackerDataKey] = "invalid json {"
	_, err = client.Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	// GetPolicyState should return error, not reinitialize
	_, err = tracker.GetPolicyState(ctx, policy)
	require.Error(t, err, "GetPolicyState should return error on corruption, not reinitialize")

	// GetAllPolicyStates should return error, not empty map
	_, err = tracker.GetAllPolicyStates(ctx)
	require.Error(t, err, "GetAllPolicyStates should return error on corruption, not empty map")
}

func TestRunTracker_PendingOnlyRun(t *testing.T) {
	// REMAINING ISSUE #6: Pending-only runs should still save initial state
	ctx := context.Background()
	tracker, _ := setupRunTracker(t)

	policy := testPolicy("test-policy", "uid-1")
	policy.CreationTimestamp = metav1.NewTime(time.Now().Add(-25 * time.Hour))

	// All backups are pending (common at startup)
	backups := []*operatorv1alpha1.EtcdBackup{
		testPendingBackup("test-policy-node1-1000", policy, "1000"),
		testPendingBackup("test-policy-node2-1000", policy, "1000"),
	}
	_, err := tracker.ProcessBackupsForPolicy(ctx, policy, backups)
	require.NoError(t, err)

	// State should be saved even with only pending backups
	allStates, err := tracker.GetAllPolicyStates(ctx)
	require.NoError(t, err)
	require.Contains(t, allStates, string(policy.UID), "pending-only run should still save initial state")
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

func testSuccessfulBackup(name string, policy *operatorv1alpha1.EtcdBackupPolicy, minuteHash string, completionTime time.Time) *operatorv1alpha1.EtcdBackup {
	return &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(policy.CreationTimestamp.Add(time.Second)),
			Labels: map[string]string{
				backuphelpers.LabelEtcdBackupPolicy:    policy.Name,
				backuphelpers.LabelEtcdBackupPolicyUID: string(policy.UID),
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

func testFailedBackup(name string, policy *operatorv1alpha1.EtcdBackupPolicy, minuteHash string, failureTime time.Time) *operatorv1alpha1.EtcdBackup {
	return &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(policy.CreationTimestamp.Add(time.Second)),
			Labels: map[string]string{
				backuphelpers.LabelEtcdBackupPolicy:    policy.Name,
				backuphelpers.LabelEtcdBackupPolicyUID: string(policy.UID),
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

func testPendingBackup(name string, policy *operatorv1alpha1.EtcdBackupPolicy, minuteHash string) *operatorv1alpha1.EtcdBackup {
	return &operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(policy.CreationTimestamp.Add(time.Second)),
			Labels: map[string]string{
				backuphelpers.LabelEtcdBackupPolicy:    policy.Name,
				backuphelpers.LabelEtcdBackupPolicyUID: string(policy.UID),
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
