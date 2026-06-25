package operator

import (
	"context"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

const (
	// Degraded condition reason
	reasonPacemakerUnhealthy = "PacemakerUnhealthy"

	// Warning message prefixes for categorizing events.
	warningPrefixFencingEvent = "Recent fencing event:"

	// Operator condition types
	conditionTypePacemakerDegraded = "PacemakerHealthCheckDegraded"

	// Event key prefixes
	eventKeyPrefixWarning = "warning:"
	eventKeyPrefixError   = "error:"

	// Event message templates
	msgPacemakerDegraded        = "Pacemaker cluster is in degraded state"
	msgPacemakerHealthy         = "Pacemaker cluster is healthy - all nodes have healthy critical resources"
	msgPacemakerWarningsCleared = "Pacemaker cluster warnings cleared"
	msgDetectedFencing          = "Pacemaker detected a recent fencing event: %s"
	msgPacemakerWarning         = "Pacemaker warning: %s"
	msgPacemakerError           = "Pacemaker error: %s"
)

// MonitorHealth retrieves pacemaker health status, updates operator conditions, and records events.
// Returns nil if no status change or monitoring completed successfully.
func (c *PacemakerLifecycleManager) MonitorHealth(ctx context.Context) error {
	// Get current pacemaker status from CR
	currentStatus, previousStatus, err := c.getPacemakerStatus(ctx)
	if err != nil {
		// Note: getPacemakerStatus currently never returns error (always returns Unknown status instead)
		// This is defensive coding in case that changes
		return fmt.Errorf("failed to get pacemaker status: %w", err)
	}

	if currentStatus == nil {
		// nil status means CR hasn't changed since last sync - skip processing to avoid redundant work
		klog.V(4).Infof("Pacemaker status unchanged since last sync")
		return nil
	}

	klog.V(2).Infof("Pacemaker health status: %s (errors: %d, warnings: %d)",
		currentStatus.OverallStatus, len(currentStatus.Errors), len(currentStatus.Warnings))

	// Update operator condition based on health status
	var statusErr error
	if err := c.updateOperatorStatus(ctx, currentStatus, previousStatus); err != nil {
		// Don't block event recording on status update failure
		klog.Warningf("Failed to update operator status (continuing with event recording): %v", err)
		statusErr = err // Capture for return
	}

	// Record health check events (with deduplication) - always run even if status update failed
	c.recordHealthCheckEvents(currentStatus, previousStatus)

	return statusErr // Return error for aggregation so sync loop can observe failures
}

// getPacemakerStatus retrieves the current pacemaker health status from the PacemakerCluster CR.
// Returns (current, previous, error).
// current is nil if CR timestamp hasn't changed (skip redundant processing).
// previous is the last valid (non-Unknown) status, used for grace period calculation.
// For Unknown status, previous is not updated (preserves last valid status for grace period).
func (c *PacemakerLifecycleManager) getPacemakerStatus(ctx context.Context) (*pacemaker.HealthStatus, *pacemaker.HealthStatus, error) {
	klog.V(4).Infof("Retrieving pacemaker status from CR...")

	// Read previous status
	c.previousMu.Lock()
	previous := c.previous
	c.previousMu.Unlock()

	// Get the PacemakerCluster CR from the informer cache
	item, exists, err := c.pacemakerInformer.GetStore().GetByKey(pacemaker.PacemakerClusterResourceName)
	if err != nil {
		// Unknown status - don't update previous (preserves last valid for grace period)
		return &pacemaker.HealthStatus{
			OverallStatus: pacemaker.StatusUnknown,
			Warnings:      []string{},
			Errors:        []string{fmt.Sprintf("Failed to get PacemakerCluster CR from cache: %v", err)},
		}, previous, nil
	}
	if !exists {
		// Unknown status - CR not found in cache
		return &pacemaker.HealthStatus{
			OverallStatus: pacemaker.StatusUnknown,
			Warnings:      []string{},
			Errors:        []string{"PacemakerCluster CR not found in cache"},
		}, previous, nil
	}

	pacemakerCR, ok := item.(*pacmkrv1.PacemakerCluster)
	if !ok {
		return &pacemaker.HealthStatus{
			OverallStatus: pacemaker.StatusUnknown,
			Warnings:      []string{},
			Errors:        []string{"Failed to convert cached item to PacemakerCluster"},
		}, previous, nil
	}

	// Check if status is populated (LastUpdated is zero means status was never set)
	if pacemakerCR.Status.LastUpdated.IsZero() {
		// Unknown status - don't update previous
		return &pacemaker.HealthStatus{
			OverallStatus: pacemaker.StatusUnknown,
			Warnings:      []string{},
			Errors:        []string{"PacemakerCluster CR has no status populated"},
		}, previous, nil
	}

	crLastUpdated := pacemakerCR.Status.LastUpdated.Time

	// Check staleness first - any update (even with errors) clears staleness.
	timeSinceUpdate := time.Since(crLastUpdated)
	if timeSinceUpdate > pacemaker.StatusStalenessThreshold {
		// Unknown status - don't update previous
		// Use absolute timestamp (stable) for event deduplication.
		return &pacemaker.HealthStatus{
			OverallStatus: pacemaker.StatusUnknown,
			Warnings:      []string{},
			Errors:        []string{fmt.Sprintf("Pacemaker status is stale (last updated: %s)", crLastUpdated.Format(time.RFC3339))},
		}, previous, nil
	}

	// Check if lastUpdated timestamp has changed since last sync.
	// If unchanged, skip processing to avoid redundant work on timer-triggered syncs.
	// Use previous.CRLastUpdated for comparison (only set for non-Unknown status).
	if previous != nil && !previous.CRLastUpdated.IsZero() && crLastUpdated.Equal(previous.CRLastUpdated) {
		klog.V(4).Infof("Skipping sync: lastUpdated timestamp unchanged (%v)", crLastUpdated)
		return nil, nil, nil
	}

	// Build health status from the CRD status fields
	currentStatus := pacemaker.BuildHealthStatusFromCR(pacemakerCR)
	currentStatus.CRLastUpdated = crLastUpdated

	// Only update previous for non-Unknown status (preserves last valid for grace period)
	// Concurrency safety: check timestamp when updating to prevent stale writes from overwriting fresh data.
	// Two concurrent syncs can read c.previous, process different CR versions, then both try to update.
	// We only update if currentStatus is fresher than what's already in c.previous.
	if currentStatus.OverallStatus != pacemaker.StatusUnknown {
		c.previousMu.Lock()
		// Only update if we have fresher data (or c.previous is nil/uninitialized)
		if c.previous == nil || c.previous.CRLastUpdated.IsZero() || currentStatus.CRLastUpdated.After(c.previous.CRLastUpdated) {
			c.previous = currentStatus
		}
		c.previousMu.Unlock()
	}

	return currentStatus, previous, nil
}

// updateOperatorStatus processes the pacemaker.HealthStatus and conditionally updates the operator state.
// previous is the last non-Unknown health status (used for grace period calculation).
func (c *PacemakerLifecycleManager) updateOperatorStatus(ctx context.Context, status *pacemaker.HealthStatus, previous *pacemaker.HealthStatus) error {
	klog.V(4).Infof("Updating operator availability based on pacemaker status: %s", status.OverallStatus)

	// Log warnings and errors for visibility
	for _, warning := range status.Warnings {
		klog.Warningf(msgPacemakerWarning, warning)
	}
	for _, err := range status.Errors {
		klog.Errorf(msgPacemakerError, err)
	}

	// Update operator conditions based on pacemaker status
	switch status.OverallStatus {
	case pacemaker.StatusError:
		return c.setPacemakerDegradedCondition(ctx, status)
	case pacemaker.StatusHealthy, pacemaker.StatusWarning:
		// Both healthy and warning states should clear degraded condition
		// Warnings are informational (e.g. recent fencing, node count mismatch) and don't indicate degradation
		if status.OverallStatus == pacemaker.StatusWarning {
			klog.V(2).Infof("Pacemaker health check has warnings but cluster is operational: %v", status.Warnings)
		}
		return c.clearPacemakerDegradedCondition(ctx, status)
	case pacemaker.StatusUnknown:
		// Unknown status means we cannot determine pacemaker health (CR not found, stale, no status, etc.)
		// Only mark as degraded if we haven't received a valid status in a while (grace period).
		// Use previous.CRLastUpdated which reflects when we last had valid cluster data.
		// If previous is nil (first sync), don't degrade immediately.
		if previous == nil || previous.CRLastUpdated.IsZero() {
			klog.V(2).Infof("Pacemaker health check cannot determine status (no previous valid status), not marking degraded yet: %v",
				status.Errors)
			return nil
		}

		timeSinceLastValid := time.Since(previous.CRLastUpdated)
		if timeSinceLastValid > pacemaker.StatusUnknownDegradedThreshold {
			klog.Warningf("Pacemaker health check cannot determine status for %v (threshold: %v), marking as degraded: %v",
				timeSinceLastValid, pacemaker.StatusUnknownDegradedThreshold, status.Errors)
			return c.setPacemakerDegradedCondition(ctx, status)
		}

		// Still within grace period, just log
		klog.V(2).Infof("Pacemaker health check cannot determine status for %v (threshold: %v), not marking degraded yet: %v",
			timeSinceLastValid, pacemaker.StatusUnknownDegradedThreshold, status.Errors)
		return nil
	default:
		// This should never happen, but log it if it does
		klog.Errorf("Unexpected pacemaker health status: %s (errors: %v, warnings: %v)",
			status.OverallStatus, status.Errors, status.Warnings)
		return nil
	}
}

// setPacemakerDegradedCondition sets the PacemakerHealthCheckDegraded condition to True.
func (c *PacemakerLifecycleManager) setPacemakerDegradedCondition(ctx context.Context, status *pacemaker.HealthStatus) error {
	message := strings.Join(status.Errors, "; ")
	if message == "" {
		message = msgPacemakerDegraded
	}

	// Check if the condition is already set with the same message
	currentCondition, err := c.getCurrentPacemakerCondition()
	if err != nil {
		return err
	}

	// Only update if the condition is not already set to True with the same message
	if currentCondition != nil &&
		currentCondition.Status == operatorv1.ConditionTrue &&
		currentCondition.Message == message {
		klog.V(4).Infof("Pacemaker degraded condition already set with same message, skipping update")
		return nil
	}

	condition := operatorv1.OperatorCondition{
		Type:    conditionTypePacemakerDegraded,
		Status:  operatorv1.ConditionTrue,
		Reason:  reasonPacemakerUnhealthy,
		Message: message,
	}

	return c.updateOperatorCondition(ctx, condition)
}

// clearPacemakerDegradedCondition sets the PacemakerHealthCheckDegraded condition to False.
func (c *PacemakerLifecycleManager) clearPacemakerDegradedCondition(ctx context.Context, status *pacemaker.HealthStatus) error {
	// Check the current condition state
	currentCondition, err := c.getCurrentPacemakerCondition()
	if err != nil {
		return err
	}

	// Skip if condition is already explicitly set to False (no change needed)
	if currentCondition != nil && currentCondition.Status == operatorv1.ConditionFalse {
		klog.V(4).Infof("Pacemaker degraded condition is already False, skipping update")
		return nil
	}

	// Set condition to False in two cases:
	// 1. Condition doesn't exist yet (first healthy check - make health status explicit)
	// 2. Condition is currently True (transitioning from degraded to healthy)
	condition := operatorv1.OperatorCondition{
		Type:   conditionTypePacemakerDegraded,
		Status: operatorv1.ConditionFalse,
	}

	err = c.updateOperatorCondition(ctx, condition)
	if err != nil {
		return err
	}

	if currentCondition == nil {
		klog.Infof("Pacemaker health check condition initialized: PacemakerHealthCheckDegraded=False")
	} else {
		klog.Infof("Pacemaker degraded condition cleared (transitioned from True to False)")
	}
	return nil
}

// getCurrentPacemakerCondition retrieves the current PacemakerHealthCheckDegraded condition
func (c *PacemakerLifecycleManager) getCurrentPacemakerCondition() (*operatorv1.OperatorCondition, error) {
	_, currentStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return nil, fmt.Errorf("failed to get current operator status: %w", err)
	}

	// Find the current pacemaker degraded condition
	for i := range currentStatus.Conditions {
		if currentStatus.Conditions[i].Type == conditionTypePacemakerDegraded {
			return &currentStatus.Conditions[i], nil
		}
	}

	return nil, nil
}

// updateOperatorCondition updates the operator condition
func (c *PacemakerLifecycleManager) updateOperatorCondition(ctx context.Context, condition operatorv1.OperatorCondition) error {
	_, _, err := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition))
	if err != nil {
		klog.Errorf("Failed to update operator status: %v", err)
		return err
	}

	klog.V(2).Infof("Updated operator condition: %s=%s", condition.Type, condition.Status)
	return nil
}

// cleanupExpiredEvents removes events from the deduplication map that have exceeded their window.
// Fencing events are kept for 24 hours, other events for 5 minutes.
func (c *PacemakerLifecycleManager) cleanupExpiredEvents() {
	c.recordedEventsMu.Lock()
	defer c.recordedEventsMu.Unlock()

	now := time.Now()

	for key, timestamp := range c.recordedEvents {
		// Determine the appropriate window based on event type
		// Fencing events have longer window
		window := pacemaker.EventDeduplicationWindowDefault
		if strings.Contains(key, warningPrefixFencingEvent) {
			window = pacemaker.EventDeduplicationWindowFencing
		}

		// Remove if expired
		if now.Sub(timestamp) > window {
			delete(c.recordedEvents, key)
		}
	}
}

// shouldRecordEvent checks if an event should be recorded based on deduplication logic.
// This should be called after cleanupExpiredEvents() has been called.
func (c *PacemakerLifecycleManager) shouldRecordEvent(eventKey string, deduplicationWindow time.Duration) bool {
	c.recordedEventsMu.Lock()
	defer c.recordedEventsMu.Unlock()

	now := time.Now()

	// Check if this event was recently recorded
	if lastRecorded, exists := c.recordedEvents[eventKey]; exists {
		if now.Sub(lastRecorded) < deduplicationWindow {
			return false
		}
	}

	// Record this event
	c.recordedEvents[eventKey] = now
	return true
}

// recordHealthCheckEvents records events for warnings, errors, and health transitions.
// current is the newly computed health status, previous is the last health status (may be nil on first sync).
func (c *PacemakerLifecycleManager) recordHealthCheckEvents(current *pacemaker.HealthStatus, previous *pacemaker.HealthStatus) {
	// Clean up expired events before processing new ones
	c.cleanupExpiredEvents()

	// Record events for warnings with appropriate deduplication window
	for _, warning := range current.Warnings {
		eventKey := fmt.Sprintf(eventKeyPrefixWarning+"%s", warning)
		// Use longer deduplication window for fencing events
		deduplicationWindow := pacemaker.EventDeduplicationWindowDefault
		if strings.Contains(warning, warningPrefixFencingEvent) {
			deduplicationWindow = pacemaker.EventDeduplicationWindowFencing
		}
		if c.shouldRecordEvent(eventKey, deduplicationWindow) {
			c.recordWarningEvent(warning)
		}
	}

	// Record events for errors with default deduplication window
	for _, err := range current.Errors {
		eventKey := fmt.Sprintf(eventKeyPrefixError+"%s", err)
		if c.shouldRecordEvent(eventKey, pacemaker.EventDeduplicationWindowDefault) {
			c.recordErrorEvent(err)
		}
	}

	// Track and detect health transitions
	c.recordHealthTransitionEvents(current, previous)
}

// recordHealthTransitionEvents detects and records health state transitions.
// - PacemakerHealthy: fires on transition to operationally healthy from degraded OR unknown state
// - PacemakerWarningsCleared: fires when warnings are resolved, regardless of overall health
func (c *PacemakerLifecycleManager) recordHealthTransitionEvents(current *pacemaker.HealthStatus, previous *pacemaker.HealthStatus) {
	// Determine previous state from the previous pacemaker.HealthStatus we computed.
	// This is derived from the previous PacemakerCluster CR we processed.
	// Note: previous.OverallStatus is never StatusUnknown (we only update previous for non-Unknown status).
	// Instead, detect Unknown state by checking if previous is nil or if CRLastUpdated is too old (same threshold as degraded status).
	// If CRLastUpdated is zero, treat as valid (test scenarios or in-memory status).
	previousWasUnknown := previous == nil || (!previous.CRLastUpdated.IsZero() && time.Since(previous.CRLastUpdated) > pacemaker.StatusUnknownDegradedThreshold)
	previousWasDegraded := previous != nil && previous.OverallStatus == pacemaker.StatusError
	previousHadWarnings := previous != nil && len(previous.Warnings) > 0

	// Determine current state
	operationallyHealthy := current.OverallStatus == pacemaker.StatusHealthy || current.OverallStatus == pacemaker.StatusWarning
	currentHasNoWarnings := len(current.Warnings) == 0

	// Record PacemakerHealthy when transitioning to operationally healthy from:
	// 1. Error state (previousWasDegraded) - recovering from known problems
	// 2. Unknown state (previousWasUnknown) - initial healthy status or recovering from stale/missing CR
	// Both Healthy and Warning are "operationally healthy" (cluster is functional).
	if operationallyHealthy && (previousWasDegraded || previousWasUnknown) {
		c.eventRecorder.Eventf(pacemaker.EventReasonHealthy, msgPacemakerHealthy)
		klog.Infof("Pacemaker cluster transitioned to healthy (status: %s, fromDegraded: %v, fromUnknown: %v)",
			current.OverallStatus, previousWasDegraded, previousWasUnknown)
	}

	// Record PacemakerWarningsCleared when warnings are resolved.
	// This fires whenever the previous status had warnings and the current one doesn't,
	// regardless of overall cluster health. Warnings are independent of error state.
	// Examples:
	// - Warning → Healthy: warnings resolved, cluster fully healthy
	// - Error+Warning → Error: warnings resolved, but cluster still degraded for other reasons
	// - Error+Warning → Healthy: warnings resolved AND cluster recovered (fires both events)
	if previousHadWarnings && currentHasNoWarnings {
		c.eventRecorder.Eventf(pacemaker.EventReasonWarningsCleared, msgPacemakerWarningsCleared)
		klog.Infof("Pacemaker cluster warnings cleared")
	}
}

// recordWarningEvent records appropriate warning events based on warning type.
// Note: Failed action events (warningPrefixFailedAction) are recorded by the status collector
// directly from pacemaker XML, not by the healthcheck controller. The PacemakerCluster CR
// only contains conditions, not raw failed action history.
func (c *PacemakerLifecycleManager) recordWarningEvent(warning string) {
	switch {
	case strings.Contains(warning, warningPrefixFencingEvent):
		c.eventRecorder.Warningf(pacemaker.EventReasonFencingEvent, msgDetectedFencing, warning)
	default:
		c.eventRecorder.Warningf(pacemaker.EventReasonWarning, msgPacemakerWarning, warning)
	}
}

// recordErrorEvent records appropriate error events with specific reasons based on error content.
// This allows operators to filter and alert on specific types of degrading conditions.
func (c *PacemakerLifecycleManager) recordErrorEvent(errorMsg string) {
	eventReason := pacemaker.GetEventReasonForError(errorMsg)
	c.eventRecorder.Warningf(eventReason, msgPacemakerError, errorMsg)
}
