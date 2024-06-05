package startupmonitorcondition

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestStartupMonitorPodConditionController(t *testing.T) {
	scenarios := []struct {
		name               string
		initialObjects     []runtime.Object
		initialNodeStatus  []operatorv1.NodeStatus
		previousConditions []operatorv1.OperatorCondition
		expectedConditions []operatorv1.OperatorCondition
	}{
		{
			name:           "scenario 1: happy path",
			initialObjects: []runtime.Object{newPod(corev1.PodRunning, "kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal")},
			expectedConditions: []operatorv1.OperatorCondition{
				{Type: "StartupMonitorPodDegraded", Status: operatorv1.ConditionFalse},
				{Type: "StartupMonitorPodContainerExcessiveRestartsDegraded", Status: operatorv1.ConditionFalse},
			},
			initialNodeStatus: []operatorv1.NodeStatus{{NodeName: "ip-10-0-129-56.ec2.internal", TargetRevision: 2}},
		},

		{
			name: "scenario 2: degraded in pending phase",
			initialObjects: []runtime.Object{
				func() *corev1.Pod {
					p := newPod(corev1.PodPending, "kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal")
					startTime := metav1.NewTime(time.Now().Add(-3 * time.Minute))
					p.Status.StartTime = &startTime
					p.Status.Reason = "PendingReason"
					p.Status.Message = "PendingMessage"
					return p
				}(),
			},
			initialNodeStatus: []operatorv1.NodeStatus{{NodeName: "ip-10-0-129-56.ec2.internal", TargetRevision: 2}},
			expectedConditions: []operatorv1.OperatorCondition{
				{Type: "StartupMonitorPodDegraded", Status: operatorv1.ConditionTrue, Reason: "PendingReason", Message: "the pod kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal has been in Pending phase for more than max tolerated time \\(2m0s\\) due to PendingMessage"},
				{Type: "StartupMonitorPodContainerExcessiveRestartsDegraded", Status: operatorv1.ConditionFalse},
			},
		},

		{
			name: "scenario 3: degraded container in pending phase",
			initialObjects: []runtime.Object{
				func() *corev1.Pod {
					p := newPod(corev1.PodPending, "kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal")
					startTime := metav1.NewTime(time.Now().Add(-3 * time.Minute))
					p.Status.StartTime = &startTime
					p.Status.Reason = "PendingReason"
					p.Status.Message = "PendingMessage"
					p.Status.ContainerStatuses = []corev1.ContainerStatus{
						{
							Name:  "WaitingContainerName",
							State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerWaitingReason", Message: "ContainerWaitingMessage"}},
						},
					}
					return p
				}(),
			},
			initialNodeStatus: []operatorv1.NodeStatus{{NodeName: "ip-10-0-129-56.ec2.internal", TargetRevision: 2}},
			expectedConditions: []operatorv1.OperatorCondition{
				{Type: "StartupMonitorPodDegraded", Status: operatorv1.ConditionTrue, Reason: "ContainerWaitingReason", Message: "the pod kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal has been in Pending phase for more than max tolerated time \\(2m0s\\) due to PendingMessage\nat least one container WaitingContainerName is waiting since .* due to ContainerWaitingMessage"},
				{Type: "StartupMonitorPodContainerExcessiveRestartsDegraded", Status: operatorv1.ConditionFalse},
			},
		},

		{
			name: "scenario 4: degraded failed container",
			initialObjects: []runtime.Object{
				func() *corev1.Pod {
					p := newPod(corev1.PodFailed, "kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal")
					p.Status.Reason = "FailedReason"
					p.Status.Message = "FailedMessage"
					p.Status.ContainerStatuses = []corev1.ContainerStatus{
						{
							Name:  "TerminatedContainerName",
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "TerminatedContainerReason", Message: "TerminatedContainerMessage", ExitCode: 255}},
						},
					}
					return p
				}(),
			},
			initialNodeStatus: []operatorv1.NodeStatus{{NodeName: "ip-10-0-129-56.ec2.internal", TargetRevision: 2}},
			expectedConditions: []operatorv1.OperatorCondition{
				{Type: "StartupMonitorPodDegraded", Status: operatorv1.ConditionTrue, Reason: "TerminatedContainerReason", Message: "at least one container TerminatedContainerName in kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal pod exited with 255 \\(expected nonzero exit code\\), due to TerminatedContainerMessage"},
				{Type: "StartupMonitorPodContainerExcessiveRestartsDegraded", Status: operatorv1.ConditionFalse},
			},
		},

		{
			name: "scenario 5: degraded excessive restarts",
			initialObjects: []runtime.Object{
				func() *corev1.Pod {
					p := newPod(corev1.PodRunning, "kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal")
					p.Status.Reason = "FailedReason"
					p.Status.Message = "FailedMessage"
					p.Status.ContainerStatuses = []corev1.ContainerStatus{
						{
							Name:         "RestartingContainerName",
							RestartCount: 4,
						},
					}
					return p
				}(),
			},
			expectedConditions: []operatorv1.OperatorCondition{
				{Type: "StartupMonitorPodDegraded", Status: operatorv1.ConditionFalse},
				{Type: "StartupMonitorPodContainerExcessiveRestartsDegraded", Status: operatorv1.ConditionTrue, Reason: "ExcessiveRestarts", Message: "at least one container RestartingContainerName in kube-apiserver-startup-monitor-ip-10-0-129-56.ec2.internal pod has restarted 4 times, max allowed is 2"},
			},
			initialNodeStatus: []operatorv1.NodeStatus{{NodeName: "ip-10-0-129-56.ec2.internal", TargetRevision: 2}},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{},
				&operatorv1.StaticPodOperatorStatus{
					NodeStatuses: scenario.initialNodeStatus,
					OperatorStatus: operatorv1.OperatorStatus{
						Conditions: scenario.previousConditions,
					}},
				nil,
				nil,
			)
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.initialObjects {
				if err := indexer.Add(obj); err != nil {
					t.Error(err)
				}
			}

			// act
			target := &startupMonitorPodConditionController{
				podLister:      corev1listers.NewPodLister(indexer).Pods("openshift-kube-apiserver"),
				operatorClient: fakeOperatorClient,
				targetName:     "kube-apiserver",
				startupMonitorEnabledFn: func() (bool, error) {
					return true, nil
				},
			}

			// validate
			err := target.sync(nil, nil)
			if err != nil {
				t.Error(err)
			}

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

func newPod(phase corev1.PodPhase, name string) *corev1.Pod {
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "openshift-kube-apiserver",
			Annotations: map[string]string{},
			Labels:      map[string]string{}},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}

	return &pod
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

		if len(expectedCondition.Message) == 0 && len(actualCondition.Message) > 0 {
			return fmt.Errorf("expected condition message regexp wasn't provided")
		}
		if matched, err := regexp.MatchString(expectedCondition.Message, actualCondition.Message); err != nil || !matched {
			return fmt.Errorf("expectedCondition.Message regexp mismatch, actual = %s, regexp = %s", actualCondition.Message, expectedCondition.Message)
		}
		// just rewrite if it matches the regexp
		expectedCondition.Message = actualCondition.Message

		if !equality.Semantic.DeepEqual(actualCondition, expectedCondition) {
			return fmt.Errorf("conditions mismatch, diff = %s", diff.ObjectDiff(actualCondition, expectedCondition))
		}
	}
	return nil
}
