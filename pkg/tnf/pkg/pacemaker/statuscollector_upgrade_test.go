package pacemaker

import (
	"context"
	"encoding/json"
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
			err = updatePacemakerStatusCRWithClient(context.Background(), client, base, freshAgents, freshScripts, tt.enabled)
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
