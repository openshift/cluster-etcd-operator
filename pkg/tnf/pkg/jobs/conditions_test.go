package jobs

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakecore "k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func TestIsTNFReadyForEtcdContainerRemoval(t *testing.T) {
	testCases := []struct {
		name           string
		job            *batchv1.Job
		expectedReady  bool
		expectedError  bool
		errorSubstring string
	}{
		{
			name:          "job not found",
			job:           nil,
			expectedReady: false,
			expectedError: false,
		},
		{
			name: "job exists but condition not set",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{},
				},
			},
			expectedReady: false,
			expectedError: false,
		},
		{
			name: "job exists with condition set to false",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobConditionType(ReadyForEtcdContainerRemovalCondition),
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expectedReady: false,
			expectedError: false,
		},
		{
			name: "job exists with condition set to true",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobConditionType(ReadyForEtcdContainerRemovalCondition),
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedReady: true,
			expectedError: false,
		},
		{
			name: "job exists with multiple conditions including target condition true",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   batchv1.JobConditionType(ReadyForEtcdContainerRemovalCondition),
							Status: corev1.ConditionTrue,
						},
						{
							Type:   batchv1.JobConditionType("SomeOtherCondition"),
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expectedReady: true,
			expectedError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var initialObjects []runtime.Object
			if tc.job != nil {
				initialObjects = append(initialObjects, tc.job)
			}

			kubeClient := fakecore.NewSimpleClientset(initialObjects...)

			ready, err := IsTNFReadyForEtcdContainerRemoval(context.TODO(), kubeClient)

			if tc.expectedError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				if tc.errorSubstring != "" && !strings.Contains(err.Error(), tc.errorSubstring) {
					t.Errorf("expected error to contain %q, got %q", tc.errorSubstring, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}

			if ready != tc.expectedReady {
				t.Errorf("expected ready=%v, got %v", tc.expectedReady, ready)
			}
		})
	}
}

func TestSetTNFReadyForEtcdContainerRemoval(t *testing.T) {
	testCases := []struct {
		name           string
		job            *batchv1.Job
		expectedError  bool
		errorSubstring string
	}{
		{
			name:           "job not found",
			job:            nil,
			expectedError:  true,
			errorSubstring: "not found",
		},
		{
			name: "job exists without existing conditions",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{},
				},
			},
			expectedError: false,
		},
		{
			name: "job exists with existing condition - should update",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:    batchv1.JobConditionType(ReadyForEtcdContainerRemovalCondition),
							Status:  corev1.ConditionFalse,
							Reason:  "OldReason",
							Message: "Old message",
						},
					},
				},
			},
			expectedError: false,
		},
		{
			name: "job exists with other conditions - should append",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var initialObjects []runtime.Object
			if tc.job != nil {
				initialObjects = append(initialObjects, tc.job)
			}

			kubeClient := fakecore.NewSimpleClientset(initialObjects...)

			err := SetTNFReadyForEtcdContainerRemoval(context.TODO(), kubeClient)

			if tc.expectedError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				if tc.errorSubstring != "" && !strings.Contains(err.Error(), tc.errorSubstring) {
					t.Errorf("expected error to contain %q, got %q", tc.errorSubstring, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				// Verify the condition was set correctly
				updatedJob, getErr := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(context.TODO(), tools.JobTypeSetup.GetNameLabelValue(), metav1.GetOptions{})
				if getErr != nil {
					t.Errorf("failed to get updated job: %v", getErr)
				}

				conditionFound := false
				for _, condition := range updatedJob.Status.Conditions {
					if string(condition.Type) == ReadyForEtcdContainerRemovalCondition {
						conditionFound = true
						if condition.Status != corev1.ConditionTrue {
							t.Errorf("expected condition status to be True, got %v", condition.Status)
						}
						if condition.Reason != "TNFSetupReadyForCEOEtcdRemoval" {
							t.Errorf("expected reason to be TNFSetupReadyForCEOEtcdRemoval, got %v", condition.Reason)
						}
						if condition.Message != "TNF setup job is ready for CEO etcd container removal" {
							t.Errorf("expected specific message, got %v", condition.Message)
						}
						break
					}
				}

				if !conditionFound {
					t.Errorf("expected condition %s to be found in job status", ReadyForEtcdContainerRemovalCondition)
				}
			}
		})
	}
}

func TestGetSetupJob(t *testing.T) {
	testCases := []struct {
		name           string
		job            *batchv1.Job
		expectedJob    bool
		expectedError  bool
		errorSubstring string
	}{
		{
			name:        "job not found",
			job:         nil,
			expectedJob: false,
		},
		{
			name: "job found",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
			},
			expectedJob: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var initialObjects []runtime.Object
			if tc.job != nil {
				initialObjects = append(initialObjects, tc.job)
			}

			kubeClient := fakecore.NewSimpleClientset(initialObjects...)

			job, err := getSetupJob(context.TODO(), kubeClient)

			if tc.expectedError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				if tc.errorSubstring != "" && !strings.Contains(err.Error(), tc.errorSubstring) {
					t.Errorf("expected error to contain %q, got %q", tc.errorSubstring, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}

			if tc.expectedJob && job == nil {
				t.Errorf("expected job to be returned but got nil")
			}

			if !tc.expectedJob && job != nil {
				t.Errorf("expected job to be nil but got a job")
			}
		})
	}
}
