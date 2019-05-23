package network

import (
	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/events"
)

func TestObserveClusterConfig(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(&configv1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: configv1.NetworkStatus{
			ClusterNetwork: []configv1.ClusterNetworkEntry{{CIDR: "podCIDR"}},
			ServiceNetwork: []string{"serviceCIDR"},
		},
	}); err != nil {
		t.Fatal(err.Error())
	}
	listers := configobservation.Listers{
		NetworkLister: configlistersv1.NewNetworkLister(indexer),
	}
	result, errors := ObserveRestrictedCIDRs(listers, events.NewInMemoryRecorder("network"), map[string]interface{}{})
	if len(errors) > 0 {
		t.Error("expected len(errors) == 0")
	}
	restrictedCIDRs, _, err := unstructured.NestedSlice(result, "admissionPluginConfig", "network.openshift.io/RestrictedEndpointsAdmission", "configuration", "restrictedCIDRs")
	if err != nil {
		t.Fatal(err)
	}
	if restrictedCIDRs[0] != "podCIDR" {
		t.Error(restrictedCIDRs[0])
	}
	if restrictedCIDRs[1] != "serviceCIDR" {
		t.Error(restrictedCIDRs[1])
	}
}
