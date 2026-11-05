package pacemaker

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
)

// observedAll returns an observed CIB map in which every known alert agent is registered exactly as
// expected, so alertAgentStatusesFromObserved reports each as Configured.
func observedAll() map[string]pcs.ObservedAlertConfig {
	m := make(map[string]pcs.ObservedAlertConfig, len(pcs.AlertConfigs))
	for _, ac := range pcs.AlertConfigs {
		m[ac.ID] = pcs.ObservedAlertConfig{
			Path:          ac.Path,
			SelectFencing: ac.ExpectFencing,
			SelectNodes:   ac.ExpectNodes,
		}
	}
	return m
}

func TestAlertAgentStatusesFromObserved(t *testing.T) {
	taint := pcs.AlertConfigs[0]
	untaint := pcs.AlertConfigs[1]
	require.Equal(t, "tnf-taint-alert", taint.ID)
	require.Equal(t, "tnf-untaint-alert", untaint.ID)

	tests := []struct {
		name          string
		observed      map[string]pcs.ObservedAlertConfig
		wantTaintOK   bool
		wantTaintRsn  string
		wantUntaintOK bool
		wantUntntRsn  string
	}{
		{
			name:          "all configured",
			observed:      observedAll(),
			wantTaintOK:   true,
			wantTaintRsn:  pacmkrv1.AlertAgentConfiguredReasonConfigured,
			wantUntaintOK: true,
			wantUntntRsn:  pacmkrv1.AlertAgentConfiguredReasonConfigured,
		},
		{
			name:          "taint missing",
			observed:      map[string]pcs.ObservedAlertConfig{untaint.ID: {Path: untaint.Path, SelectNodes: true}},
			wantTaintOK:   false,
			wantTaintRsn:  pacmkrv1.AlertAgentConfiguredReasonNotRegistered,
			wantUntaintOK: true,
			wantUntntRsn:  pacmkrv1.AlertAgentConfiguredReasonConfigured,
		},
		{
			name: "untaint wrong path is misconfigured",
			observed: map[string]pcs.ObservedAlertConfig{
				taint.ID:   {Path: taint.Path, SelectFencing: true},
				untaint.ID: {Path: "/wrong/path.sh", SelectNodes: true},
			},
			wantTaintOK:   true,
			wantTaintRsn:  pacmkrv1.AlertAgentConfiguredReasonConfigured,
			wantUntaintOK: false,
			wantUntntRsn:  pacmkrv1.AlertAgentConfiguredReasonMisconfigured,
		},
		{
			name: "taint wrong event filter is misconfigured",
			observed: map[string]pcs.ObservedAlertConfig{
				taint.ID:   {Path: taint.Path, SelectFencing: false, SelectNodes: true},
				untaint.ID: {Path: untaint.Path, SelectNodes: true},
			},
			wantTaintOK:   false,
			wantTaintRsn:  pacmkrv1.AlertAgentConfiguredReasonMisconfigured,
			wantUntaintOK: true,
			wantUntntRsn:  pacmkrv1.AlertAgentConfiguredReasonConfigured,
		},
		{
			name:          "nothing observed means both missing",
			observed:      map[string]pcs.ObservedAlertConfig{},
			wantTaintOK:   false,
			wantTaintRsn:  pacmkrv1.AlertAgentConfiguredReasonNotRegistered,
			wantUntaintOK: false,
			wantUntntRsn:  pacmkrv1.AlertAgentConfiguredReasonNotRegistered,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := alertAgentStatusesFromObserved(tc.observed)

			taintObs := got[pacmkrv1.PacemakerClusterAlertAgentNameTaint]
			require.NotNil(t, taintObs)
			require.Equal(t, tc.wantTaintOK, taintObs.ok)
			require.Equal(t, tc.wantTaintRsn, taintObs.reason)

			untaintObs := got[pacmkrv1.PacemakerClusterAlertAgentNameUntaint]
			require.NotNil(t, untaintObs)
			require.Equal(t, tc.wantUntaintOK, untaintObs.ok)
			require.Equal(t, tc.wantUntntRsn, untaintObs.reason)
		})
	}
}

func TestAlertAgentStatusesFromObserved_MisconfiguredMessageNamesOnlyDifferingFields(t *testing.T) {
	untaint := pcs.AlertConfigs[1]
	require.Equal(t, "tnf-untaint-alert", untaint.ID)

	// Only fencing differs from expected; path and nodes match exactly.
	observed := map[string]pcs.ObservedAlertConfig{
		untaint.ID: {Path: untaint.Path, SelectFencing: true, SelectNodes: untaint.ExpectNodes},
	}

	got := alertAgentStatusesFromObserved(observed)
	untaintObs := got[pacmkrv1.PacemakerClusterAlertAgentNameUntaint]
	require.NotNil(t, untaintObs)
	require.False(t, untaintObs.ok)
	require.Equal(t, pacmkrv1.AlertAgentConfiguredReasonMisconfigured, untaintObs.reason)

	require.Contains(t, untaintObs.msg, "fencing", "the field that actually differs must be named")
	require.NotContains(t, untaintObs.msg, "path", "path matched expected/observed and must not be named")
	require.NotContains(t, untaintObs.msg, "nodes", "nodes matched expected/observed and must not be named")
}

func TestAlertAgentScriptObservationFromMCOState(t *testing.T) {
	scriptPath := pcs.AlertConfigs[0].Path

	tests := []struct {
		name    string
		state   string
		reason  string
		wantOK  bool
		wantRsn string
	}{
		{
			name:    "not degraded is present",
			state:   "Done",
			reason:  "",
			wantOK:  true,
			wantRsn: pacmkrv1.AlertAgentAvailableReasonAvailable,
		},
		{
			name:    "degraded naming this script is missing",
			state:   mcoStateDegraded,
			reason:  "failed to write file " + scriptPath,
			wantOK:  false,
			wantRsn: pacmkrv1.AlertAgentAvailableReasonUnavailable,
		},
		{
			name:    "degraded for an unrelated reason stays present",
			state:   mcoStateDegraded,
			reason:  "some other unrelated failure",
			wantOK:  true,
			wantRsn: pacmkrv1.AlertAgentAvailableReasonAvailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := alertAgentScriptObservationFromMCOState(scriptPath, tc.state, tc.reason)
			require.NotNil(t, got)
			require.Equal(t, tc.wantOK, got.ok)
			require.Equal(t, tc.wantRsn, got.reason)
		})
	}
}

func TestMergeCondition(t *testing.T) {
	now := metav1.Now()
	old := metav1.NewTime(now.Add(-time.Hour))
	const condType = pacmkrv1.AlertAgentConfiguredConditionType

	t.Run("fresh with no existing stamps now", func(t *testing.T) {
		got := mergeCondition(condType, &alertAgentObservation{ok: true, reason: "Configured", msg: "ok"}, nil, "Pending", "pending", now)
		require.Equal(t, metav1.ConditionTrue, got.Status)
		require.Equal(t, now, got.LastTransitionTime)
	})

	t.Run("fresh unchanged status preserves LastTransitionTime", func(t *testing.T) {
		existing := &metav1.Condition{Type: condType, Status: metav1.ConditionTrue, Reason: "Configured", LastTransitionTime: old}
		got := mergeCondition(condType, &alertAgentObservation{ok: true, reason: "Configured", msg: "still ok"}, existing, "Pending", "pending", now)
		require.Equal(t, metav1.ConditionTrue, got.Status)
		require.Equal(t, old, got.LastTransitionTime, "LastTransitionTime must be preserved when status is unchanged")
		require.Equal(t, "still ok", got.Message, "message must still refresh")
	})

	t.Run("fresh changed status stamps now", func(t *testing.T) {
		existing := &metav1.Condition{Type: condType, Status: metav1.ConditionTrue, Reason: "Configured", LastTransitionTime: old}
		got := mergeCondition(condType, &alertAgentObservation{ok: false, reason: "NotRegistered", msg: "gone"}, existing, "Pending", "pending", now)
		require.Equal(t, metav1.ConditionFalse, got.Status)
		require.Equal(t, now, got.LastTransitionTime, "LastTransitionTime must advance when status changes")
	})

	t.Run("no fresh carries existing forward verbatim", func(t *testing.T) {
		existing := &metav1.Condition{Type: condType, Status: metav1.ConditionFalse, Reason: "NotRegistered", Message: "prior", LastTransitionTime: old}
		got := mergeCondition(condType, nil, existing, "Pending", "pending", now)
		require.Equal(t, *existing, got)
	})

	t.Run("no fresh no existing synthesizes Unknown Pending", func(t *testing.T) {
		got := mergeCondition(condType, nil, nil, "Pending", "pending msg", now)
		require.Equal(t, metav1.ConditionUnknown, got.Status)
		require.Equal(t, "Pending", got.Reason)
		require.Equal(t, "pending msg", got.Message)
		require.Equal(t, now, got.LastTransitionTime)
	})
}

func TestMergeAlertAgentResource(t *testing.T) {
	now := metav1.Now()
	old := metav1.NewTime(now.Add(-time.Hour))
	name := pacmkrv1.PacemakerClusterAlertAgentNameTaint

	requireThreeConditions := func(t *testing.T, entry pacmkrv1.PacemakerClusterAlertAgentStatus) {
		t.Helper()
		require.Equal(t, name, entry.Name)
		require.NotNil(t, FindCondition(entry.Conditions, pacmkrv1.AlertAgentHealthyConditionType))
		require.NotNil(t, FindCondition(entry.Conditions, pacmkrv1.AlertAgentConfiguredConditionType))
		require.NotNil(t, FindCondition(entry.Conditions, pacmkrv1.AlertAgentAvailableConditionType))
	}

	t.Run("both signals ok yields Healthy True", func(t *testing.T) {
		configured := &alertAgentObservation{ok: true, reason: pacmkrv1.AlertAgentConfiguredReasonConfigured, msg: "registered"}
		available := &alertAgentObservation{ok: true, reason: pacmkrv1.AlertAgentAvailableReasonAvailable, msg: "present"}

		got := mergeAlertAgentResource(name, configured, available, nil, now)
		requireThreeConditions(t, got)
		require.Equal(t, metav1.ConditionTrue, FindCondition(got.Conditions, pacmkrv1.AlertAgentConfiguredConditionType).Status)
		require.Equal(t, metav1.ConditionTrue, FindCondition(got.Conditions, pacmkrv1.AlertAgentAvailableConditionType).Status)
		healthy := FindCondition(got.Conditions, pacmkrv1.AlertAgentHealthyConditionType)
		require.Equal(t, metav1.ConditionTrue, healthy.Status)
		require.Equal(t, pacmkrv1.AlertAgentHealthyReasonHealthy, healthy.Reason)
	})

	t.Run("script missing makes Healthy False and names the problem", func(t *testing.T) {
		configured := &alertAgentObservation{ok: true, reason: pacmkrv1.AlertAgentConfiguredReasonConfigured, msg: "registered"}
		available := &alertAgentObservation{ok: false, reason: pacmkrv1.AlertAgentAvailableReasonUnavailable, msg: "script missing on this node"}

		got := mergeAlertAgentResource(name, configured, available, nil, now)
		require.Equal(t, metav1.ConditionTrue, FindCondition(got.Conditions, pacmkrv1.AlertAgentConfiguredConditionType).Status)
		require.Equal(t, metav1.ConditionFalse, FindCondition(got.Conditions, pacmkrv1.AlertAgentAvailableConditionType).Status)
		healthy := FindCondition(got.Conditions, pacmkrv1.AlertAgentHealthyConditionType)
		require.Equal(t, metav1.ConditionFalse, healthy.Status)
		require.Equal(t, pacmkrv1.AlertAgentHealthyReasonUnhealthy, healthy.Reason)
		require.Contains(t, healthy.Message, "script missing on this node")
	})

	t.Run("only available observed leaves Configured and Healthy Pending", func(t *testing.T) {
		available := &alertAgentObservation{ok: true, reason: pacmkrv1.AlertAgentAvailableReasonAvailable, msg: "present"}

		got := mergeAlertAgentResource(name, nil, available, nil, now)
		requireThreeConditions(t, got)
		require.Equal(t, metav1.ConditionUnknown, FindCondition(got.Conditions, pacmkrv1.AlertAgentConfiguredConditionType).Status)
		require.Equal(t, metav1.ConditionTrue, FindCondition(got.Conditions, pacmkrv1.AlertAgentAvailableConditionType).Status)
		require.Equal(t, metav1.ConditionUnknown, FindCondition(got.Conditions, pacmkrv1.AlertAgentHealthyConditionType).Status, "Healthy needs both signals to be decided")
	})

	t.Run("unchanged status preserves prior LastTransitionTime", func(t *testing.T) {
		existing := &pacmkrv1.PacemakerClusterAlertAgentStatus{
			Name: name,
			Conditions: []metav1.Condition{
				{Type: pacmkrv1.AlertAgentConfiguredConditionType, Status: metav1.ConditionTrue, Reason: pacmkrv1.AlertAgentConfiguredReasonConfigured, LastTransitionTime: old},
			},
		}
		configured := &alertAgentObservation{ok: true, reason: pacmkrv1.AlertAgentConfiguredReasonConfigured, msg: "still registered"}
		available := &alertAgentObservation{ok: true, reason: pacmkrv1.AlertAgentAvailableReasonAvailable, msg: "present"}

		got := mergeAlertAgentResource(name, configured, available, existing, now)
		require.Equal(t, old, FindCondition(got.Conditions, pacmkrv1.AlertAgentConfiguredConditionType).LastTransitionTime, "Configured unchanged, LTT preserved")
	})
}

func TestNodeNamesFromStatus(t *testing.T) {
	require.Nil(t, nodeNamesFromStatus(nil))
	require.Nil(t, nodeNamesFromStatus(&pacmkrv1.PacemakerClusterStatus{}))

	nodes := []pacmkrv1.PacemakerClusterNodeStatus{{NodeName: "master-0"}, {NodeName: "master-1"}}
	got := nodeNamesFromStatus(&pacmkrv1.PacemakerClusterStatus{Nodes: &nodes})
	require.Equal(t, []string{"master-0", "master-1"}, got)
}

func alertEntry(node pacmkrv1.PacemakerClusterNodeStatus, name pacmkrv1.PacemakerClusterAlertAgentName) *pacmkrv1.PacemakerClusterAlertAgentStatus {
	for i := range node.AlertAgents {
		if node.AlertAgents[i].Name == name {
			return &node.AlertAgents[i]
		}
	}
	return nil
}

func healthyClusterStatus(now metav1.Time, nodes []pacmkrv1.PacemakerClusterNodeStatus) *pacmkrv1.PacemakerClusterStatus {
	return &pacmkrv1.PacemakerClusterStatus{
		Conditions: []metav1.Condition{
			{Type: pacmkrv1.ClusterHealthyConditionType, Status: metav1.ConditionTrue},
			{Type: pacmkrv1.ClusterInServiceConditionType, Status: metav1.ConditionTrue},
			{Type: pacmkrv1.ClusterNodeCountAsExpectedConditionType, Status: metav1.ConditionTrue},
		},
		LastUpdated: now,
		Nodes:       &nodes,
	}
}

func TestApplyAlertAgentStatus(t *testing.T) {
	now := metav1.Now()
	old := metav1.NewTime(now.Add(-time.Hour))

	// status freshly built this run: two nodes, each already Healthy from core Kubelet/Etcd resources.
	healthyNodeConditions := func() []metav1.Condition {
		return []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}
	}
	nodes := []pacmkrv1.PacemakerClusterNodeStatus{
		{NodeName: "master-0", Conditions: healthyNodeConditions()},
		{NodeName: "master-1", Conditions: healthyNodeConditions()},
	}
	status := healthyClusterStatus(now, nodes)

	// existing CR: master-0 already had a TaintAlertAgent entry with Configured True from a prior run.
	existingNodes := []pacmkrv1.PacemakerClusterNodeStatus{
		{
			NodeName: "master-0",
			AlertAgents: []pacmkrv1.PacemakerClusterAlertAgentStatus{
				{
					Name: pacmkrv1.PacemakerClusterAlertAgentNameTaint,
					Conditions: []metav1.Condition{
						{Type: pacmkrv1.AlertAgentConfiguredConditionType, Status: metav1.ConditionTrue, Reason: pacmkrv1.AlertAgentConfiguredReasonConfigured, LastTransitionTime: old},
					},
				},
			},
		},
	}
	existing := &pacmkrv1.PacemakerClusterStatus{Nodes: &existingNodes}

	freshAgents := alertAgentStatusesFromObserved(observedAll())
	freshScripts := map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
		"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
		"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
	}

	applyAlertAgentStatus(status, existing, freshAgents, freshScripts, now, true)

	// Cluster Healthy must remain True: both nodes' alert agents are fully healthy.
	require.Equal(t, metav1.ConditionTrue, FindCondition(status.Conditions, pacmkrv1.ClusterHealthyConditionType).Status)

	// Every node must gain the two alert-agent entries and a True AlertAgentsHealthy node condition.
	for _, n := range *status.Nodes {
		require.Len(t, n.AlertAgents, 2, "TaintAlertAgent + UntaintAlertAgent for node %s", n.NodeName)

		taint := alertEntry(n, pacmkrv1.PacemakerClusterAlertAgentNameTaint)
		require.NotNil(t, taint)
		require.Equal(t, metav1.ConditionTrue, FindCondition(taint.Conditions, pacmkrv1.AlertAgentHealthyConditionType).Status)

		untaint := alertEntry(n, pacmkrv1.PacemakerClusterAlertAgentNameUntaint)
		require.NotNil(t, untaint)
		require.Equal(t, metav1.ConditionTrue, FindCondition(untaint.Conditions, pacmkrv1.AlertAgentHealthyConditionType).Status)

		require.Equal(t, metav1.ConditionTrue, getConditionStatus(n.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType))
		require.Equal(t, "AllAlertAgentsHealthy", FindCondition(n.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType).Reason)
		require.Equal(t, metav1.ConditionTrue, getConditionStatus(n.Conditions, pacmkrv1.NodeHealthyConditionType), "an all-healthy fold must not flip node.Healthy")
	}

	// master-0's carried-forward TaintAlertAgent Configured condition must keep its prior LastTransitionTime.
	master0 := (*status.Nodes)[0]
	require.Equal(t, "master-0", master0.NodeName)
	master0Taint := alertEntry(master0, pacmkrv1.PacemakerClusterAlertAgentNameTaint)
	require.Equal(t, old, FindCondition(master0Taint.Conditions, pacmkrv1.AlertAgentConfiguredConditionType).LastTransitionTime)

	// master-1 had no prior entry, so its Configured condition is freshly stamped.
	master1 := (*status.Nodes)[1]
	master1Taint := alertEntry(master1, pacmkrv1.PacemakerClusterAlertAgentNameTaint)
	require.Equal(t, now, FindCondition(master1Taint.Conditions, pacmkrv1.AlertAgentConfiguredConditionType).LastTransitionTime)
}

func TestApplyAlertAgentStatus_SkipsPhantomEntriesWhenNothingObserved(t *testing.T) {
	now := metav1.Now()

	nodes := []pacmkrv1.PacemakerClusterNodeStatus{
		{
			NodeName:   "master-0",
			Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}},
		},
	}
	status := healthyClusterStatus(now, nodes)

	// Neither signal observed this run and no existing CR: no alert-agent entries or condition added.
	applyAlertAgentStatus(status, &pacmkrv1.PacemakerClusterStatus{}, nil, nil, now, true)

	require.Empty(t, (*status.Nodes)[0].AlertAgents, "no phantom alert-agent entries before anything is observed")
	require.Nil(t, FindCondition((*status.Nodes)[0].Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType), "AlertAgentsHealthy stays absent, not Unknown, before anything is observed")
	require.Equal(t, metav1.ConditionTrue, FindCondition(status.Conditions, pacmkrv1.ClusterHealthyConditionType).Status)
}

func TestApplyAlertAgentStatus_UnhealthyAlertAgentFoldsIntoNodeAndClusterHealthy(t *testing.T) {
	now := metav1.Now()

	nodes := []pacmkrv1.PacemakerClusterNodeStatus{
		{NodeName: "master-0", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
		{NodeName: "master-1", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
	}
	status := healthyClusterStatus(now, nodes)

	// Configured is cluster-wide and fine for both agents on every node. master-0's taint
	// alert script is missing per its MCO state (Available=False); master-1 is fully healthy - this
	// isolates the fold to the per-node signal, since Configured cannot differ between nodes.
	taintPath := pcs.AlertConfigs[0].Path
	freshAgents := alertAgentStatusesFromObserved(observedAll())
	freshScripts := map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
		"master-0": alertAgentScriptObservationsFromMCOState(mcoStateDegraded, "failed to write file "+taintPath),
		"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
	}

	applyAlertAgentStatus(status, &pacmkrv1.PacemakerClusterStatus{}, freshAgents, freshScripts, now, true)

	master0 := (*status.Nodes)[0]
	require.Equal(t, metav1.ConditionFalse, getConditionStatus(master0.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType))
	require.Equal(t, "SomeAlertAgentsUnhealthy", FindCondition(master0.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType).Reason)
	require.Equal(t, metav1.ConditionFalse, getConditionStatus(master0.Conditions, pacmkrv1.NodeHealthyConditionType), "a confirmed-unhealthy alert agent must fold into node.Healthy")

	master1 := (*status.Nodes)[1]
	require.Equal(t, metav1.ConditionTrue, getConditionStatus(master1.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType))
	require.Equal(t, metav1.ConditionTrue, getConditionStatus(master1.Conditions, pacmkrv1.NodeHealthyConditionType))

	require.Equal(t, metav1.ConditionFalse, FindCondition(status.Conditions, pacmkrv1.ClusterHealthyConditionType).Status,
		"one unhealthy node must fold into cluster.Healthy")
}

func TestApplyAlertAgentStatus_PartialFailureReachesHealthCheck(t *testing.T) {
	taint := pacmkrv1.PacemakerClusterAlertAgentNameTaint
	missingConfig := observedAll()
	delete(missingConfig, pcs.AlertConfigs[0].ID)
	misconfigured := observedAll()
	wrongPath := misconfigured[pcs.AlertConfigs[0].ID]
	wrongPath.Path = "/wrong/path.sh"
	misconfigured[pcs.AlertConfigs[0].ID] = wrongPath
	goodScripts := map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
		"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
		"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
	}

	for _, tc := range []struct {
		name       string
		configured map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation
		scripts    map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation
		failedType string
		issue      string
		wantErrors int
	}{
		{
			name:       "missing registration while node list fails",
			configured: alertAgentStatusesFromObserved(missingConfig),
			failedType: pacmkrv1.AlertAgentConfiguredConditionType,
			issue:      "not registered in the CIB",
			wantErrors: 2,
		},
		{
			name:       "misconfigured registration while node list fails",
			configured: alertAgentStatusesFromObserved(misconfigured),
			failedType: pacmkrv1.AlertAgentConfiguredConditionType,
			issue:      "misconfigured",
			wantErrors: 2,
		},
		{
			name: "missing script while registration query fails",
			scripts: map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
				"master-0": alertAgentScriptObservationsFromMCOState(mcoStateDegraded, "missing "+pcs.AlertConfigs[0].Path),
				"master-1": goodScripts["master-1"],
			},
			failedType: pacmkrv1.AlertAgentAvailableConditionType,
			issue:      "missing " + pcs.AlertConfigs[0].Path,
			wantErrors: 1,
		},
	} {
		for _, previouslyHealthy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/previouslyHealthy=%t", tc.name, previouslyHealthy), func(t *testing.T) {
				now := metav1.Now()
				old := metav1.NewTime(now.Add(-time.Minute))
				newStatus := func(at metav1.Time) *pacmkrv1.PacemakerClusterStatus {
					return healthyClusterStatus(at, []pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.1.10"}),
						createHealthyNodeStatus("master-1", []string{"192.168.1.11"}),
					})
				}
				var existing *pacmkrv1.PacemakerClusterStatus
				if previouslyHealthy {
					existing = newStatus(old)
					applyAlertAgentStatus(existing, nil, alertAgentStatusesFromObserved(observedAll()), goodScripts, old, true)
					require.Equal(t, StatusHealthy, BuildHealthStatusFromCR(&pacmkrv1.PacemakerCluster{Status: *existing}).OverallStatus)
				}

				status := newStatus(now)
				applyAlertAgentStatus(status, existing, tc.configured, tc.scripts, now, true)
				for _, phase := range []string{"failure observed", "both queries unavailable"} {
					if phase == "both queries unavailable" {
						existing = status
						status = newStatus(metav1.NewTime(now.Add(time.Second)))
						applyAlertAgentStatus(status, existing, nil, nil, status.LastUpdated, true)
					}
					node := (*status.Nodes)[0]
					agent := alertEntry(node, taint)
					require.NotNil(t, agent)
					require.Equal(t, metav1.ConditionFalse, getConditionStatus(agent.Conditions, tc.failedType), phase)
					healthy := FindCondition(agent.Conditions, pacmkrv1.AlertAgentHealthyConditionType)
					require.Equal(t, metav1.ConditionFalse, healthy.Status, phase)
					require.Contains(t, healthy.Message, tc.issue)
					require.Equal(t, now, healthy.LastTransitionTime)
					nodeHealthy := FindCondition(node.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType)
					require.NotNil(t, nodeHealthy)
					require.Equal(t, metav1.ConditionFalse, nodeHealthy.Status, phase)
					require.Equal(t, now, nodeHealthy.LastTransitionTime)
					require.Equal(t, metav1.ConditionFalse, getConditionStatus(node.Conditions, pacmkrv1.NodeHealthyConditionType), phase)
					require.Equal(t, metav1.ConditionFalse, getConditionStatus(status.Conditions, pacmkrv1.ClusterHealthyConditionType), phase)
					health := BuildHealthStatusFromCR(&pacmkrv1.PacemakerCluster{Status: *status})
					require.Equal(t, StatusError, health.OverallStatus, phase)
					require.Len(t, health.Errors, tc.wantErrors)
					for _, issue := range health.Errors {
						require.Contains(t, issue, string(taint))
						require.Contains(t, issue, tc.issue)
					}
				}

				if previouslyHealthy {
					// Recovery needs only the failed source to refresh: the other source's
					// previously healthy condition remains valid under the carry-forward policy.
					recovered := newStatus(metav1.NewTime(now.Add(2 * time.Second)))
					var configured map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation
					var scripts map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation
					if tc.configured != nil {
						configured = alertAgentStatusesFromObserved(observedAll())
					} else {
						scripts = goodScripts
					}
					applyAlertAgentStatus(recovered, status, configured, scripts, recovered.LastUpdated, true)
					require.Equal(t, metav1.ConditionTrue, getConditionStatus(recovered.Conditions, pacmkrv1.ClusterHealthyConditionType))
					health := BuildHealthStatusFromCR(&pacmkrv1.PacemakerCluster{Status: *recovered})
					require.Equal(t, StatusHealthy, health.OverallStatus)
					require.Empty(t, health.Errors)
				}
			})
		}
	}
}

func TestApplyAlertAgentStatus_FeatureGateControlsCarryForwardAndHealth(t *testing.T) {
	now := metav1.Now()
	base := healthyClusterStatus(now, []pacmkrv1.PacemakerClusterNodeStatus{
		{NodeName: "master-0", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
		{NodeName: "master-1", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
	})
	existing := base.DeepCopy()
	freshAgents := alertAgentStatusesFromObserved(nil) // Both agents are missing from the CIB.
	freshScripts := map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
		"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
		"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
	}
	applyAlertAgentStatus(existing, nil, freshAgents, freshScripts, now, true)
	require.Equal(t, metav1.ConditionFalse, getConditionStatus(existing.Conditions, pacmkrv1.ClusterHealthyConditionType))

	for _, enabled := range []bool{false, true} {
		for _, observed := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/observed=%t", enabled, observed), func(t *testing.T) {
				status := base.DeepCopy()
				if observed {
					applyAlertAgentStatus(status, existing, freshAgents, freshScripts, now, enabled)
				} else {
					applyAlertAgentStatus(status, existing, nil, nil, now, enabled)
				}
				if !enabled {
					require.Equal(t, base, status, "disabled gate must omit agent data and leave base health unchanged, including when old status has failures")
					return
				}
				require.Equal(t, metav1.ConditionFalse, getConditionStatus(status.Conditions, pacmkrv1.ClusterHealthyConditionType))
				for _, node := range *status.Nodes {
					require.Len(t, node.AlertAgents, 2, "enabled gate must retain both entries even without a fresh observation")
					require.Equal(t, metav1.ConditionFalse, getConditionStatus(node.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType))
				}
			})
		}
	}
}
