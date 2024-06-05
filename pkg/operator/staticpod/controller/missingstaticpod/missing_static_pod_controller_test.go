package missingstaticpodcontroller

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

var emptyStaticPodPayloadYaml = ``

var validStaticPodPayloadYaml = `
apiVersion: v1
kind: Pod
spec:
  terminationGracePeriodSeconds: 194
`

var staticPodPayloadInvalidTypeYaml = `
apiVersion: v1
kind: Pod
spec:
  terminationGracePeriodSeconds: "194"
`

var staticPodPayloadNoTerminationGracePeriodYaml = `
apiVersion: v1
kind: Pod
spec:
  priorityClassName: system-node-critical
`

func TestMissingStaticPodControllerSync(t *testing.T) {
	var (
		// we need to use metav1.Now as the base for all the times since the sync loop
		// compare the termination timestamp with the threshold based on the elapsed
		// time.
		now = metav1.Now()

		targetNamespace = "test"
		operandName     = "test-operand"
	)

	testCases := []struct {
		name            string
		pods            []*corev1.Pod
		cms             []*corev1.ConfigMap
		expectSyncErr   bool
		expectedEvents  int
		isSNODeployment snoDeploymentFunc
	}{
		{
			name: "two terminated installer pods per node with correct revision",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeTerminatedInstallerPod(1, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeStaticPod(operandName, targetNamespace, "node-1", 2),
				makeStaticPod(operandName, targetNamespace, "node-2", 2),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 2, validStaticPodPayloadYaml),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "current revision older than the latest installer revision",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-3*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(3, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeStaticPod(operandName, targetNamespace, "node-1", 2),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 3, validStaticPodPayloadYaml),
			},
			expectSyncErr:   true,
			expectedEvents:  1,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "current revision older then the latest installer revision but termination time within threshold",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeTerminatedInstallerPod(3, "node-1", targetNamespace, 0, now),
				makeStaticPod(operandName, targetNamespace, "node-1", 2),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 3, validStaticPodPayloadYaml),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "only one node has a missing pod",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-3*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(3, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeTerminatedInstallerPod(1, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-3*time.Hour))),
				makeTerminatedInstallerPod(2, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(3, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeStaticPod(operandName, targetNamespace, "node-2", 3),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 2, validStaticPodPayloadYaml),
				makeOperandConfigMap(targetNamespace, operandName, 3, validStaticPodPayloadYaml),
			},
			expectSyncErr:   true,
			expectedEvents:  1,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "multiple missing pods for the same revision but on different nodes should produce multiple events",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-3*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(3, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeTerminatedInstallerPod(1, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-3*time.Hour))),
				makeTerminatedInstallerPod(2, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(3, "node-2", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 3, validStaticPodPayloadYaml),
			},
			expectSyncErr:   true,
			expectedEvents:  2,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "installer pod is still running",
			pods: []*corev1.Pod{
				makeInstallerPod(2, "node-1", targetNamespace),
				makeStaticPod(operandName, targetNamespace, "node-1", 1),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 1, validStaticPodPayloadYaml),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "installer pod ran into an error",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 1, now),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 1, validStaticPodPayloadYaml),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "operand configmap with payload without termination grace period",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeStaticPod(operandName, targetNamespace, "node-1", 2),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 2, staticPodPayloadNoTerminationGracePeriodYaml),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "operand configmap with invalid payload",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 2, staticPodPayloadInvalidTypeYaml),
			},
			expectSyncErr:   true,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "operand configmap with empty payload",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 2, emptyStaticPodPayloadYaml),
			},
			expectSyncErr:   true,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "operand configmap without pod.yaml key",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
			},
			cms: []*corev1.ConfigMap{
				{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod-%d", operandName, 2), Namespace: targetNamespace}},
			},
			expectSyncErr:   true,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "the controller should take into account the latest graceful termination period of the static pod",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Minute))),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 1, makeValidPayloadWithGracefulTerminationPeriod(30)),
				makeOperandConfigMap(targetNamespace, operandName, 2, makeValidPayloadWithGracefulTerminationPeriod(300)),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeMultiNodeDeploymentFunction(),
		},
		{
			name: "missing pod in SNO within termination threshold",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeTerminatedInstallerPod(3, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Minute))),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 1, makeValidPayloadWithGracefulTerminationPeriod(30)),
				makeOperandConfigMap(targetNamespace, operandName, 2, makeValidPayloadWithGracefulTerminationPeriod(30)),
				makeOperandConfigMap(targetNamespace, operandName, 3, makeValidPayloadWithGracefulTerminationPeriod(30)),
			},
			expectSyncErr:   false,
			expectedEvents:  0,
			isSNODeployment: makeSingleNodeDeploymentFunction(),
		},
		{
			name: "missing pod in SNO over termination threshold",
			pods: []*corev1.Pod{
				makeTerminatedInstallerPod(1, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-2*time.Hour))),
				makeTerminatedInstallerPod(2, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-time.Hour))),
				makeTerminatedInstallerPod(3, "node-1", targetNamespace, 0, metav1.NewTime(now.Add(-4*time.Minute))),
			},
			cms: []*corev1.ConfigMap{
				makeOperandConfigMap(targetNamespace, operandName, 1, makeValidPayloadWithGracefulTerminationPeriod(30)),
				makeOperandConfigMap(targetNamespace, operandName, 2, makeValidPayloadWithGracefulTerminationPeriod(30)),
				makeOperandConfigMap(targetNamespace, operandName, 3, makeValidPayloadWithGracefulTerminationPeriod(30)),
			},
			expectSyncErr:   true,
			expectedEvents:  1,
			isSNODeployment: makeSingleNodeDeploymentFunction(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				nil,
				nil,
				nil,
				nil,
			)
			kubeClient := fake.NewSimpleClientset()
			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "missing-static-pod-controller", &corev1.ObjectReference{})
			syncCtx := factory.NewSyncContext("MissingStaticPodController", eventRecorder)

			podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range tc.pods {
				podIndexer.Add(initialObj)
			}
			podLister := corev1listers.NewPodLister(podIndexer)

			cmIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range tc.cms {
				cmIndexer.Add(initialObj)
			}
			cmLister := corev1listers.NewConfigMapLister(cmIndexer)

			controller := missingStaticPodController{
				operatorClient:                    fakeOperatorClient,
				podListerForTargetNamespace:       podLister.Pods(targetNamespace),
				configMapListerForTargetNamespace: cmLister.ConfigMaps(targetNamespace),
				targetNamespace:                   targetNamespace,
				staticPodName:                     operandName,
				operandName:                       operandName,
				lastEventEmissionPerNode:          make(lastEventEmissionPerNode),
				isSNODeployment:                   tc.isSNODeployment,
			}

			err := controller.sync(context.TODO(), syncCtx)
			if err != nil != tc.expectSyncErr {
				t.Fatalf("expected sync error to have occured to be %t, got: %t, err: %v", tc.expectSyncErr, err != nil, err)
			}

			if err == nil {
				return
			}

			events, err := kubeClient.CoreV1().Events("test").List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}

			if len(events.Items) != tc.expectedEvents {
				t.Fatalf("expected %d events to have been emitted, got: %d", tc.expectedEvents, len(events.Items))
			}

			for _, ev := range events.Items {
				if ev.Reason != "MissingStaticPod" {
					t.Fatalf("expected all events to have a MissingStartingPod reason, found: %s", ev.Reason)
				}
			}
		})
	}
}

func makeMultiNodeDeploymentFunction() snoDeploymentFunc {
	return func() (bool, bool, error) {
		return false, true, nil
	}
}

func makeSingleNodeDeploymentFunction() snoDeploymentFunc {
	return func() (bool, bool, error) {
		return true, true, nil
	}
}

func makeTerminatedInstallerPod(revision int, node string, namespace string, exitCode int32, terminatedAt metav1.Time) *corev1.Pod {
	pod := makeInstallerPod(revision, node, namespace)
	pod.Status = corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "installer", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, FinishedAt: terminatedAt}}},
		},
	}
	return pod
}

func makeStaticPod(name string, namespace string, nodeName string, revision int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", name, nodeName),
			Namespace: namespace,
			Labels:    map[string]string{"revision": strconv.Itoa(revision)},
		},
		Spec:   corev1.PodSpec{},
		Status: corev1.PodStatus{},
	}
}

func makeInstallerPod(revision int, node, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("installer-%d-%s", revision, node),
			Namespace: namespace,
			Labels:    map[string]string{"app": "installer"},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

func makeOperandConfigMap(targetNamespace string, operandName string, revision int, payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod-%d", operandName, revision), Namespace: targetNamespace},
		Data:       map[string]string{"pod.yaml": payload},
	}
}

func makeValidPayloadWithGracefulTerminationPeriod(period int) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
spec:
  terminationGracePeriodSeconds: %d
`, period)
}

func TestGetStaticPodTerminationGracePeriodSecondsForRevision(t *testing.T) {
	scenarios := []struct {
		name                           string
		staticPodPayload               string
		targetRevision                 int
		expectedTerminationGracePeriod time.Duration
		expectedError                  error
	}{
		// scenario 1
		{
			name:                           "happy path, found terminationGracePeriodSeconds in pod.yaml.spec.terminationGracePeriodSeconds field",
			staticPodPayload:               validStaticPodPayloadYaml,
			expectedTerminationGracePeriod: 194 * time.Second,
		},

		// scenario 2
		{
			name:             "invalid type for terminationGracePeriodSeconds in pod.yaml.spec.terminationGracePeriodSeconds field",
			staticPodPayload: staticPodPayloadInvalidTypeYaml,
			expectedError:    fmt.Errorf("json: cannot unmarshal string into Go struct field PodSpec.spec.terminationGracePeriodSeconds of type int64"),
		},

		// scenario 3
		{
			name:                           "default value for terminationGracePeriodSeconds is returned if pod.yaml.spec.terminationGracePeriodSeconds wasn't specified",
			staticPodPayload:               staticPodPayloadNoTerminationGracePeriodYaml,
			expectedTerminationGracePeriod: 30 * time.Second,
		},

		// scenario 4
		{
			name:             "pod.yaml.spec is required",
			staticPodPayload: emptyStaticPodPayloadYaml,
			expectedError:    fmt.Errorf("didn't find required key \"pod.yaml\" in cm: operand-name-pod-0/target-namespace"),
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			targetConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("operand-name-pod-%d", scenario.targetRevision), Namespace: "target-namespace"},
			}
			if len(scenario.staticPodPayload) > 0 {
				targetConfigMap.Data = map[string]string{"pod.yaml": scenario.staticPodPayload}
			}
			configMapIndexer.Add(targetConfigMap)
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("target-namespace")

			// act
			c := &missingStaticPodController{
				operatorClient:                    nil,
				podListerForTargetNamespace:       nil,
				configMapListerForTargetNamespace: configMapLister,
				targetNamespace:                   "target-namespace",
				operandName:                       "operand-name",
			}
			actualTerminationGracePeriod, err := c.getStaticPodTerminationGracePeriodSecondsForRevision(scenario.targetRevision)

			// validate
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from getStaticPodTerminationGracePeriodSecondsForRevision function")
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatal(err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if actualTerminationGracePeriod != scenario.expectedTerminationGracePeriod {
				t.Fatalf("unexpected termination grace period for: %v, expected: %v", actualTerminationGracePeriod, scenario.expectedTerminationGracePeriod)
			}
		})
	}
}

func TestGetMostRecentInstallerPodByNode(t *testing.T) {
	testCases := []struct {
		name              string
		pods              []*corev1.Pod
		expectedPodByNode map[string]*corev1.Pod
	}{
		{
			name: "two installer pods per node",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
			},
			expectedPodByNode: map[string]*corev1.Pod{
				"node-1": {ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
				"node-2": {ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podByNode, err := getMostRecentInstallerPodByNode(tc.pods)
			if err != nil {
				t.Fatal(err)
			}

			if !cmp.Equal(podByNode, tc.expectedPodByNode) {
				t.Fatalf("unexpected most recent installer pod by node:\n%s", cmp.Diff(podByNode, tc.expectedPodByNode))
			}
		})
	}
}

func TestGetInstallerPodsByNode(t *testing.T) {
	testCases := []struct {
		name               string
		pods               []*corev1.Pod
		expectedPodsByNode map[string][]*corev1.Pod
	}{
		{
			name: "two installer pods per node",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
			},
			expectedPodsByNode: map[string][]*corev1.Pod{
				"node-1": {
					{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
				},
				"node-2": {
					{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podsByNode, err := getInstallerPodsByNode(tc.pods)
			if err != nil {
				t.Fatal(err)
			}

			if !cmp.Equal(podsByNode, tc.expectedPodsByNode) {
				t.Fatalf("unexpected pods by node seperation:\n%s", cmp.Diff(podsByNode, tc.expectedPodsByNode))
			}
		})
	}
}

func TestInstallerPodFinishedAt(t *testing.T) {
	expectedTime := metav1.Unix(1644579221, 0)

	testCases := []struct {
		name           string
		pod            *corev1.Pod
		expectFinished bool
	}{
		{
			name: "installer pod finished",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "container", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Time{}}}},
				{Name: "installer", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: expectedTime}}},
			}}},
			expectFinished: true,
		},
		{
			name: "installer pod not finished",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "installer", State: corev1.ContainerState{}},
			}}},
			expectFinished: false,
		},
		{
			name: "installer pod without installer container status reported",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "installer"},
			}}},
			expectFinished: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			time, finished := installerPodFinishedAt(tc.pod)
			if finished != tc.expectFinished {
				t.Fatalf("expected installer container finished to be %t, got: %t", tc.expectFinished, finished)
			}
			if finished && !time.Equal(expectedTime.Time) {
				t.Fatalf("expected installer container to be finished at %s, got: %s", expectedTime, time)
			}
		})
	}
}
