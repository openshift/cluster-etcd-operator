package internal

import (
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSortByRevision(t *testing.T) {
	testCases := []struct {
		name             string
		pods             []*corev1.Pod
		expectedPodOrder []*corev1.Pod
	}{
		{
			name: "unsorted installer pods",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-3-node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-1"}},
			},
			expectedPodOrder: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-2-node-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "installer-3-node-1"}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sort.Sort(ByRevision(tc.pods))

			if !cmp.Equal(tc.pods, tc.expectedPodOrder) {
				t.Fatalf("unexpected pod order after sorting by revision:\n%s", cmp.Diff(tc.pods, tc.expectedPodOrder))
			}
		})
	}
}

func TestGetRevisionOfPod(t *testing.T) {
	testCases := []struct {
		name             string
		pod              *corev1.Pod
		expectedRevision int
		expectError      bool
	}{
		{
			name:             "installer pod",
			pod:              &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "installer-1-node-1"}},
			expectedRevision: 1,
			expectError:      false,
		},
		{
			name:        "installer pod without revision",
			pod:         &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "installer"}},
			expectError: true,
		},
		{
			name:        "installer pod with bad revision",
			pod:         &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "installer-a-node-1"}},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			revision, err := GetRevisionOfPod(tc.pod)
			if err != nil != tc.expectError {
				t.Fatalf("expected error to have occured to be %t, got: %t, err: %v", tc.expectError, err != nil, err)
			}
			if err == nil && revision != tc.expectedRevision {
				t.Fatalf("expected revision to be %d, got: %d", tc.expectedRevision, revision)
			}
		})
	}
}
