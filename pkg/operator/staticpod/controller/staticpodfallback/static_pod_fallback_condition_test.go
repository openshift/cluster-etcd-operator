package staticpodfallback

import (
	"fmt"
	"sort"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1listers "k8s.io/client-go/listers/core/v1"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/client-go/tools/cache"
)

func TestStaticPodFallbackConditionController(t *testing.T) {
	scenarios := []struct {
		name               string
		initialObjects     []runtime.Object
		previousConditions []operatorv1.OperatorCondition
		expectedConditions []operatorv1.OperatorCondition
	}{
		{
			name:           "scenario 1: happy path",
			initialObjects: []runtime.Object{newPod(corev1.PodRunning, corev1.ConditionTrue, "3", "kas")},
			expectedConditions: []operatorv1.OperatorCondition{
				{
					Type:   "StaticPodFallbackRevisionDegraded",
					Status: operatorv1.ConditionFalse,
					Reason: "",
				},
			},
		},

		{
			name: "scenario 2: fallback detected, degraded condition set",
			initialObjects: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod(corev1.PodRunning, corev1.ConditionTrue, "3", "kas")
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-for-revision"] = "3"
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-reason"] = "SomeReason"
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-message"] = "SomeMsg"
					return pod
				}(),
			},
			expectedConditions: []operatorv1.OperatorCondition{
				{
					Type:    "StaticPodFallbackRevisionDegraded",
					Status:  operatorv1.ConditionTrue,
					Reason:  "SomeReason",
					Message: fmt.Sprintf("a static pod %v was rolled back to revision %v due to %v", "kas", "3", "SomeMsg"),
				},
			},
		},

		{
			name: "scenario 3: fallback detected, degraded condition set, multiple pods",
			initialObjects: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod(corev1.PodRunning, corev1.ConditionTrue, "3", "kas")
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-for-revision"] = "3"
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-reason"] = "SomeReason"
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-message"] = "SomeMsg"
					return pod
				}(),
				func() *corev1.Pod {
					pod := newPod(corev1.PodRunning, corev1.ConditionTrue, "3", "kas-1")
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-for-revision"] = "3"
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-reason"] = "DifferentReason"
					pod.Annotations["startup-monitor.static-pods.openshift.io/fallback-message"] = "DifferentMsg"
					return pod
				}(),
			},
			expectedConditions: []operatorv1.OperatorCondition{
				{
					Type:    "StaticPodFallbackRevisionDegraded",
					Status:  operatorv1.ConditionTrue,
					Reason:  "SomeReason",
					Message: fmt.Sprintf("%s\n%s", "a static pod kas was rolled back to revision 3 due to SomeMsg", "a static pod kas-1 was rolled back to revision 3 due to DifferentMsg"),
				},
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeOperatorClient := v1helpers.NewFakeOperatorClient(
				nil,
				&operatorv1.OperatorStatus{
					Conditions: scenario.previousConditions,
				},
				nil,
			)
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.initialObjects {
				if err := indexer.Add(obj); err != nil {
					t.Error(err)
				}
			}

			// act
			target := &staticPodFallbackConditionController{
				podLister:        orderedPodNamespaceLister{corev1listers.NewPodLister(indexer).Pods("openshift-kube-apiserver")},
				operatorClient:   fakeOperatorClient,
				podLabelSelector: labels.Set{"apiserver": "true"}.AsSelector(),
				startupMonitorEnabledFn: func() (bool, error) {
					return true, nil
				},
			}

			err := target.sync(nil, nil)
			if err != nil {
				t.Error(err)
			}

			// validate
			_, actualOperatorStatus, _, err := fakeOperatorClient.GetOperatorState()
			if err != nil {
				t.Fatal(err)
			}
			if err := areCondidtionsEqual(scenario.expectedConditions, actualOperatorStatus.Conditions); err != nil {
				t.Error(err)
			}
		})
	}
}

func areCondidtionsEqual(expectedConditions []operatorv1.OperatorCondition, actualConditions []operatorv1.OperatorCondition) error {
	if len(expectedConditions) != len(actualConditions) {
		return fmt.Errorf("expected %d conditions but got %d", len(expectedConditions), len(actualConditions))
	}
	for _, expectedCondition := range expectedConditions {
		actualConditionPtr := v1helpers.FindOperatorCondition(actualConditions, expectedCondition.Type)
		if actualConditionPtr == nil {
			return fmt.Errorf("%q condition hasn't been found", expectedCondition.Type)
		}
		// we don't care about the last transition time
		actualConditionPtr.LastTransitionTime = metav1.Time{}
		// so that we don't compare ref vs value types
		actualCondition := *actualConditionPtr
		if !equality.Semantic.DeepEqual(actualCondition, expectedCondition) {
			return fmt.Errorf("conditions mismatch, diff = %s", diff.ObjectDiff(actualCondition, expectedCondition))
		}
	}
	return nil
}

func newPod(phase corev1.PodPhase, ready corev1.ConditionStatus, revision, name string) *corev1.Pod {
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "openshift-kube-apiserver",
			Annotations: map[string]string{},
			Labels: map[string]string{
				"revision":  revision,
				"apiserver": "true",
			}},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: ready,
			}},
		},
	}

	return &pod
}

// we need a deterministic pod lister for tests
// otherwise the order of the pods will yield different results
type orderedPodNamespaceLister struct {
	podLister corev1listers.PodNamespaceLister
}

func (s orderedPodNamespaceLister) List(selector labels.Selector) (ret []*v1.Pod, err error) {
	pods, err := s.podLister.List(selector)
	if err != nil {
		return nil, err
	}
	// sort pods by name
	sort.Sort(ascendingName(pods))
	return pods, nil
}

func (s orderedPodNamespaceLister) Get(name string) (*v1.Pod, error) {
	return s.podLister.Get(name)
}

// is a sort.Interface that Sorts a list of Pods based on the names of the Pod
type ascendingName []*v1.Pod

func (as ascendingName) Len() int {
	return len(as)
}

func (as ascendingName) Swap(i, j int) {
	as[i], as[j] = as[j], as[i]
}

func (as ascendingName) Less(i, j int) bool {
	return as[i].Name < as[j].Name
}
