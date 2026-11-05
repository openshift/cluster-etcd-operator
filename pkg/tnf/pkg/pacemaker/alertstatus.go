package pacemaker

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
)

// This file populates the alert-agent entries OCPEDGE-2914 added to the PacemakerCluster API.
// Alert agents get a dedicated per-node alertAgents[] array (PacemakerClusterAlertAgentStatus),
// following the same "separate array" pattern as fencingAgents[] rather than reusing resources[]: an
// alert agent lives in the CIB's <alerts> section, not <resources>, and pacemaker never starts, stops,
// monitors, or moves it, so it doesn't share any of the pacemaker-managed resource conditions. Each
// entry carries exactly three conditions, derived from two independent signals:
//
//   - Cluster-wide CIB registration -> the Configured condition, derived from
//     `pcs alert config --output-format json` (the CIB is replicated to every node by Pacemaker, so
//     a single query from any node is authoritative and identical for all nodes).
//   - Per-node script presence -> the Available condition, derived from each Node's Machine Config
//     Operator reconciliation state (its annotations), which is readable for every node from any pod
//     regardless of where the collector runs.
//   - Healthy is the aggregate of the two above.
//
// Unlike the resources[]-reuse approach this replaced, alert-agent health here DOES fold upward: each
// node's alertAgents[].Healthy entries are aggregated into an optional AlertAgentsHealthy node
// condition, which in turn feeds into that node's Healthy condition (and so into the cluster's
// Healthy condition) exactly like fencingAgents[].Healthy feeds FencingAvailable/FencingHealthy.
// AlertAgentsHealthy is optional in the schema (no MinItems bump, no self.exists() rule) so that a
// status collector version predating alert-agent tracking - or one that simply hasn't observed
// alert-agent status yet this run - never fails validation; a Pending/Unknown or entirely absent
// observation is never treated as a failure, only a confirmed False folds into node/cluster Healthy.

// MCO publishes each node's reconciliation state on the Node object via these annotations.
const (
	mcoNodeStateAnnotation  = "machineconfiguration.openshift.io/state"
	mcoNodeReasonAnnotation = "machineconfiguration.openshift.io/reason"
	mcoStateDegraded        = "Degraded"
)

// alertHealthyReasonPending is used on the aggregate Healthy condition when it cannot yet be decided
// this run. The API doesn't export a Pending reason for the generic Healthy condition type (Kubelet/
// Etcd's Healthy condition never goes Unknown), so this stays a local constant.
const alertHealthyReasonPending = "Pending"

// alertAgentNameByID maps a pcs alert id (e.g. "tnf-taint-alert") to the API's
// PacemakerClusterAlertAgentName value used for the alert-agent entry in the CR.
var alertAgentNameByID = map[string]pacmkrv1.PacemakerClusterAlertAgentName{
	"tnf-taint-alert":   pacmkrv1.PacemakerClusterAlertAgentNameTaint,
	"tnf-untaint-alert": pacmkrv1.PacemakerClusterAlertAgentNameUntaint,
}

// alertAgentObservation is a single fresh observation of an alert-agent-related fact, either
// cluster-wide registration (Configured) or per-node script presence (Available). It maps directly to
// a condition: ok is the True/False status, with reason/msg attached. A nil *alertAgentObservation
// means "not observed this run" and triggers carry-forward or Pending in the merge layer.
type alertAgentObservation struct {
	ok     bool
	reason string
	msg    string
}

// =============================================================================
// Cluster-wide alert registration (the Configured condition)
// =============================================================================

// buildAlertAgentStatuses runs `pcs alert config` once and returns a per-agent observation of
// cluster-wide CIB registration for every known alert agent, keyed by the API agent name. It returns
// nil if the pcs command itself fails ("not observed this run", not "confirmed missing"), so the
// merge layer carries forward the previous Configured condition or reports Pending rather than a
// false NotRegistered.
func buildAlertAgentStatuses(ctx context.Context) map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation {
	observed, err := pcs.CollectAlertConfig(ctx)
	if err != nil {
		klog.Warningf("Failed to collect pcs alert config, alert-agent Configured conditions will not be refreshed this run: %v", err)
		return nil
	}
	return alertAgentStatusesFromObserved(observed)
}

// alertAgentStatusesFromObserved is the pure interpretation logic behind buildAlertAgentStatuses:
// it compares each expected AlertConfig against the observed CIB config and produces a per-agent
// Configured observation - Configured when path and event filter match, Misconfigured when the alert
// exists but a field differs, NotRegistered when it is absent from the CIB entirely.
func alertAgentStatusesFromObserved(observed map[string]pcs.ObservedAlertConfig) map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation {
	result := make(map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation, len(pcs.AlertConfigs))
	for _, ac := range pcs.AlertConfigs {
		name, known := alertAgentNameByID[ac.ID]
		if !known {
			continue
		}

		obs, found := observed[ac.ID]
		switch {
		case !found:
			result[name] = &alertAgentObservation{
				ok:     false,
				reason: pacmkrv1.AlertAgentConfiguredReasonNotRegistered,
				msg:    fmt.Sprintf("alert agent %q is not registered in the CIB", ac.ID),
			}
		case obs.Path != ac.Path || obs.SelectFencing != ac.ExpectFencing || obs.SelectNodes != ac.ExpectNodes:
			var diffs []string
			if obs.Path != ac.Path {
				diffs = append(diffs, fmt.Sprintf("path expected %q observed %q", ac.Path, obs.Path))
			}
			if obs.SelectFencing != ac.ExpectFencing {
				diffs = append(diffs, fmt.Sprintf("fencing expected %v observed %v", ac.ExpectFencing, obs.SelectFencing))
			}
			if obs.SelectNodes != ac.ExpectNodes {
				diffs = append(diffs, fmt.Sprintf("nodes expected %v observed %v", ac.ExpectNodes, obs.SelectNodes))
			}
			result[name] = &alertAgentObservation{
				ok:     false,
				reason: pacmkrv1.AlertAgentConfiguredReasonMisconfigured,
				msg:    fmt.Sprintf("alert agent %q is misconfigured: %s", ac.ID, strings.Join(diffs, "; ")),
			}
		default:
			result[name] = &alertAgentObservation{
				ok:     true,
				reason: pacmkrv1.AlertAgentConfiguredReasonConfigured,
				msg:    fmt.Sprintf("alert agent %q is registered in the CIB as expected", ac.ID),
			}
		}
	}
	return result
}

// =============================================================================
// Per-node script presence (the Available condition) via MCO node state
// =============================================================================

// buildAlertAgentScriptStatusesFromMCO reads MCO's per-node reconciliation state for every node
// named in nodeNames, in a single List call, and derives an alert-agent availability observation per
// known agent for each. It works identically regardless of which node the calling pod runs on, so it
// needs no per-node targeting or rotation. It returns nil only if the List call itself fails (nothing
// observed this run for any node); a wanted node absent from the returned list is simply absent from
// the result map ("not observed this run" for that node only).
func buildAlertAgentScriptStatusesFromMCO(ctx context.Context, kubeClient kubernetes.Interface, nodeNames []string) map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation {
	nodeList, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("Failed to list nodes for alert script MCO status, alert-agent Available conditions will not be refreshed this run: %v", err)
		return nil
	}

	wanted := make(map[string]bool, len(nodeNames))
	for _, n := range nodeNames {
		wanted[n] = true
	}

	result := make(map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation, len(nodeNames))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if !wanted[node.Name] {
			continue
		}
		result[node.Name] = alertAgentScriptObservationsFromMCOState(
			node.Annotations[mcoNodeStateAnnotation],
			node.Annotations[mcoNodeReasonAnnotation],
		)
	}
	return result
}

// alertAgentScriptObservationsFromMCOState derives an availability observation for every known alert
// agent on one node from that node's MCO state/reason annotations. It always returns a complete,
// decided entry per known agent - per the binary rule in alertAgentScriptObservationFromMCOState there
// is no "not observed" outcome once a Node object was successfully read.
func alertAgentScriptObservationsFromMCOState(state, reason string) map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation {
	result := make(map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation, len(pcs.AlertConfigs))
	for _, ac := range pcs.AlertConfigs {
		name, known := alertAgentNameByID[ac.ID]
		if !known {
			continue
		}
		result[name] = alertAgentScriptObservationFromMCOState(ac.Path, state, reason)
	}
	return result
}

// alertAgentScriptObservationFromMCOState implements the binary interpretation rule: a Degraded
// state whose reason names this exact script path is a confirmed absence (Available=False, reason
// Unavailable); everything else - not Degraded, or Degraded for a reason that does not name this
// script - is reported present. An unrelated degradation is intentionally not surfaced as Unknown
// here; the trade-off (a genuinely-missing script not yet named in the reason is under-reported as
// present until MCO names it) is accepted to keep this a clean binary signal.
func alertAgentScriptObservationFromMCOState(scriptPath, state, reason string) *alertAgentObservation {
	if state == mcoStateDegraded && strings.Contains(reason, scriptPath) {
		return &alertAgentObservation{
			ok:     false,
			reason: pacmkrv1.AlertAgentAvailableReasonUnavailable,
			msg:    fmt.Sprintf("node is Degraded due to %s: %s", scriptPath, reason),
		}
	}
	return &alertAgentObservation{
		ok:     true,
		reason: pacmkrv1.AlertAgentAvailableReasonAvailable,
		msg:    fmt.Sprintf("%s is present and executable with the expected permissions; no MachineConfig drift reported (node state=%q, reason=%q)", scriptPath, state, reason),
	}
}

// =============================================================================
// Building the alert-agent entries
// =============================================================================

// mergeAlertAgentResource builds one alert-agent entry (its Configured, Available, and aggregate
// Healthy conditions) for a single alert agent on a node, merging the two fresh signals against the
// existing entry so LastTransitionTime is preserved when a condition's status is unchanged. configured
// comes from the cluster-wide CIB registration; available from this node's script presence; Healthy is
// derived from both.
func mergeAlertAgentResource(name pacmkrv1.PacemakerClusterAlertAgentName, configured, available *alertAgentObservation, existing *pacmkrv1.PacemakerClusterAlertAgentStatus, now metav1.Time) pacmkrv1.PacemakerClusterAlertAgentStatus {
	var existingConfigured, existingAvailable, existingHealthy *metav1.Condition
	if existing != nil {
		existingConfigured = FindCondition(existing.Conditions, pacmkrv1.AlertAgentConfiguredConditionType)
		existingAvailable = FindCondition(existing.Conditions, pacmkrv1.AlertAgentAvailableConditionType)
		existingHealthy = FindCondition(existing.Conditions, pacmkrv1.AlertAgentHealthyConditionType)
	}

	configuredCond := mergeCondition(
		pacmkrv1.AlertAgentConfiguredConditionType,
		configured,
		existingConfigured,
		pacmkrv1.AlertAgentConfiguredReasonPending,
		"alert agent CIB registration has not yet been observed by the status collector",
		now,
	)
	availableCond := mergeCondition(
		pacmkrv1.AlertAgentAvailableConditionType,
		available,
		existingAvailable,
		pacmkrv1.AlertAgentAvailableReasonPending,
		"alert agent script presence has not yet been observed by the status collector",
		now,
	)

	// Aggregate the merged conditions so a confirmed failure overrides an unobserved signal.
	// Success requires both conditions to be True, including any carried-forward observations.
	var healthyFresh *alertAgentObservation
	ok := configuredCond.Status == metav1.ConditionTrue && availableCond.Status == metav1.ConditionTrue
	if ok || configuredCond.Status == metav1.ConditionFalse || availableCond.Status == metav1.ConditionFalse {
		healthyFresh = &alertAgentObservation{
			ok:     ok,
			reason: alertHealthyReason(ok),
			msg:    alertHealthyMessage(ok, configuredCond, availableCond),
		}
	}
	healthyCond := mergeCondition(
		pacmkrv1.AlertAgentHealthyConditionType,
		healthyFresh,
		existingHealthy,
		alertHealthyReasonPending,
		"alert agent health has not yet been observed by the status collector",
		now,
	)

	return pacmkrv1.PacemakerClusterAlertAgentStatus{
		Conditions: []metav1.Condition{healthyCond, configuredCond, availableCond},
		Name:       name,
	}
}

func alertHealthyReason(ok bool) string {
	if ok {
		return pacmkrv1.AlertAgentHealthyReasonHealthy
	}
	return pacmkrv1.AlertAgentHealthyReasonUnhealthy
}

func alertHealthyMessage(ok bool, configured, available metav1.Condition) string {
	if ok {
		return "alert agent is registered in the CIB and its script is present on this node"
	}
	var problems []string
	if configured.Status == metav1.ConditionFalse {
		problems = append(problems, configured.Message)
	}
	if available.Status == metav1.ConditionFalse {
		problems = append(problems, available.Message)
	}
	return strings.Join(problems, "; ")
}

// =============================================================================
// Applying the merged alert-agent entries onto a freshly-built PacemakerClusterStatus
// =============================================================================

// nodeNamesFromStatus returns the node names present in a freshly-built status, used to scope the
// MCO node lookup to the nodes that will actually appear in the CR.
func nodeNamesFromStatus(status *pacmkrv1.PacemakerClusterStatus) []string {
	if status == nil || status.Nodes == nil {
		return nil
	}
	names := make([]string, 0, len(*status.Nodes))
	for _, n := range *status.Nodes {
		names = append(names, n.NodeName)
	}
	return names
}

// applyAlertAgentStatus populates each node's alertAgents[] in the freshly-built status, merging each
// entry against the corresponding entry on the existing CR (existing may be a zero-valued status on
// the create path, which the merge treats as "no prior value") so LastTransitionTime is preserved. For
// each node it also folds the merged entries into an optional AlertAgentsHealthy node condition, and
// - unlike the resources[]-reuse design this replaced - propagates a confirmed-unhealthy fold into
// that node's Healthy condition and the cluster's Healthy condition, via refreshHealthAfterAlertAgents.
// freshAgents / freshScripts being nil means "not observed this run" and drives carry-forward or
// Pending inside the merge helpers.
func applyAlertAgentStatus(
	status *pacmkrv1.PacemakerClusterStatus,
	existing *pacmkrv1.PacemakerClusterStatus,
	freshAgents map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation,
	freshScripts map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation,
	now metav1.Time,
	alertAgentStatusEnabled bool,
) {
	// The status is rebuilt from Pacemaker on every run. Do not carry forward alert-agent
	// entries or fold their health into the fresh status when the gate is disabled.
	if !alertAgentStatusEnabled || status.Nodes == nil {
		return
	}

	existingAlertAgentsByNode := make(map[string][]pacmkrv1.PacemakerClusterAlertAgentStatus)
	existingConditionsByNode := make(map[string][]metav1.Condition)
	if existing != nil && existing.Nodes != nil {
		for i := range *existing.Nodes {
			n := &(*existing.Nodes)[i]
			existingAlertAgentsByNode[n.NodeName] = n.AlertAgents
			existingConditionsByNode[n.NodeName] = n.Conditions
		}
	}

	nodes := *status.Nodes
	for i := range nodes {
		node := &nodes[i]
		var freshForNode map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation
		if freshScripts != nil {
			freshForNode = freshScripts[node.NodeName]
		}

		agents, nodeHealthy := applyAlertAgentEntries(node.AlertAgents, existingAlertAgentsByNode[node.NodeName], freshAgents, freshForNode, now)
		node.AlertAgents = agents

		existingAlertAgentsHealthy := FindCondition(existingConditionsByNode[node.NodeName], pacmkrv1.NodeAlertAgentsHealthyConditionType)
		mergeNodeAlertAgentsHealthy(node, nodeHealthy, existingAlertAgentsHealthy, now)
	}

	refreshHealthAfterAlertAgents(status, now)
}

// applyAlertAgentEntries builds the merged alertAgents[] entries for a single node and, alongside
// them, an aggregate observation for the node's AlertAgentsHealthy condition. To avoid synthesizing a
// phantom all-Pending entry before any signal has ever been observed, an agent is skipped entirely
// when it has neither a fresh signal this run nor a carried-forward entry. The aggregate uses the
// merged Healthy conditions: any False makes the node aggregate unhealthy, and all True makes it
// healthy. Otherwise it is nil, so the node condition carries forward or remains absent.
func applyAlertAgentEntries(agents, existing []pacmkrv1.PacemakerClusterAlertAgentStatus, freshAgents, freshScripts map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation, now metav1.Time) ([]pacmkrv1.PacemakerClusterAlertAgentStatus, *alertAgentObservation) {
	existingByName := make(map[pacmkrv1.PacemakerClusterAlertAgentName]*pacmkrv1.PacemakerClusterAlertAgentStatus, len(existing))
	for i := range existing {
		existingByName[existing[i].Name] = &existing[i]
	}

	var appended, unhealthyCount, healthyCount int
	for _, ac := range pcs.AlertConfigs {
		name, known := alertAgentNameByID[ac.ID]
		if !known {
			continue
		}

		var configuredObs, availableObs *alertAgentObservation
		if freshAgents != nil {
			configuredObs = freshAgents[name]
		}
		if freshScripts != nil {
			availableObs = freshScripts[name]
		}
		existingEntry := existingByName[name]

		if configuredObs == nil && availableObs == nil && existingEntry == nil {
			continue
		}
		agent := mergeAlertAgentResource(name, configuredObs, availableObs, existingEntry, now)
		agents = append(agents, agent)
		appended++

		switch getConditionStatus(agent.Conditions, pacmkrv1.AlertAgentHealthyConditionType) {
		case metav1.ConditionTrue:
			healthyCount++
		case metav1.ConditionFalse:
			unhealthyCount++
		}
	}

	var nodeHealthy *alertAgentObservation
	if unhealthyCount > 0 || (appended > 0 && healthyCount == appended) {
		ok := healthyCount == appended
		nodeHealthy = &alertAgentObservation{
			ok:     ok,
			reason: nodeAlertAgentsHealthyReason(ok),
			msg:    nodeAlertAgentsHealthyMessage(ok),
		}
	}
	return agents, nodeHealthy
}

func nodeAlertAgentsHealthyReason(ok bool) string {
	if ok {
		return "AllAlertAgentsHealthy"
	}
	return "SomeAlertAgentsUnhealthy"
}

func nodeAlertAgentsHealthyMessage(ok bool) string {
	if ok {
		return "All tracked alert agents are healthy"
	}
	return "One or more tracked alert agents are unhealthy"
}

// mergeNodeAlertAgentsHealthy merges the fresh per-node alert-agent aggregate into the node's
// AlertAgentsHealthy condition and appends it to node.Conditions. Unlike the other node-level
// conditions, this one is genuinely absent (not just Unknown) until either a fresh aggregate or a
// carried-forward value exists - matching the API's optional-in-the-schema design so a status
// collector that has never observed alert-agent status never has to invent a placeholder condition.
func mergeNodeAlertAgentsHealthy(node *pacmkrv1.PacemakerClusterNodeStatus, fresh *alertAgentObservation, existing *metav1.Condition, now metav1.Time) {
	if fresh == nil && existing == nil {
		return
	}
	cond := mergeCondition(
		pacmkrv1.NodeAlertAgentsHealthyConditionType,
		fresh,
		existing,
		pacmkrv1.NodeAlertAgentsHealthyReasonPending,
		"alert agent health has not yet been observed by the status collector",
		now,
	)
	node.Conditions = append(node.Conditions, cond)
}

// refreshHealthAfterAlertAgents folds a confirmed-unhealthy AlertAgentsHealthy condition into its
// node's Healthy condition, then recomputes the cluster's Healthy condition from the (possibly
// updated) per-node Healthy statuses - completing the aggregation chain described in the package
// comment: alertAgents[].Healthy -> AlertAgentsHealthy -> node.Healthy -> cluster.Healthy. A
// Pending/Unknown or absent AlertAgentsHealthy is never treated as a failure and never flips an
// otherwise-healthy node to unhealthy; only a confirmed False does.
func refreshHealthAfterAlertAgents(status *pacmkrv1.PacemakerClusterStatus, now metav1.Time) {
	if status.Nodes == nil {
		return
	}
	nodes := *status.Nodes
	for i := range nodes {
		node := &nodes[i]
		if getConditionStatus(node.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType) != metav1.ConditionFalse {
			continue
		}
		if healthy := FindCondition(node.Conditions, pacmkrv1.NodeHealthyConditionType); healthy != nil && healthy.Status == metav1.ConditionTrue {
			healthy.Status = metav1.ConditionFalse
			healthy.Reason = pacmkrv1.NodeHealthyReasonUnhealthy
			healthy.Message = "One or more alert agents are unhealthy"
			healthy.LastTransitionTime = now
		}
	}

	nodeCountCond := FindCondition(status.Conditions, pacmkrv1.ClusterNodeCountAsExpectedConditionType)
	inServiceCond := FindCondition(status.Conditions, pacmkrv1.ClusterInServiceConditionType)
	clusterHealthy := FindCondition(status.Conditions, pacmkrv1.ClusterHealthyConditionType)
	if nodeCountCond != nil && inServiceCond != nil && clusterHealthy != nil {
		*clusterHealthy = buildClusterHealthyCondition(*nodeCountCond, *inServiceCond, nodes, now)
	}
}

// =============================================================================
// Shared three-tier condition merge
// =============================================================================

// mergeCondition merges a fresh observation for one condition against its existing value:
//   - Tier 1 (fresh present): the fresh observation wins, stamped now - but LastTransitionTime is
//     preserved from the existing condition when the status is unchanged.
//   - Tier 2 (no fresh, existing present): the existing condition is carried forward unchanged,
//     preserving its LastTransitionTime (so a prior Pending stays Pending, not re-timestamped).
//   - Tier 3 (no fresh, no existing): an Unknown/Pending condition is synthesized, stamped now.
func mergeCondition(condType string, fresh *alertAgentObservation, existing *metav1.Condition, pendingReason, pendingMsg string, now metav1.Time) metav1.Condition {
	if fresh != nil {
		status := metav1.ConditionFalse
		if fresh.ok {
			status = metav1.ConditionTrue
		}
		cond := metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             fresh.reason,
			Message:            fresh.msg,
			LastTransitionTime: now,
		}
		if existing != nil && existing.Status == status {
			cond.LastTransitionTime = existing.LastTransitionTime
		}
		return cond
	}

	if existing != nil {
		return *existing
	}

	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionUnknown,
		Reason:             pendingReason,
		Message:            pendingMsg,
		LastTransitionTime: now,
	}
}
