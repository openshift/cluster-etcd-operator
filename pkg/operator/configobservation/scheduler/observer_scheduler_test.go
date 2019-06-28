package scheduler

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/events"
)

func TestObserveSchedulerConfig(t *testing.T) {
	nodeSelector := "type=user-node,region=east"
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(&configv1.Scheduler{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.SchedulerSpec{
			DefaultNodeSelector: nodeSelector,
		},
	}); err != nil {
		t.Fatal(err.Error())
	}
	listers := configobservation.Listers{
		SchedulerLister: configlistersv1.NewSchedulerLister(indexer),
	}
	result, errors := ObserveDefaultNodeSelector(listers, events.NewInMemoryRecorder("scheduler"), map[string]interface{}{})
	if len(errors) > 0 {
		t.Fatalf("expected len(errors) == 0")
	}
	observedSelector, _, err := unstructured.NestedString(result, "projectConfig", "defaultNodeSelector")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if observedSelector != nodeSelector {
		t.Fatalf("expected nodeselector to be %v but got %v", nodeSelector, observedSelector)
	}
}
