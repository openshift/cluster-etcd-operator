package pacemaker

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

func TestAlertAgentStatusEnabledForReleaseDuringUpgrade(t *testing.T) {
	gate := features.FeatureGateTNFObservability
	for _, tt := range []struct {
		name    string
		version string
		want    bool
	}{
		{name: "old Job retains old release without gate", version: "5.1.0"},
		{name: "new Job uses enabled gate", version: "5.2.0", want: true},
		{name: "disabled gate in later release", version: "5.3.0"},
		{name: "manual run uses desired release", want: true},
		{name: "development image uses desired release", version: "0.0.1-snapshot", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewSimpleClientset(
				&configv1.FeatureGate{
					ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
					Status: configv1.FeatureGateStatus{FeatureGates: []configv1.FeatureGateDetails{
						{Version: "5.2.0", Enabled: []configv1.FeatureGateAttributes{{Name: gate}}},
						{Version: "5.1.0"},
						{Version: "5.3.0", Disabled: []configv1.FeatureGateAttributes{{Name: gate}}},
					}},
				},
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: "version"},
					Status:     configv1.ClusterVersionStatus{Desired: configv1.Release{Version: "5.2.0"}},
				},
			)
			enabled, err := alertAgentStatusEnabledForRelease(context.Background(), client, tt.version)
			require.NoError(t, err)
			require.Equal(t, tt.want, enabled)
			repairs := 0
			reconcileAlertAgentsIfEnabled(context.Background(), enabled, func(ctx context.Context) error {
				repairs++
				require.NoError(t, ctx.Err())
				return nil
			})
			if tt.want {
				require.Equal(t, 1, repairs)
			} else {
				require.Zero(t, repairs, "disabled or unknown gates must not query or modify alert configuration")
			}
			if tt.version != "" && tt.version != "0.0.1-snapshot" {
				require.Len(t, client.Actions(), 1, "release Jobs must not consult the newer desired version")
				require.Equal(t, "featuregates", client.Actions()[0].GetResource().Resource)
			}
		})
	}
}

func TestAlertAgentStatusGateLookupErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version string
		objects []runtime.Object
		failGet bool
		wantErr string
	}{
		{name: "feature gate resource missing", version: "5.2.0", wantErr: "get cluster feature gates"},
		{name: "new release status not published yet", version: "5.2.0", objects: []runtime.Object{
			&configv1.FeatureGate{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: configv1.FeatureGateStatus{
				FeatureGates: []configv1.FeatureGateDetails{{Version: "5.1.0"}},
			}},
		}, wantErr: "missing desired version"},
		{name: "API unavailable", version: "5.2.0", failGet: true, wantErr: "API unavailable"},
		{name: "manual run cannot read cluster version", wantErr: "get cluster version"},
		{name: "manual run has no usable version", objects: []runtime.Object{
			&configv1.ClusterVersion{ObjectMeta: metav1.ObjectMeta{Name: "version"}},
		}, wantErr: "no release version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewSimpleClientset(tt.objects...)
			if tt.failGet {
				client.PrependReactor("get", "featuregates", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("API unavailable")
				})
			}
			_, err := alertAgentStatusEnabledForRelease(context.Background(), client, tt.version)
			require.ErrorContains(t, err, tt.wantErr, "lookup failure must not be treated as a disabled gate")
			for _, action := range client.Actions() {
				require.Equal(t, "get", action.GetVerb())
			}
		})
	}
}

func TestAlertAgentStatusGateManualRunUsesHistoryFallback(t *testing.T) {
	client := configfake.NewSimpleClientset(
		&configv1.ClusterVersion{ObjectMeta: metav1.ObjectMeta{Name: "version"}, Status: configv1.ClusterVersionStatus{
			History: []configv1.UpdateHistory{{Version: "5.2.0"}},
		}},
		&configv1.FeatureGate{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: configv1.FeatureGateStatus{
			FeatureGates: []configv1.FeatureGateDetails{{Version: "5.2.0", Enabled: []configv1.FeatureGateAttributes{
				{Name: features.FeatureGateTNFObservability},
			}}},
		}},
	)
	enabled, err := alertAgentStatusEnabledForRelease(context.Background(), client, "")
	require.NoError(t, err)
	require.True(t, enabled)
}
