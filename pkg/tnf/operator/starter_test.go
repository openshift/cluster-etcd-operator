package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"sigs.k8s.io/yaml"
)

func TestTNFObservabilityFeatureGate(t *testing.T) {
	gate := features.FeatureGateTNFObservability
	for _, tt := range []struct {
		name     string
		enabled  []configv1.FeatureGateName
		disabled []configv1.FeatureGateName
		want     bool
	}{
		{name: "older payload does not know gate"},
		{name: "legacy alert-agent gate does not enable observability", enabled: []configv1.FeatureGateName{"PacemakerAlertAgentStatus"}},
		{name: "disabled", disabled: []configv1.FeatureGateName{gate}},
		{name: "enabled", enabled: []configv1.FeatureGateName{gate}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enabled := pacemaker.AlertAgentStatusEnabled(featuregates.NewFeatureGate(tt.enabled, tt.disabled))
			require.Equal(t, tt.want, enabled)

			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(enabled)), &crd))
			require.Equal(t, "pacemakerclusters.etcd.openshift.io", crd.Name)
			require.Len(t, crd.Spec.Versions, 1)
			require.Equal(t, "v1", crd.Spec.Versions[0].Name)
			node := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["nodes"].Items.Schema
			_, hasAgents := node.Properties["alertAgents"]
			require.Equal(t, tt.want, hasAgents, "schema must match the collector's gate")
			retainsAgents := false
			for _, validation := range node.XValidations {
				if validation.Rule == "!has(oldSelf.alertAgents) || has(self.alertAgents)" {
					retainsAgents = true
				}
			}
			require.Equal(t, tt.want, retainsAgents, "retention validation must also be gated")
		})
	}
}

func TestPacemakerCRDMatchesAPIFeatureSets(t *testing.T) {
	for _, profile := range []struct {
		name string
		api  features.ClusterProfileName
	}{
		{name: "SelfManagedHA", api: features.SelfManaged},
		{name: "Hypershift", api: features.Hypershift},
	} {
		for _, featureSet := range []configv1.FeatureSet{configv1.Default, configv1.OKD, configv1.TechPreviewNoUpgrade, configv1.DevPreviewNoUpgrade} {
			name := string(featureSet)
			if name == "" {
				name = "Default"
			}
			t.Run(profile.name+"/"+name, func(t *testing.T) {
				resolved := features.FeatureSets(5, profile.api, featureSet)
				var enabled, disabled []configv1.FeatureGateName
				for _, gate := range resolved.Enabled {
					enabled = append(enabled, gate.FeatureGateAttributes.Name)
				}
				for _, gate := range resolved.Disabled {
					disabled = append(disabled, gate.FeatureGateAttributes.Name)
				}
				observability := pacemaker.AlertAgentStatusEnabled(featuregates.NewFeatureGate(enabled, disabled))
				want := profile.api == features.SelfManaged && (featureSet == configv1.TechPreviewNoUpgrade || featureSet == configv1.DevPreviewNoUpgrade)
				require.Equal(t, want, observability)

				source := filepath.Join("..", "..", "..", "vendor", "github.com", "openshift", "api", "etcd", "v1", "zz_generated.crd-manifests",
					fmt.Sprintf("0000_25_etcd_01_pacemakerclusters-%s-%s.crd.yaml", profile.name, name))
				data, err := os.ReadFile(source)
				require.NoError(t, err)
				var upstream, embedded apiextensionsv1.CustomResourceDefinition
				require.NoError(t, yaml.Unmarshal(data, &upstream))
				require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(observability)), &embedded))
				require.Equal(t, upstream.Spec, embedded.Spec, "selected schema must match the API profile and feature set")
			})
		}
	}
}

func TestPacemakerCRDReadyDuringUpgrade(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(enabled)), &crd))
		require.False(t, pacemakerCRDReady(&crd, enabled), "schema must be established first")
		crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{
			Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
		}}
		require.True(t, pacemakerCRDReady(&crd, enabled))
		require.False(t, pacemakerCRDReady(&crd, !enabled), "old established schema must not start collectors for the new gate state")
		crd.Spec.Versions[0].Served = false
		require.False(t, pacemakerCRDReady(&crd, enabled), "v1 must be served")
		crd.Spec.Versions[0].Served = true
		crd.Spec.Versions[0].Schema = nil
		require.False(t, pacemakerCRDReady(&crd, enabled), "missing schema must wait rather than panic")
	}
}

type crdRoundTripper func(*http.Request) (*http.Response, error)

func (f crdRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPacemakerCRDReconcilesFeatureGateChanges(t *testing.T) {
	for _, initiallyEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("initiallyEnabled=%t", initiallyEnabled), func(t *testing.T) {
			ctx := context.Background()
			var stored *apiextensionsv1.CustomResourceDefinition
			var writes []string
			client, err := apiextensionsclient.NewForConfig(&rest.Config{
				Host:          "https://crd-upgrade-test.invalid",
				ContentConfig: rest.ContentConfig{ContentType: "application/json"},
				Transport: crdRoundTripper(func(req *http.Request) (*http.Response, error) {
					code := http.StatusOK
					var body any
					switch req.Method {
					case http.MethodGet:
						if stored == nil {
							code = http.StatusNotFound
							body = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
						} else {
							body = stored
						}
					case http.MethodPost, http.MethodPut:
						require.NoError(t, json.NewDecoder(req.Body).Decode(&stored))
						writes = append(writes, req.Method)
						body = stored
					default:
						t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
					}
					data, err := json.Marshal(body)
					require.NoError(t, err)
					return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
				}),
			})
			require.NoError(t, err)
			holder := (&resourceapply.ClientHolder{}).WithAPIExtensionsClient(client)
			recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
			// Exercise the same asset application used by the static-resource controller:
			// install, change the gate on upgrade/restart, then change it back.
			for _, enabled := range []bool{initiallyEnabled, !initiallyEnabled, initiallyEnabled} {
				results := resourceapply.ApplyDirectly(ctx, holder, recorder, resourceapply.NewResourceCache(), bindata.Asset, pacemakerCRDAsset(enabled))
				require.Len(t, results, 1)
				require.NoError(t, results[0].Error)
				require.True(t, results[0].Changed)
				actual, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "pacemakerclusters.etcd.openshift.io", metav1.GetOptions{})
				require.NoError(t, err)
				var expected apiextensionsv1.CustomResourceDefinition
				require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(enabled)), &expected))
				require.Equal(t, expected.Spec.Versions, actual.Spec.Versions, "replace the schema, including alert-agent fields and retention validation")
				actual.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}
				require.True(t, pacemakerCRDReady(actual, enabled))
				require.False(t, pacemakerCRDReady(actual, !enabled))
			}
			require.Equal(t, []string{http.MethodPost, http.MethodPut, http.MethodPut}, writes, "existing CRDs must be updated in place")
		})
	}
}

func TestPacemakerLifecycleManagerStartupCondition(t *testing.T) {
	client := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{}, nil, nil)
	ctx := context.Background()

	require.NoError(t, setPacemakerLifecycleManagerStartupCondition(ctx, client, errors.New("informer setup failed")))
	_, status, _, err := client.GetStaticPodOperatorState()
	require.NoError(t, err)
	condition := v1helpers.FindOperatorCondition(status.Conditions, conditionTypePacemakerLifecycleManagerDegraded)
	require.NotNil(t, condition)
	require.Equal(t, operatorv1.ConditionTrue, condition.Status)
	require.Contains(t, condition.Message, "informer setup failed")

	require.NoError(t, setPacemakerLifecycleManagerStartupCondition(ctx, client, nil))
	_, status, _, err = client.GetStaticPodOperatorState()
	require.NoError(t, err)
	condition = v1helpers.FindOperatorCondition(status.Conditions, conditionTypePacemakerLifecycleManagerDegraded)
	require.NotNil(t, condition)
	require.Equal(t, operatorv1.ConditionFalse, condition.Status)
}
