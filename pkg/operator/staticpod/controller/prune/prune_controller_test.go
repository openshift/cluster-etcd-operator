package prune

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func TestSync(t *testing.T) {
	tests := []struct {
		name string

		failedLimit    int32
		succeededLimit int32

		targetNamespace string
		status          operatorv1.StaticPodOperatorStatus

		objects         []int32
		expectedObjects []int32

		expectedPrunePod  bool
		expectedPruneArgs string
	}{
		{
			name:            "prunes api resources based on failedLimit 1, succeedLimit 1",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 4,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 2,
						TargetRevision:  0,
					},
				},
			},
			failedLimit:       1,
			succeededLimit:    1,
			objects:           []int32{1, 2, 3, 4},
			expectedObjects:   []int32{2, 4},
			expectedPrunePod:  true,
			expectedPruneArgs: "-v=4 --max-eligible-revision=4 --protected-revisions=2,4 --resource-dir=/etc/kubernetes/static-pod-resources --cert-dir= --static-pod-name=test-pod",
		},
		{
			name:            "prunes api resources with multiple nodes based on failedLimit 1, succeedLimit 1",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 5,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 2,
					},
					{
						NodeName:        "test-node-2",
						CurrentRevision: 3,
					},
					{
						NodeName:        "test-node-3",
						CurrentRevision: 4,
					},
				},
			},
			failedLimit:       1,
			succeededLimit:    1,
			objects:           []int32{1, 2, 3, 4, 5, 6},
			expectedObjects:   []int32{2, 3, 4, 5, 6},
			expectedPrunePod:  true,
			expectedPruneArgs: "-v=4 --max-eligible-revision=5 --protected-revisions=2,3,4,5 --resource-dir=/etc/kubernetes/static-pod-resources --cert-dir= --static-pod-name=test-pod",
		},
		{
			name:            "prunes api resources without nodes",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 1,
				NodeStatuses:            []operatorv1.NodeStatus{},
			},
			failedLimit:      1,
			succeededLimit:   1,
			objects:          []int32{1, 2, 3, 4, 5, 6},
			expectedObjects:  []int32{1, 2, 3, 4, 5, 6},
			expectedPrunePod: false,
		},
		{
			name:            "prunes api resources based on failedLimit 2, succeedLimit 3",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 10,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 4,
						TargetRevision:  7,
					},
				},
			},
			failedLimit:       2,
			succeededLimit:    3,
			objects:           []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			expectedObjects:   []int32{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			expectedPrunePod:  true,
			expectedPruneArgs: "-v=4 --max-eligible-revision=10 --protected-revisions=2,3,4,5,6,7,8,9,10 --resource-dir=/etc/kubernetes/static-pod-resources --cert-dir= --static-pod-name=test-pod",
		},
		{
			name:            "prunes api resources based on failedLimit 2, succeedLimit 3 and all relevant revisions set",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 40,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:           "test-node-1",
						CurrentRevision:    10,
						TargetRevision:     30,
						LastFailedRevision: 20,
					},
				},
			},
			failedLimit:       2,
			succeededLimit:    3,
			objects:           int32Range(1, 50),
			expectedObjects:   []int32{8, 9, 10, 19, 20, 28, 29, 30, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50},
			expectedPrunePod:  true,
			expectedPruneArgs: "-v=4 --max-eligible-revision=40 --protected-revisions=8,9,10,19,20,28,29,30,38,39,40 --resource-dir=/etc/kubernetes/static-pod-resources --cert-dir= --static-pod-name=test-pod",
		},
		{
			name:            "prunes api resources based on failedLimit 0, succeedLimit 0",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 40,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:           "test-node-1",
						CurrentRevision:    10,
						TargetRevision:     30,
						LastFailedRevision: 20,
					},
				},
			},
			failedLimit:       0,
			succeededLimit:    0,
			objects:           int32Range(1, 50),
			expectedObjects:   []int32{6, 7, 8, 9, 10, 16, 17, 18, 19, 20, 26, 27, 28, 29, 30, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50},
			expectedPrunePod:  true,
			expectedPruneArgs: "-v=4 --max-eligible-revision=40 --protected-revisions=6,7,8,9,10,16,17,18,19,20,26,27,28,29,30,36,37,38,39,40 --resource-dir=/etc/kubernetes/static-pod-resources --cert-dir= --static-pod-name=test-pod",
		},
		{
			name:            "protects all",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 20,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:           "test-node-1",
						CurrentRevision:    10,
						TargetRevision:     15,
						LastFailedRevision: 5,
					},
				},
			},
			failedLimit:      5,
			succeededLimit:   5,
			objects:          []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			expectedObjects:  []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			expectedPrunePod: false,
		},
		{
			name:            "protects all with different nodes",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 20,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 15,
					},
					{
						NodeName:           "test-node-2",
						CurrentRevision:    10,
						LastFailedRevision: 5,
					},
				},
			},
			failedLimit:      5,
			succeededLimit:   5,
			objects:          []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			expectedObjects:  []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
			expectedPrunePod: false,
		},
		{
			name:            "protects all with unlimited revisions",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 1,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 1,
						TargetRevision:  0,
					},
				},
			},
			failedLimit:      -1,
			succeededLimit:   -1,
			objects:          []int32{1, 2, 3},
			expectedObjects:  []int32{1, 2, 3},
			expectedPrunePod: false,
		},
		{
			name:            "protects all with unlimited succeeded revisions",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 5,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 1,
						TargetRevision:  0,
					},
				},
			},
			failedLimit:      1,
			succeededLimit:   -1,
			objects:          []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			expectedObjects:  []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			expectedPrunePod: false,
		},
		{
			name:            "protects all with unlimited failed revisions",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 5,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 1,
						TargetRevision:  0,
					},
				},
			},
			failedLimit:      -1,
			succeededLimit:   1,
			objects:          []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			expectedObjects:  []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			expectedPrunePod: false,
		},
		{
			name:            "protects all with unlimited failed revisions, but no progress",
			targetNamespace: "prune-api",
			status: operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: 5,
				NodeStatuses: []operatorv1.NodeStatus{
					{
						NodeName:        "test-node-1",
						CurrentRevision: 5,
						TargetRevision:  0,
					},
				},
			},
			failedLimit:       -1,
			succeededLimit:    1,
			objects:           []int32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			expectedObjects:   []int32{5, 6, 7, 8, 9, 10},
			expectedPrunePod:  true,
			expectedPruneArgs: "-v=4 --max-eligible-revision=5 --protected-revisions=5 --resource-dir=/etc/kubernetes/static-pod-resources --cert-dir= --static-pod-name=test-pod",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()
			for _, rev := range tc.objects {
				_ = kubeClient.Tracker().Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("revision-status-%d", rev), Namespace: "prune-api"},
					Data: map[string]string{
						"revision": fmt.Sprintf("%d", rev),
					},
				})
			}
			fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					FailedRevisionLimit:    tc.failedLimit,
					SucceededRevisionLimit: tc.succeededLimit,
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&tc.status,
				nil,
				nil,
			)
			var prunerPod *corev1.Pod
			kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				prunerPod = action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				return false, nil, nil
			})
			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

			c := &PruneController{
				targetNamespace:   tc.targetNamespace,
				podResourcePrefix: "test-pod",
				command:           []string{"/bin/true"},
				configMapGetter:   kubeClient.CoreV1(),
				secretGetter:      kubeClient.CoreV1(),
				podGetter:         kubeClient.CoreV1(),
				operatorClient:    fakeStaticPodOperatorClient,
			}
			c.retrieveStatusConfigMapOwnerRefsFn = func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error) {
				return []metav1.OwnerReference{}, nil
			}
			c.prunerPodImageFn = func() string { return "docker.io/foo/bar" }

			if err := c.sync(context.TODO(), factory.NewSyncContext("TestSync", eventRecorder)); err != nil {
				t.Fatal(err)
			}

			// check configmap still existing
			statusConfigMaps, err := c.configMapGetter.ConfigMaps(tc.targetNamespace).List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				t.Fatalf("unexpected error %q", err)
			}
			expected := sets.New(tc.expectedObjects...)
			got := sets.New(configMapRevisions(t, statusConfigMaps.Items)...)
			if missing := expected.Difference(got); len(missing) > 0 {
				t.Errorf("got %+v, missing %+v", sets.List(got), sets.List(missing))
			}
			if unexpected := got.Difference(expected); len(unexpected) > 0 {
				t.Errorf("got %+v, unexpected %+v", sets.List(got), sets.List(unexpected))
			}

			// check prune pod
			if !tc.expectedPrunePod && prunerPod != nil {
				t.Errorf("unexpected pruner pod created with command: %v", prunerPod.Spec.Containers[0].Args)
			} else if tc.expectedPrunePod && prunerPod == nil {
				t.Error("expected pruner pod, but it has not been created")
			} else if tc.expectedPrunePod {
				gotArgs := strings.Join(prunerPod.Spec.Containers[0].Args, " ")
				if gotArgs != tc.expectedPruneArgs {
					t.Errorf("unexpected arguments:\n      got: %s\n expected: %s", gotArgs, tc.expectedPruneArgs)
				}
			}
		})
	}
}

func int32Range(from, to int32) []int32 {
	ret := make([]int32, to-from+1)
	for i := from; i <= to; i++ {
		ret[i-from] = i
	}
	return ret
}

func configMapRevisions(t *testing.T, objs []corev1.ConfigMap) []int32 {
	revs := make([]int32, 0, len(objs))
	for _, o := range objs {
		if rev, err := strconv.Atoi(o.Data["revision"]); err != nil {
			t.Fatal(err)
		} else {
			revs = append(revs, int32(rev))
		}
	}
	return revs
}
