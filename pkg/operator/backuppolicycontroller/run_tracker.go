package backuppolicycontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

const (
	failureTrackerConfigMapName = "backup-failure-tracker"
	failureTrackerDataKey       = "tracker-data"
)

// PolicyRunState tracks the consecutive failure state for a single policy.
// Persisted in ConfigMap for durability across restarts and retention.
type PolicyRunState struct {
	// PolicyUID identifies the policy (survives name changes and recreation)
	PolicyUID types.UID `json:"policyUID"`

	// ConsecutiveFailures counts failed runs since last success
	ConsecutiveFailures int `json:"consecutiveFailures"`

	// LastSuccessTime is when the last successful run completed (any backup in run succeeded)
	// Nil if policy has never succeeded
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`

	// LastProcessedRunMinuteHash is the minute hash of the last run we counted
	// Prevents double-counting on duplicate reconciles
	LastProcessedRunMinuteHash string `json:"lastProcessedRunMinuteHash,omitempty"`

	// PolicyCreationTime tracks when we first saw this policy
	// Used for grace periods ("first-policy24h allowance")
	PolicyCreationTime metav1.Time `json:"policyCreationTime"`

	// RunMetadata tracks expected member count per run for proper finalization
	// Key is minute hash, pruned for runs older than retention window
	RunMetadata map[string]*RunMetadata `json:"runMetadata,omitempty"`
}

// RunMetadata stores metadata about a scheduled backup run
type RunMetadata struct {
	// ExpectedMembers is the number of backup objects expected for this run
	// Used to distinguish "partial run" from "complete run with all failures"
	ExpectedMembers int `json:"expectedMembers"`
}

// RunTracker manages durable tracking of backup run failures per policy.
type RunTracker struct {
	namespace      string
	configMapClient corev1client.ConfigMapInterface
}

const (
	// runMetadataRetentionWindow is how many runs to keep metadata for
	// This allows late arrivals to be properly accounted while preventing unbounded growth
	runMetadataRetentionWindow = 100
)

// NewRunTracker creates a new RunTracker
func NewRunTracker(namespace string, configMapClient corev1client.ConfigMapInterface) *RunTracker {
	return &RunTracker{
		namespace:      namespace,
		configMapClient: configMapClient,
	}
}

// RegisterRun records metadata for a scheduled backup run.
// Called by the controller when it creates backup objects for a run.
func (t *RunTracker) RegisterRun(ctx context.Context, policy *operatorv1alpha1.EtcdBackupPolicy, minuteHash string, expectedMembers int) error {
	state, err := t.GetPolicyState(ctx, policy)
	if err != nil {
		return err
	}

	if state.RunMetadata == nil {
		state.RunMetadata = make(map[string]*RunMetadata)
	}

	state.RunMetadata[minuteHash] = &RunMetadata{
		ExpectedMembers: expectedMembers,
	}

	// Prune old metadata to prevent unbounded growth
	t.pruneOldRunMetadata(state)

	return t.SavePolicyState(ctx, policy, state)
}

// GetPolicyState loads the state for a given policy from the ConfigMap.
// Returns nil if no state exists (new policy).
func (t *RunTracker) GetPolicyState(ctx context.Context, policy *operatorv1alpha1.EtcdBackupPolicy) (*PolicyRunState, error) {
	cm, err := t.configMapClient.Get(ctx, failureTrackerConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// ConfigMap doesn't exist yet - this is the first policy
		return t.initializePolicyState(policy), nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get failure tracker ConfigMap: %w", err)
	}

	data, ok := cm.Data[failureTrackerDataKey]
	if !ok {
		// ConfigMap exists but has no data
		return t.initializePolicyState(policy), nil
	}

	var allStates map[string]*PolicyRunState
	if err := json.Unmarshal([]byte(data), &allStates); err != nil {
		// REMAINING ISSUE #5 FIX: Return error instead of reinitializing
		return nil, fmt.Errorf("failed to unmarshal failure tracker data (corrupted ConfigMap): %w", err)
	}

	state, ok := allStates[string(policy.UID)]
	if !ok {
		// New policy we haven't seen before
		return t.initializePolicyState(policy), nil
	}

	return state, nil
}

// SavePolicyState persists the state for a policy to the ConfigMap.
func (t *RunTracker) SavePolicyState(ctx context.Context, policy *operatorv1alpha1.EtcdBackupPolicy, state *PolicyRunState) error {
	// Ensure state has policy UID
	state.PolicyUID = policy.UID

	cm, err := t.configMapClient.Get(ctx, failureTrackerConfigMapName, metav1.GetOptions{})
	cmExists := true
	if apierrors.IsNotFound(err) {
		// Create ConfigMap
		cmExists = false
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      failureTrackerConfigMapName,
				Namespace: t.namespace,
			},
			Data: make(map[string]string),
		}
	} else if err != nil {
		return fmt.Errorf("failed to get failure tracker ConfigMap: %w", err)
	}

	// Load existing states
	// BLOCKER #4: Don't reset on corruption - preserve data and return error
	var allStates map[string]*PolicyRunState
	if data, ok := cm.Data[failureTrackerDataKey]; ok {
		if err := json.Unmarshal([]byte(data), &allStates); err != nil {
			// Corruption detected - return error instead of resetting
			return fmt.Errorf("failed to unmarshal failure tracker data (corrupted ConfigMap): %w", err)
		}
	} else {
		allStates = make(map[string]*PolicyRunState)
	}

	// Update this policy's state
	allStates[string(policy.UID)] = state

	// Serialize back
	dataBytes, err := json.Marshal(allStates)
	if err != nil {
		return fmt.Errorf("failed to marshal failure tracker data: %w", err)
	}
	cm.Data[failureTrackerDataKey] = string(dataBytes)

	// Save ConfigMap
	if !cmExists {
		cm, err = t.configMapClient.Create(ctx, cm, metav1.CreateOptions{})
	} else {
		cm, err = t.configMapClient.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to save failure tracker ConfigMap: %w", err)
	}

	return nil
}

// ProcessBackupsForPolicy examines finished backups for a policy and updates failure tracking.
// Groups backups by run (minute hash), processes new runs idempotently.
func (t *RunTracker) ProcessBackupsForPolicy(
	ctx context.Context,
	policy *operatorv1alpha1.EtcdBackupPolicy,
	backups []*operatorv1alpha1.EtcdBackup,
) (*PolicyRunState, error) {
	state, err := t.GetPolicyState(ctx, policy)
	if err != nil {
		return nil, err
	}

	// BLOCKER #5: Filter backups to only those belonging to this policy UID
	policyBackups := t.filterBackupsByPolicyUID(backups, policy.UID)

	// Group backups by run (minute hash)
	runs := t.groupBackupsByRun(policyBackups)

	// BLOCKER #3: Always save state for new policies, even with no backups
	// This ensures never-successful policies have metrics for alerts
	if len(runs) == 0 {
		if state.LastProcessedRunMinuteHash == "" {
			// New policy with no backups yet - save initial state
			if err := t.SavePolicyState(ctx, policy, state); err != nil {
				return nil, err
			}
		}
		return state, nil
	}

	// Sort runs chronologically (by minute hash)
	sortedRuns := t.sortRuns(runs)

	// BLOCKER #1 & #2: Process all runs to find latest success, but only increment failures once
	// Track which runs we've already counted as failures
	lastProcessedHash, _ := strconv.ParseInt(state.LastProcessedRunMinuteHash, 10, 64)
	modified := false
	latestSuccessTime := state.LastSuccessTime
	latestSuccessHash := int64(0)
	consecutiveFailuresSinceSuccess := 0

	for _, runHash := range sortedRuns {
		runHashInt, _ := strconv.ParseInt(runHash, 10, 64)
		runBackups := runs[runHash]

		// Get expected member count from metadata
		expectedMembers := 0
		if state.RunMetadata != nil {
			if metadata, ok := state.RunMetadata[runHash]; ok {
				expectedMembers = metadata.ExpectedMembers
			}
		}

		outcome := t.determineRunOutcome(runBackups, expectedMembers)

		switch outcome {
		case runSucceeded:
			// Track latest success (may come in late)
			successTime := t.getRunCompletionTime(runBackups)
			if successTime != nil && (latestSuccessTime == nil || successTime.After(latestSuccessTime.Time)) {
				latestSuccessTime = successTime
				latestSuccessHash = runHashInt
				modified = true
				klog.V(4).Infof("BackupRunTracker: policy %s run %s succeeded", policy.Name, runHash)
			}

		case runFailed:
			// Only count this failure if we haven't processed it yet
			if runHashInt > lastProcessedHash {
				consecutiveFailuresSinceSuccess++
				klog.Warningf("BackupRunTracker: policy %s run %s failed", policy.Name, runHash)
			}

		case runInProgress:
			// Don't process incomplete runs
			klog.V(4).Infof("BackupRunTracker: policy %s run %s still in progress, skipping", policy.Name, runHash)
		}
	}

	// Update state: failures since last success
	if latestSuccessTime != nil && (state.LastSuccessTime == nil || latestSuccessTime.After(state.LastSuccessTime.Time)) {
		// New success found - reset counter and update success time
		state.LastSuccessTime = latestSuccessTime
		state.ConsecutiveFailures = 0
		modified = true

		// Count failures after this success
		for _, runHash := range sortedRuns {
			runHashInt, _ := strconv.ParseInt(runHash, 10, 64)
			if runHashInt > latestSuccessHash {
				runBackups := runs[runHash]
				expectedMembers := 0
				if state.RunMetadata != nil {
					if metadata, ok := state.RunMetadata[runHash]; ok {
						expectedMembers = metadata.ExpectedMembers
					}
				}
				if t.determineRunOutcome(runBackups, expectedMembers) == runFailed {
					state.ConsecutiveFailures++
				}
			}
		}
	} else if consecutiveFailuresSinceSuccess > 0 {
		// New failures since last processed, no new success
		state.ConsecutiveFailures += consecutiveFailuresSinceSuccess
		modified = true
	}

	// Update cursor to latest processed run (failed or succeeded, not in-progress)
	latestProcessed := ""
	for i := len(sortedRuns) - 1; i >= 0; i-- {
		runHash := sortedRuns[i]
		expectedMembers := 0
		if state.RunMetadata != nil {
			if metadata, ok := state.RunMetadata[runHash]; ok {
				expectedMembers = metadata.ExpectedMembers
			}
		}
		outcome := t.determineRunOutcome(runs[runHash], expectedMembers)
		if outcome != runInProgress {
			latestProcessed = runHash
			break
		}
	}
	if latestProcessed != "" && latestProcessed != state.LastProcessedRunMinuteHash {
		state.LastProcessedRunMinuteHash = latestProcessed
		modified = true
	}

	// REMAINING ISSUE #6 FIX: Save initial state for new policies even if all runs are pending
	isNewPolicy := state.LastProcessedRunMinuteHash == ""
	if modified || isNewPolicy {
		klog.V(4).Infof("BackupRunTracker: policy %s state update - failures: %d, lastSuccess: %v",
			policy.Name, state.ConsecutiveFailures, state.LastSuccessTime)
		if err := t.SavePolicyState(ctx, policy, state); err != nil {
			return nil, err
		}
	}

	return state, nil
}

// GetAllPolicyStates returns states for all policies (for metrics emission)
func (t *RunTracker) GetAllPolicyStates(ctx context.Context) (map[string]*PolicyRunState, error) {
	cm, err := t.configMapClient.Get(ctx, failureTrackerConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return make(map[string]*PolicyRunState), nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get failure tracker ConfigMap: %w", err)
	}

	data, ok := cm.Data[failureTrackerDataKey]
	if !ok {
		return make(map[string]*PolicyRunState), nil
	}

	var allStates map[string]*PolicyRunState
	if err := json.Unmarshal([]byte(data), &allStates); err != nil {
		// REMAINING ISSUE #5 FIX: Return error instead of empty map
		return nil, fmt.Errorf("failed to unmarshal failure tracker data (corrupted ConfigMap): %w", err)
	}

	return allStates, nil
}

// Helper functions

func (t *RunTracker) initializePolicyState(policy *operatorv1alpha1.EtcdBackupPolicy) *PolicyRunState {
	return &PolicyRunState{
		PolicyUID:           policy.UID,
		ConsecutiveFailures: 0,
		LastSuccessTime:     nil,
		PolicyCreationTime:  policy.CreationTimestamp,
	}
}

type runOutcome int

const (
	runInProgress runOutcome = iota
	runSucceeded
	runFailed
)

// determineRunOutcome determines if a run succeeded, failed, or is still in progress.
// Success: ANY backup in the run completed successfully
// Failure: ALL EXPECTED backups in the run failed
// In Progress: Not all expected members are finished
func (t *RunTracker) determineRunOutcome(backups []*operatorv1alpha1.EtcdBackup, expectedMembers int) runOutcome {
	allFinished := true
	anySucceeded := false

	for _, backup := range backups {
		if !backuphelpers.IsBackupFinished(backup) {
			allFinished = false
			continue
		}

		if backuphelpers.IsBackupCompleted(backup) {
			anySucceeded = true
		}
	}

	if anySucceeded {
		return runSucceeded
	}

	if !allFinished {
		return runInProgress
	}

	// All visible members are finished and failed
	// If we have expected member count, verify we have all members
	if expectedMembers > 0 {
		if len(backups) < expectedMembers {
			// Missing members - run is in progress
			return runInProgress
		}
		// All expected members are finished and all failed
		return runFailed
	}

	// No metadata - fall back to conservative: all visible finished and failed = failed
	// This maintains backward compatibility for runs without RegisterRun
	return runFailed
}

// pruneOldRunMetadata removes metadata for runs older than the retention window
func (t *RunTracker) pruneOldRunMetadata(state *PolicyRunState) {
	if len(state.RunMetadata) <= runMetadataRetentionWindow {
		return
	}

	// Get all run hashes and sort them
	hashes := make([]string, 0, len(state.RunMetadata))
	for hash := range state.RunMetadata {
		hashes = append(hashes, hash)
	}

	sort.Slice(hashes, func(i, j int) bool {
		hi, _ := strconv.ParseInt(hashes[i], 10, 64)
		hj, _ := strconv.ParseInt(hashes[j], 10, 64)
		return hi < hj
	})

	// Keep only the most recent runMetadataRetentionWindow runs
	toDelete := len(hashes) - runMetadataRetentionWindow
	for i := 0; i < toDelete; i++ {
		delete(state.RunMetadata, hashes[i])
	}
}

// filterBackupsByPolicyUID filters backups to only include those created by this policy UID.
// This prevents counting backups from a deleted policy with the same name.
func (t *RunTracker) filterBackupsByPolicyUID(backups []*operatorv1alpha1.EtcdBackup, policyUID types.UID) []*operatorv1alpha1.EtcdBackup {
	filtered := make([]*operatorv1alpha1.EtcdBackup, 0, len(backups))
	for _, backup := range backups {
		// Check if backup has policy UID label matching this policy
		if backupPolicyUID, ok := backup.Labels[backuphelpers.LabelEtcdBackupPolicyUID]; ok {
			if backupPolicyUID == string(policyUID) {
				filtered = append(filtered, backup)
			}
		} else {
			// Backups without UID label are from before UID tracking was added
			// Include them for backward compatibility (old backups)
			// This is safe because policy names are unique at any point in time
			filtered = append(filtered, backup)
		}
	}
	return filtered
}

// groupBackupsByRun groups backups by their minute hash (scheduled run identifier)
func (t *RunTracker) groupBackupsByRun(backups []*operatorv1alpha1.EtcdBackup) map[string][]*operatorv1alpha1.EtcdBackup {
	runs := make(map[string][]*operatorv1alpha1.EtcdBackup)

	for _, backup := range backups {
		minuteHash := extractMinuteHashFromName(backup.Name)
		if minuteHash == "" {
			// Shouldn't happen for policy-created backups, but skip if malformed
			klog.Warningf("BackupRunTracker: could not extract minute hash from backup name: %s", backup.Name)
			continue
		}
		runs[minuteHash] = append(runs[minuteHash], backup)
	}

	return runs
}

// sortRuns returns run minute hashes sorted chronologically (oldest first)
func (t *RunTracker) sortRuns(runs map[string][]*operatorv1alpha1.EtcdBackup) []string {
	hashes := make([]string, 0, len(runs))
	for hash := range runs {
		hashes = append(hashes, hash)
	}

	sort.Slice(hashes, func(i, j int) bool {
		// Parse as int for numerical comparison
		hi, _ := strconv.ParseInt(hashes[i], 10, 64)
		hj, _ := strconv.ParseInt(hashes[j], 10, 64)
		return hi < hj
	})

	return hashes
}

// extractMinuteHashFromName extracts the minute hash from a backup name.
// Format: {policy}-{minuteHash} or {policy}-{nodeUID}-{minuteHash}
// Returns empty string if name is malformed.
func extractMinuteHashFromName(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return ""
	}
	// Last part is always the minute hash
	lastPart := parts[len(parts)-1]
	// Validate it's numeric
	if _, err := strconv.ParseInt(lastPart, 10, 64); err != nil {
		return ""
	}
	return lastPart
}

// getRunCompletionTime finds the LATEST completion time among successful backups in the run
// REMAINING ISSUE #3 FIX: Returns maximum timestamp, not first
func (t *RunTracker) getRunCompletionTime(backups []*operatorv1alpha1.EtcdBackup) *metav1.Time {
	var latestTime *metav1.Time

	for _, backup := range backups {
		if backuphelpers.IsBackupCompleted(backup) {
			for _, cond := range backup.Status.Conditions {
				if cond.Type == string(operatorv1alpha1.BackupCompleted) && cond.Status == metav1.ConditionTrue {
					if latestTime == nil || cond.LastTransitionTime.After(latestTime.Time) {
						latestTime = &cond.LastTransitionTime
					}
				}
			}
		}
	}
	return latestTime
}
