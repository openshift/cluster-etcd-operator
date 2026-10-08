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
}

// RunTracker manages durable tracking of backup run failures per policy.
type RunTracker struct {
	namespace      string
	configMapClient corev1client.ConfigMapInterface
}

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
	if !ok {
		// ConfigMap exists but has no data
		return t.initializePolicyState(policy), nil
	}

	var allStates map[string]*PolicyRunState
	if err := json.Unmarshal([]byte(data), &allStates); err != nil {
		klog.Warningf("failed to unmarshal failure tracker data, reinitializing: %v", err)
		return t.initializePolicyState(policy), nil
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
	var allStates map[string]*PolicyRunState
	if data, ok := cm.Data[failureTrackerDataKey]; ok {
		if err := json.Unmarshal([]byte(data), &allStates); err != nil {
			klog.Warningf("failed to unmarshal failure tracker data, resetting: %v", err)
			allStates = make(map[string]*PolicyRunState)
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

	// Group backups by run (minute hash)
	runs := t.groupBackupsByRun(backups)
	if len(runs) == 0 {
		// No backups yet - return current state
		return state, nil
	}

	// Sort runs chronologically (by minute hash)
	sortedRuns := t.sortRuns(runs)

	// Process runs newer than last processed
	lastProcessedHash, _ := strconv.ParseInt(state.LastProcessedRunMinuteHash, 10, 64)
	modified := false

	for _, runHash := range sortedRuns {
		runHashInt, _ := strconv.ParseInt(runHash, 10, 64)
		if runHashInt <= lastProcessedHash {
			continue // Already counted this run
		}

		runBackups := runs[runHash]
		outcome := t.determineRunOutcome(runBackups)

		switch outcome {
		case runSucceeded:
			// Any backup succeeded - reset failure counter
			state.ConsecutiveFailures = 0
			state.LastSuccessTime = t.getRunCompletionTime(runBackups)
			state.LastProcessedRunMinuteHash = runHash
			modified = true
			klog.V(4).Infof("BackupRunTracker: policy %s run %s succeeded, reset failure counter", policy.Name, runHash)

		case runFailed:
			// All backups failed - increment failure counter
			state.ConsecutiveFailures++
			state.LastProcessedRunMinuteHash = runHash
			modified = true
			klog.Warningf("BackupRunTracker: policy %s run %s failed, consecutive failures: %d", policy.Name, runHash, state.ConsecutiveFailures)

		case runInProgress:
			// Don't process incomplete runs yet
			klog.V(4).Infof("BackupRunTracker: policy %s run %s still in progress, skipping", policy.Name, runHash)
			// Don't update lastProcessedRunMinuteHash - will reprocess next sync
		}
	}

	if modified {
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
		klog.Warningf("failed to unmarshal failure tracker data: %v", err)
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
// Failure: ALL backups in the run failed
// In Progress: At least one backup not finished
func (t *RunTracker) determineRunOutcome(backups []*operatorv1alpha1.EtcdBackup) runOutcome {
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
	return runFailed
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

// getRunCompletionTime finds the completion time of the first successful backup in the run
func (t *RunTracker) getRunCompletionTime(backups []*operatorv1alpha1.EtcdBackup) *metav1.Time {
	for _, backup := range backups {
		if backuphelpers.IsBackupCompleted(backup) {
			for _, cond := range backup.Status.Conditions {
				if cond.Type == string(operatorv1alpha1.BackupCompleted) && cond.Status == metav1.ConditionTrue {
					return &cond.LastTransitionTime
				}
			}
		}
	}
	return nil
}
