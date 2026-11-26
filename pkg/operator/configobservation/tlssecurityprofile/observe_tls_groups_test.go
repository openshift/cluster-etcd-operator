package tlssecurityprofile

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
)

func TestObserveTLSSecurityProfile(t *testing.T) {
	intermediateGroups := configv1.TLSProfiles[configv1.TLSProfileIntermediateType].Groups
	intermediateCurvePrefs := tlsGroupsToCurvePreferences(intermediateGroups)
	disabled := featuregates.NewHardcodedFeatureGateAccess(nil, []configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences})
	enabled := featuregates.NewHardcodedFeatureGateAccess([]configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences}, nil)

	tests := []struct {
		name              string
		accessor          featuregates.FeatureGateAccess
		apiServer         *configv1.APIServer
		existing          map[string]any
		wantCurvePrefs    []int64
		wantCurvePrefsKey bool
	}{
		{
			name:     "gate disabled does not observe named-profile groups",
			accessor: disabled,
			apiServer: &configv1.APIServer{
				Spec: configv1.APIServerSpec{
					TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
				},
			},
		},
		{
			name:              "gate enabled with no APIServer uses intermediate curve preferences",
			accessor:          enabled,
			wantCurvePrefs:    intermediateCurvePrefs,
			wantCurvePrefsKey: true,
		},
		{
			name:     "gate enabled uses custom groups converted to curve preferences",
			accessor: enabled,
			apiServer: &configv1.APIServer{
				Spec: configv1.APIServerSpec{
					TLSSecurityProfile: &configv1.TLSSecurityProfile{
						Type: configv1.TLSProfileCustomType,
						Custom: &configv1.CustomTLSProfile{
							TLSProfileSpec: configv1.TLSProfileSpec{
								MinTLSVersion: configv1.VersionTLS12,
								Groups:        []configv1.TLSGroup{configv1.TLSGroupX25519, configv1.TLSGroupSecP256r1},
							},
						},
					},
				},
			},
			wantCurvePrefs:    tlsGroupsToCurvePreferences([]configv1.TLSGroup{configv1.TLSGroupX25519, configv1.TLSGroupSecP256r1}),
			wantCurvePrefsKey: true,
		},
		{
			name:     "gate enabled with custom groups omitted stores an empty list",
			accessor: enabled,
			apiServer: &configv1.APIServer{
				Spec: configv1.APIServerSpec{
					TLSSecurityProfile: &configv1.TLSSecurityProfile{
						Type: configv1.TLSProfileCustomType,
						Custom: &configv1.CustomTLSProfile{
							TLSProfileSpec: configv1.TLSProfileSpec{
								MinTLSVersion: configv1.VersionTLS12,
							},
						},
					},
				},
			},
			wantCurvePrefs:    []int64{},
			wantCurvePrefsKey: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listers := configobservation.Listers{
				APIServerLister_: newAPIServerLister(t, tt.apiServer),
			}
			recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
			got, errs := ObserveTLSSecurityProfile(tt.accessor)(listers, recorder, tt.existing)
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			gotCurvePrefsRaw, found, err := unstructured.NestedSlice(got, curvePreferencesPath...)
			if err != nil {
				t.Fatal(err)
			}
			if found != tt.wantCurvePrefsKey {
				t.Fatalf("curve preferences key found=%v, want %v, config=%v", found, tt.wantCurvePrefsKey, got)
			}

			// Convert []interface{} to []int64
			var gotCurvePrefs []int64
			if gotCurvePrefsRaw != nil {
				gotCurvePrefs = make([]int64, 0, len(gotCurvePrefsRaw))
				for _, cpRaw := range gotCurvePrefsRaw {
					switch v := cpRaw.(type) {
					case int64:
						gotCurvePrefs = append(gotCurvePrefs, v)
					case int:
						gotCurvePrefs = append(gotCurvePrefs, int64(v))
					default:
						t.Fatalf("unexpected curve preference type: %T", cpRaw)
					}
				}
			}

			if diff := cmp.Diff(tt.wantCurvePrefs, gotCurvePrefs); diff != "" {
				t.Fatalf("curve preferences mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func tlsGroupsToCurvePreferences(groups []configv1.TLSGroup) []int64 {
	curvePrefs32, _ := crypto.TLSGroupsToCurvePreferences(groups)
	curvePrefs := make([]int64, len(curvePrefs32))
	for i, cp := range curvePrefs32 {
		curvePrefs[i] = int64(cp)
	}
	return curvePrefs
}

func newAPIServerLister(t *testing.T, apiServer *configv1.APIServer) configlistersv1.APIServerLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if apiServer != nil {
		apiServer = apiServer.DeepCopy()
		apiServer.Name = "cluster"
		if err := indexer.Add(apiServer); err != nil {
			t.Fatal(err)
		}
	}
	return configlistersv1.NewAPIServerLister(indexer)
}
