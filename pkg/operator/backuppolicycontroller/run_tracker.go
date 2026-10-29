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
	// DERIVED from RunOutcomes, not stored independently
	ConsecutiveFailures int `json:"consecutiveFailures"`

	// LastSuccessTime is when the last successful run completed (any backup in run succeeded)
	// Nil if policy has never succeeded
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`

	// PolicyCreationTime tracks when we first saw this policy
	// Used for grace periods ("first-policy24h allowance")
	PolicyCreationTime metav1.Time `json:"policyCreationTime"`

	// RunOutcomes stores durable per-run outcomes to decouple accounting from retention
	// Key is minute hash, value is outcome ("success" or "failure")
	// Pruned to keep recent N runs only
	// This is the SOURCE OF TRUTH for consecutive failures
	RunOutcomes map[string]string `json:"runOutcomes,omitempty"`

	// DEPRECATED FIELDS (kept for backward compatibility)

	// LastProcessedRunMinuteHash is DEPRECATED - replaced by RunOutcomes
	LastProcessedRunMinuteHash string `json:"lastProcessedRunMinuteHash,omitempty"`

	// RunMetadata is DEPRECATED - expected members now stored in backup annotations
	RunMetadata map[string]*RunMetadata `json:"runMetadata,omitempty"`
}

// RunMetadata is DEPRECATED - kept for backward compatibility only
type RunMetadata struct {
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
	if !ok || data == "" {
		// ConfigMap exists but tracking payload is missing: treat as lost state,
		// not first initialization (NotFound above). Recovery is to delete the CM.
		return nil, fmt.Errorf("failure tracker ConfigMap missing %q data (lost tracking state)", failureTrackerDataKey)
	}

	var allStates map[string]*PolicyRunState
	if err := json.Unmarshal([]byte(data), &allStates); err != nil {
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

	// Load existing states. Missing/empty key or corrupt JSON: do not reset in place;
	// return an error so the operator surfaces tracker health instead of wiping history.
	var allStates map[string]*PolicyRunState
	if data, ok := cm.Data[failureTrackerDataKey]; ok && data != "" {
		if err := json.Unmarshal([]byte(data), &allStates); err != nil {
			return fmt.Errorf("failed to unmarshal failure tracker data (corrupted ConfigMap): %w", err)
		}
	} else if cmExists {
		return fmt.Errorf("failure tracker ConfigMap missing %q data (lost tracking state)", failureTrackerDataKey)
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

	// Filter backups to only those belonging to this policy UID
	// Also prevents attributing old unlabeled backups to recreated policy with same name
	policyBackups := t.filterBackupsByPolicyUID(backups, policy.UID, state.PolicyCreationTime)

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

	// Initialize RunOutcomes map if needed
	if state.RunOutcomes == nil {
		state.RunOutcomes = make(map[string]string)
	}

	// DURABLE OUTCOMES: Process runs and record outcomes durably
	// This decouples consecutive failure counting from backup retention
	modified := false
	latestSuccessTime := state.LastSuccessTime
	successReset := false
	// newlyFinalizedFailures counts runs whose outcome is recorded as failure for the
	// first time (or changed to failure) in this sync. That is the accounting boundary:
	// each finalized failure advances the durable streak exactly once.
	newlyFinalizedFailures := 0

	for _, runHash := range sortedRuns {
		runBackups := runs[runHash]

		// Get expected member count from backup annotations (stored atomically with backup creation)
		expectedMembers := t.getExpectedMembersFromBackups(runBackups)
		outcome := t.determineRunOutcome(runBackups, expectedMembers)

		// Only record finalized outcomes (skip in-progress)
		if outcome == runInProgress {
			klog.V(4).Infof("BackupRunTracker: policy %s run %s still in progress, skipping", policy.Name, runHash)
			continue
		}

		// Check if this run's outcome changed
		previousOutcome, exists := state.RunOutcomes[runHash]
		var currentOutcomeStr string
		if outcome == runSucceeded {
			currentOutcomeStr = "success"
			// Track latest success time
			successTime := t.getRunCompletionTime(runBackups)
			if successTime != nil && (latestSuccessTime == nil || successTime.After(latestSuccessTime.Time)) {
				latestSuccessTime = successTime
				modified = true
				klog.V(4).Infof("BackupRunTracker: policy %s run %s succeeded", policy.Name, runHash)
			}
		} else {
			currentOutcomeStr = "failure"
			klog.Warningf("BackupRunTracker: policy %s run %s failed", policy.Name, runHash)
		}

		// Record outcome if new or changed
		if !exists || previousOutcome != currentOutcomeStr {
			if currentOutcomeStr == "success" && previousOutcome != "success" {
				successReset = true
			}
			if currentOutcomeStr == "failure" && previousOutcome != "failure" {
				newlyFinalizedFailures++
			}
			state.RunOutcomes[runHash] = currentOutcomeStr
			modified = true
		}
	}

	// Update last success time if we found a newer one
	if latestSuccessTime != nil && (state.LastSuccessTime == nil || latestSuccessTime.After(state.LastSuccessTime.Time)) {
		state.LastSuccessTime = latestSuccessTime
		successReset = true
		modified = true
	}

	// ConsecutiveFailures is a durable summary independent of bounded RunOutcomes history.
	// On success, rebuild from the retained window. Otherwise advance exactly once per
	// newly finalized failure; never shrink when pruning truncates older failures.
	calculatedFailures := t.calculateConsecutiveFailures(state.RunOutcomes, state.LastSuccessTime)
	prevFailures := state.ConsecutiveFailures
	if successReset {
		state.ConsecutiveFailures = calculatedFailures
	} else {
		state.ConsecutiveFailures += newlyFinalizedFailures
		if calculatedFailures > state.ConsecutiveFailures {
			// Heal undersized summaries (e.g. upgrade) from the retained window.
			state.ConsecutiveFailures = calculatedFailures
		}
	}
	if state.ConsecutiveFailures != prevFailures {
		modified = true
	}

	// Hard-bound history: keep only the most recent N run outcomes.
	if len(state.RunOutcomes) > runMetadataRetentionWindow {
		t.pruneOldRunOutcomes(state)
		modified = true
	}

	// Always save state for new policies (even with no finished runs)
	// This ensures never-successful policies appear in metrics
	isNewPolicy := len(state.RunOutcomes) == 0 && state.LastSuccessTime == nil
	if modified || isNewPolicy {
		klog.V(4).Infof("BackupRunTracker: policy %s state update - failures: %d, lastSuccess: %v",
			policy.Name, state.ConsecutiveFailures, state.LastSuccessTime)
		if err := t.SavePolicyState(ctx, policy, state); err != nil {
			return nil, err
		}
	}

	return state, nil
}

// GetAllPolicyStates returns states for all policies via a live ConfigMap GET.
// Prefer parseAllPolicyStates / a ConfigMap lister for scrape-time metrics.
func (t *RunTracker) GetAllPolicyStates(ctx context.Context) (map[string]*PolicyRunState, error) {
	cm, err := t.configMapClient.Get(ctx, failureTrackerConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return make(map[string]*PolicyRunState), nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get failure tracker ConfigMap: %w", err)
	}
	return parseAllPolicyStates(cm)
}

// parseAllPolicyStates decodes tracker state from a ConfigMap.
// A nil ConfigMap means first initialization (empty map, no error).
// A present ConfigMap with a missing/empty data key is lost established state (error).
// Corrupt JSON returns an error.
func parseAllPolicyStates(cm *corev1.ConfigMap) (map[string]*PolicyRunState, error) {
	if cm == nil {
		return make(map[string]*PolicyRunState), nil
	}
	data, ok := cm.Data[failureTrackerDataKey]
	if !ok || data == "" {
		return nil, fmt.Errorf("failure tracker ConfigMap missing %q data (lost tracking state)", failureTrackerDataKey)
	}

	var allStates map[string]*PolicyRunState
	if err := json.Unmarshal([]byte(data), &allStates); err != nil {
		return nil, fmt.Errorf("failed to unmarshal failure tracker data (corrupted ConfigMap): %w", err)
	}
	if allStates == nil {
		return make(map[string]*PolicyRunState), nil
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
		if len(backups) < expectedMembers && !hasNewerRun {
			// Missing members - run is in progress and no newer run has been finalized
			return runInProgress
		}
		// All expected members are finished and all failed
		return runFailed
	}

	// No metadata - fall back to conservative: all visible finished and failed = failed
	// This maintains backward compatibility for runs without annotations
	return runFailed
}

// filterBackupsByPolicyUID filters backups to only include those created by this policy UID.
// This prevents counting backups from a deleted policy with the same name.
func (t *RunTracker) filterBackupsByPolicyUID(backups []*operatorv1alpha1.EtcdBackup, policyUID types.UID, policyCreationTime metav1.Time) []*operatorv1alpha1.EtcdBackup {
	filtered := make([]*operatorv1alpha1.EtcdBackup, 0, len(backups))
	for _, backup := range backups {
		// Check if backup has policy UID label matching this policy
		if backupPolicyUID, ok := backup.Labels[backuphelpers.LabelEtcdBackupPolicyUID]; ok {
			if backupPolicyUID == string(policyUID) {
				filtered = append(filtered, backup)
			}
			// Skip backups with different UID (from deleted policy with same name)
		} else {
			// Backups without UID label are from before UID tracking was added
			// Only include them if they were created AFTER this policy was created
			// This prevents attributing old backups to a recreated policy with the same name
			if backup.CreationTimestamp.After(policyCreationTime.Time) || backup.CreationTimestamp.Equal(&policyCreationTime) {
				filtered = append(filtered, backup)
			} else {
				// Old backup from before this policy existed - likely from deleted policy with same name
				klog.V(4).Infof("BackupRunTracker: skipping unlabeled backup %s (created %v before policy creation %v)",
					backup.Name, backup.CreationTimestamp.Time, policyCreationTime.Time)
			}
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

// getExpectedMembersFromBackups extracts the expected member count from backup annotations.
// All backups in a run should have the same annotation (set atomically at creation time).
// Returns 0 if annotation is missing (pre-annotation backups) or invalid.
func (t *RunTracker) getExpectedMembersFromBackups(backups []*operatorv1alpha1.EtcdBackup) int {
	for _, backup := range backups {
		if backup.Annotations == nil {
			continue
		}
		if expectedStr, ok := backup.Annotations[backuphelpers.AnnotationBackupExpectedMembers]; ok {
			if expected, err := strconv.Atoi(expectedStr); err == nil && expected > 0 {
				return expected
			}
		}
	}
	return 0
}

// calculateConsecutiveFailures counts failures since the last success from durable outcomes.
// This is the ONLY source of truth for consecutive failures, decoupling from backup retention.
func (t *RunTracker) calculateConsecutiveFailures(outcomes map[string]string, lastSuccessTime *metav1.Time) int {
	if len(outcomes) == 0 {
		return 0
	}

	// Find the latest success hash (if any)
	latestSuccessHash := int64(0)
	if lastSuccessTime != nil {
		for hash, outcome := range outcomes {
			if outcome == "success" {
				hashInt, _ := strconv.ParseInt(hash, 10, 64)
				if hashInt > latestSuccessHash {
					latestSuccessHash = hashInt
				}
			}
		}
	}

	// Count failures after latest success
	consecutiveFailures := 0
	for hash, outcome := range outcomes {
		if outcome == "failure" {
			hashInt, _ := strconv.ParseInt(hash, 10, 64)
			if hashInt > latestSuccessHash {
				consecutiveFailures++
			}
		}
	}

	return consecutiveFailures
}

// pruneOldRunOutcomes removes old run outcomes to prevent unbounded growth.
// Keeps only the most recent runMetadataRetentionWindow runs. Durable summary
// fields (LastSuccessTime, ConsecutiveFailures) are preserved separately.
func (t *RunTracker) pruneOldRunOutcomes(state *PolicyRunState) {
	if len(state.RunOutcomes) <= runMetadataRetentionWindow {
		return
	}

	allHashes := make([]string, 0, len(state.RunOutcomes))
	for hash := range state.RunOutcomes {
		allHashes = append(allHashes, hash)
	}
	sort.Slice(allHashes, func(i, j int) bool {
		hi, _ := strconv.ParseInt(allHashes[i], 10, 64)
		hj, _ := strconv.ParseInt(allHashes[j], 10, 64)
		return hi < hj
	})

	cutoffIndex := len(allHashes) - runMetadataRetentionWindow
	for i := 0; i < cutoffIndex; i++ {
		delete(state.RunOutcomes, allHashes[i])
	}
}
