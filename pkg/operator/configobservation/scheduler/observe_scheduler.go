package scheduler

import (
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/klog"
)

// ObserveDefaultNodeSelector reads the defaultNodeSelector from the scheduler configuration instance cluster
func ObserveDefaultNodeSelector(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	listers := genericListers.(configobservation.Listers)
	errs := []error{}
	prevObservedConfig := map[string]interface{}{}

	defaultNodeSelectorPath := []string{"projectConfig", "defaultNodeSelector"}
	currentdefaultNodeSelector, _, err := unstructured.NestedString(existingConfig, defaultNodeSelectorPath...)
	if err != nil {
		return prevObservedConfig, append(errs, err)
	}
	if len(currentdefaultNodeSelector) > 0 {
		if err := unstructured.SetNestedField(prevObservedConfig, currentdefaultNodeSelector, defaultNodeSelectorPath...); err != nil {
			errs = append(errs, err)
		}
	}

	observedConfig := map[string]interface{}{}
	schedulerConfig, err := listers.SchedulerLister.Get("cluster")
	if errors.IsNotFound(err) {
		klog.Warningf("scheduler.config.openshift.io/cluster: not found")
		return observedConfig, errs
	}
	if err != nil {
		return prevObservedConfig, errs
	}

	defaultNodeSelector := schedulerConfig.Spec.DefaultNodeSelector
	if len(defaultNodeSelector) > 0 {
		if err := unstructured.SetNestedField(observedConfig, defaultNodeSelector, defaultNodeSelectorPath...); err != nil {
			errs = append(errs, err)
		}
		if defaultNodeSelector != currentdefaultNodeSelector {
			recorder.Eventf("ObserveDefaultNodeSelectorChanged", "default node selector changed to %q", defaultNodeSelector)
		}
	}
	return observedConfig, errs
}
