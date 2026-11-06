package pacemaker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

type collectorRoundTripper func(*http.Request) (*http.Response, error)

func (f collectorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCollectorStatusWritesDuringUpgrade(t *testing.T) {
	for _, tt := range []struct {
		name           string
		create         bool
		enabled        bool
		oldSchema      bool
		newerCollector bool
	}{
		{name: "older payload keeps base status"},
		{name: "enabled feature adds entries to existing pre-feature status", enabled: true},
		{name: "enabled feature initializes a new resource", create: true, enabled: true},
		{name: "old schema rejects new fields without accepting aggregate health", enabled: true, oldSchema: true},
		{name: "old schema rejects initialization until ready", create: true, enabled: true, oldSchema: true},
		{name: "late older collection cannot overwrite a newer collection", enabled: true, newerCollector: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.Now()
			base := healthyClusterStatus(now, []pacmkrv1.PacemakerClusterNodeStatus{
				{NodeName: "master-0", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
				{NodeName: "master-1", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
			})
			existing := &pacmkrv1.PacemakerCluster{
				TypeMeta:   metav1.TypeMeta{APIVersion: pacmkrv1.GroupVersion.String(), Kind: "PacemakerCluster"},
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", ResourceVersion: "42"},
				Status:     *base.DeepCopy(),
			}
			existing.Status.LastUpdated = metav1.NewTime(now.Add(-time.Minute))
			if tt.newerCollector {
				existing.Status.LastUpdated = metav1.NewTime(now.Add(time.Minute))
			}
			writes, acceptedStatus := 0, false
			respond := func(code int, body any) (*http.Response, error) {
				data, err := json.Marshal(body)
				require.NoError(t, err)
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			}
			client, err := CreatePacemakerRESTClient(&rest.Config{
				Host: "https://collector-upgrade-test.invalid",
				Transport: collectorRoundTripper(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet {
						if tt.create {
							return respond(http.StatusNotFound, metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
						}
						return respond(http.StatusOK, existing)
					}
					writes++
					require.Equal(t, metav1.FieldValidationStrict, req.URL.Query().Get("fieldValidation"))
					var submitted pacmkrv1.PacemakerCluster
					require.NoError(t, json.NewDecoder(req.Body).Decode(&submitted))
					for _, node := range *submitted.Status.Nodes {
						if tt.enabled {
							require.Len(t, node.AlertAgents, 2)
						} else {
							require.Empty(t, node.AlertAgents)
						}
					}
					if tt.oldSchema {
						return respond(http.StatusBadRequest, metav1.Status{
							TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure,
							Reason: metav1.StatusReasonBadRequest, Code: http.StatusBadRequest,
							Message: "strict decoding error: unknown field status.nodes[0].alertAgents",
						})
					}
					if req.Method == http.MethodPost {
						// Creation strips status; the collector must follow with a strict status PUT.
						return respond(http.StatusCreated, &pacmkrv1.PacemakerCluster{TypeMeta: existing.TypeMeta, ObjectMeta: existing.ObjectMeta})
					}
					require.Equal(t, http.MethodPut, req.Method)
					require.True(t, strings.HasSuffix(req.URL.Path, "/status"))
					require.Equal(t, "42", submitted.ResourceVersion)
					acceptedStatus = true
					return respond(http.StatusOK, &submitted)
				}),
			})
			require.NoError(t, err)
			// Missing CIB registrations create real unhealthy observations, so rejecting
			// an older schema must also prevent acceptance of their aggregate health.
			freshAgents := alertAgentStatusesFromObserved(nil)
			freshScripts := map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
				"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
				"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
			}
			gate := alertAgentGateDisabled
			if tt.enabled {
				gate = alertAgentGateEnabled
			}
			err = updatePacemakerStatusCRWithClient(context.Background(), client, base, freshAgents, freshScripts, gate)
			if tt.oldSchema {
				require.ErrorContains(t, err, "unknown field")
				require.False(t, acceptedStatus)
				return
			}
			require.NoError(t, err)
			if tt.newerCollector {
				require.Zero(t, writes)
				require.False(t, acceptedStatus)
				return
			}
			require.True(t, acceptedStatus)
			if tt.create {
				require.Equal(t, 2, writes)
			} else {
				require.Equal(t, 1, writes)
			}
		})
	}
}

func TestCollectorBaseStatusFallback(t *testing.T) {
	for _, tt := range []struct {
		name              string
		missing           bool
		previouslyEnabled bool
		agentsUnhealthy   bool
		baseUnhealthy     bool
		readFails         bool
		newerCollector    bool
		wantErr           string
	}{
		{name: "existing base status continues"},
		{name: "missing CR can be initialized", missing: true},
		{name: "previous healthy alert-agent status is retained", previouslyEnabled: true},
		{name: "previous unhealthy alert-agent status still affects health", previouslyEnabled: true, agentsUnhealthy: true},
		{name: "new base failures still affect health", previouslyEnabled: true, baseUnhealthy: true},
		{name: "unreadable CR must not be overwritten", readFails: true, wantErr: "get PacemakerCluster"},
		{name: "late unresolved collection cannot overwrite a newer collection", previouslyEnabled: true, newerCollector: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now().Truncate(time.Second))
			status := healthyClusterStatus(now, []pacmkrv1.PacemakerClusterNodeStatus{
				{NodeName: "master-0", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
				{NodeName: "master-1", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
			})
			existing := &pacmkrv1.PacemakerCluster{
				TypeMeta:   metav1.TypeMeta{APIVersion: pacmkrv1.GroupVersion.String(), Kind: "PacemakerCluster"},
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", ResourceVersion: "42"},
				Status:     *status.DeepCopy(),
			}
			existing.Status.LastUpdated = metav1.NewTime(now.Add(-time.Hour))
			if tt.previouslyEnabled {
				observed := observedAll()
				if tt.agentsUnhealthy {
					delete(observed, "tnf-taint-alert")
				}
				applyAlertAgentStatus(&existing.Status, nil, alertAgentStatusesFromObserved(observed), map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
					"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
					"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
				}, existing.Status.LastUpdated, true)
			}
			if tt.newerCollector {
				existing.Status.LastUpdated = metav1.NewTime(now.Add(time.Minute))
			}
			if tt.baseUnhealthy {
				(*status.Nodes)[0].Conditions[0].Status = metav1.ConditionFalse
				(*status.Nodes)[0].Conditions[0].Message = "Etcd resource is unhealthy"
			}
			// Match prior observations by node name, not by the order returned by Pacemaker.
			(*status.Nodes)[0], (*status.Nodes)[1] = (*status.Nodes)[1], (*status.Nodes)[0]
			writes := 0
			var submitted pacmkrv1.PacemakerCluster
			client, err := CreatePacemakerRESTClient(&rest.Config{
				Host: "https://collector-fallback-test.invalid",
				Transport: collectorRoundTripper(func(req *http.Request) (*http.Response, error) {
					code, body := http.StatusOK, any(existing)
					if req.Method == http.MethodGet {
						if tt.readFails {
							return nil, fmt.Errorf("API unavailable")
						}
						if tt.missing {
							code = http.StatusNotFound
							body = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
						}
					} else {
						writes++
						require.Equal(t, metav1.FieldValidationStrict, req.URL.Query().Get("fieldValidation"))
						submitted = pacmkrv1.PacemakerCluster{}
						require.NoError(t, json.NewDecoder(req.Body).Decode(&submitted))
						if req.Method == http.MethodPost {
							code = http.StatusCreated
						}
					}
					data, err := json.Marshal(body)
					require.NoError(t, err)
					return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
				}),
			})
			require.NoError(t, err)

			// Even if observations are supplied accidentally, an unresolved gate must
			// ignore them rather than clear a prior failure or create new agent entries.
			err = updatePacemakerStatusCRWithClient(context.Background(), client, status, alertAgentStatusesFromObserved(observedAll()), map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
				"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
				"master-1": alertAgentScriptObservationsFromMCOState("Done", ""),
			}, alertAgentGateUnresolved)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Zero(t, writes)
				return
			}
			require.NoError(t, err)
			if tt.newerCollector {
				require.Zero(t, writes)
				return
			}
			require.Positive(t, writes)
			require.Equal(t, now, submitted.Status.LastUpdated)
			for _, node := range *submitted.Status.Nodes {
				if !tt.previouslyEnabled {
					require.Empty(t, node.AlertAgents)
					require.Nil(t, FindCondition(node.Conditions, nodeAlertAgentsObservedConditionType))
					continue
				}
				for _, previous := range *existing.Status.Nodes {
					if previous.NodeName != node.NodeName {
						continue
					}
					require.Equal(t, previous.AlertAgents, node.AlertAgents, "retain all agent conditions and their transition times")
					require.Equal(t, FindCondition(previous.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType), FindCondition(node.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType))
				}
				freshness := FindCondition(node.Conditions, nodeAlertAgentsObservedConditionType)
				require.NotNil(t, freshness)
				require.Equal(t, metav1.ConditionFalse, freshness.Status)
				require.Equal(t, "FeatureGateUnresolved", freshness.Reason)
				require.Equal(t, now, freshness.LastTransitionTime)
			}
			wantHealth, wantOverall := metav1.ConditionTrue, StatusHealthy
			if tt.agentsUnhealthy || tt.baseUnhealthy {
				wantHealth, wantOverall = metav1.ConditionFalse, StatusError
			}
			require.Equal(t, wantHealth, getConditionStatus(submitted.Status.Conditions, pacmkrv1.ClusterHealthyConditionType))
			require.Equal(t, wantOverall, BuildHealthStatusFromCR(&submitted, true).OverallStatus, "fresh base status must not be treated as stale")
			if !tt.previouslyEnabled {
				return
			}

			// A later unresolved collection retains the first freshness transition,
			// keeps a known failure, and does not invent entries for a newly seen node.
			existing = submitted.DeepCopy()
			nextTime := metav1.NewTime(now.Add(time.Minute))
			nextStatus := healthyClusterStatus(nextTime, []pacmkrv1.PacemakerClusterNodeStatus{
				{NodeName: "master-2", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
				{NodeName: "master-0", Conditions: []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}},
			})
			require.NoError(t, updatePacemakerStatusCRWithClient(context.Background(), client, nextStatus, nil, nil, alertAgentGateUnresolved))
			require.Equal(t, nextTime, submitted.Status.LastUpdated)
			require.Len(t, *submitted.Status.Nodes, 2, "do not recreate removed nodes")
			newNode, retainedNode := (*submitted.Status.Nodes)[0], (*submitted.Status.Nodes)[1]
			require.Empty(t, newNode.AlertAgents)
			require.Nil(t, FindCondition(newNode.Conditions, nodeAlertAgentsObservedConditionType))
			require.Equal(t, (*existing.Status.Nodes)[1].AlertAgents, retainedNode.AlertAgents)
			require.Equal(t, now, FindCondition(retainedNode.Conditions, nodeAlertAgentsObservedConditionType).LastTransitionTime)
			wantRetainedHealth := metav1.ConditionTrue
			if tt.agentsUnhealthy {
				wantRetainedHealth = metav1.ConditionFalse
			}
			require.Equal(t, wantRetainedHealth, getConditionStatus(submitted.Status.Conditions, pacmkrv1.ClusterHealthyConditionType))

			// Once the gate resolves enabled, fresh successful observations clear the
			// freshness signal and any retained agent failure, including on the new node.
			existing = submitted.DeepCopy()
			recovered := healthyClusterStatus(metav1.NewTime(nextTime.Add(time.Minute)), *nextStatus.Nodes)
			// Build a fresh base status, rather than reusing the merged node conditions.
			for i := range *recovered.Nodes {
				(*recovered.Nodes)[i].AlertAgents = nil
				(*recovered.Nodes)[i].Conditions = []metav1.Condition{{Type: pacmkrv1.NodeHealthyConditionType, Status: metav1.ConditionTrue}}
			}
			require.NoError(t, updatePacemakerStatusCRWithClient(context.Background(), client, recovered, alertAgentStatusesFromObserved(observedAll()), map[string]map[pacmkrv1.PacemakerClusterAlertAgentName]*alertAgentObservation{
				"master-0": alertAgentScriptObservationsFromMCOState("Done", ""),
				"master-2": alertAgentScriptObservationsFromMCOState("Done", ""),
			}, alertAgentGateEnabled))
			require.Equal(t, metav1.ConditionTrue, getConditionStatus(submitted.Status.Conditions, pacmkrv1.ClusterHealthyConditionType))
			for _, node := range *submitted.Status.Nodes {
				require.Len(t, node.AlertAgents, 2)
				require.Nil(t, FindCondition(node.Conditions, nodeAlertAgentsObservedConditionType))
				require.Equal(t, metav1.ConditionTrue, getConditionStatus(node.Conditions, pacmkrv1.NodeAlertAgentsHealthyConditionType))
			}
		})
	}
}
