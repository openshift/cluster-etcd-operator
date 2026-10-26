package tlssecurityprofile

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
)

func TestObserveTLSGroups(t *testing.T) {
	intermediate := groupNames(configv1.TLSProfiles[configv1.TLSProfileIntermediateType])
	disabled := featuregates.NewHardcodedFeatureGateAccess(nil, []configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences})
	enabled := featuregates.NewHardcodedFeatureGateAccess([]configv1.FeatureGateName{features.FeatureGateTLSGroupPreferences}, nil)

	tests := []struct {
		name          string
		accessor      featuregates.FeatureGateAccess
		apiServer     *configv1.APIServer
		existing      map[string]any
		wantGroups    []string
		wantGroupsKey bool
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
			name:          "gate enabled with no APIServer uses intermediate groups",
			accessor:      enabled,
			wantGroups:    intermediate,
			wantGroupsKey: true,
		},
		{
			name:     "gate enabled uses custom groups",
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
			wantGroups:    []string{"X25519", "secp256r1"},
			wantGroupsKey: true,
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
			wantGroups:    []string{},
			wantGroupsKey: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listers := configobservation.Listers{
				APIServerLister_: newAPIServerLister(t, tt.apiServer),
			}
			recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
			got, errs := ObserveTLSGroups(tt.accessor)(listers, recorder, tt.existing)
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			gotGroups, found, err := unstructured.NestedStringSlice(got, groupsPath...)
			if err != nil {
				t.Fatal(err)
			}
			if found != tt.wantGroupsKey {
				t.Fatalf("groups key found=%v, want %v, config=%v", found, tt.wantGroupsKey, got)
			}
			if diff := cmp.Diff(tt.wantGroups, gotGroups); diff != "" {
				t.Fatalf("groups mismatch (-want +got):\n%s", diff)
			}
		})
	}
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
