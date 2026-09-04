package operator

/*
TEST COVERAGE SUMMARY - status_collector_test.go
=================================================

This file tests status collector node rotation and failure handling logic.

WHAT'S TESTED
-------------

checkLastJobFailed():
├── Job completed successfully → false
├── Job failed → true
├── Job failed with FailureTarget (deadline exceeded) → true
├── Job completed despite earlier pod failures (Complete takes precedence) → false
├── Job still running (no conditions) → false
└── No jobs exist → false

Node rotation state machine:
├── Success → stays on same node (sticky behavior)
├── Failure → rotates to next node
├── Failure on last node → wraps around to first node
└── Node list changed → resets to first node
*/

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type jobSetup struct {
	timestamp  time.Time
	conditions []batchv1.JobCondition
}

func TestCheckLastJobFailed(t *testing.T) {
	tests := []struct {
		name           string
		jobConditions  []batchv1.JobCondition
		jobFailedCount int32
		wantFailed     bool
		createJob      bool // Explicitly control job creation
		// For multi-job tests
		multipleJobs []jobSetup
	}{
		{
			name: "job completed successfully",
			jobConditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
			createJob:  true,
			wantFailed: false,
		},
		{
			name: "job failed",
			jobConditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			createJob:  true,
			wantFailed: true,
		},
		{
			name: "job failed with FailureTarget - deadline exceeded",
			jobConditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
			},
			createJob:  true,
			wantFailed: true,
		},
		{
			name: "job completed successfully despite earlier pod failures - Complete takes precedence",
			jobConditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			jobFailedCount: 2, // Had pod failures during retries
			createJob:      true,
			wantFailed:     false,
		},
		{
			name:          "job still running - no conditions set",
			jobConditions: []batchv1.JobCondition{},
			createJob:     true,
			wantFailed:    false,
		},
		{
			name:          "no jobs exist",
			jobConditions: nil,
			createJob:     false,
			wantFailed:    false,
		},
		{
			name: "multiple jobs - newest failed overrides older success",
			multipleJobs: []jobSetup{
				{
					timestamp: time.Now().Add(-2 * time.Minute),
					conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
				{
					timestamp: time.Now().Add(-1 * time.Minute),
					conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
			},
			wantFailed: true,
		},
		{
			name: "multiple jobs - newest success overrides older failure",
			multipleJobs: []jobSetup{
				{
					timestamp: time.Now().Add(-2 * time.Minute),
					conditions: []batchv1.JobCondition{
						{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
					},
				},
				{
					timestamp: time.Now().Add(-1 * time.Minute),
					conditions: []batchv1.JobCondition{
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
					},
				},
			},
			wantFailed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()

			// Create single job or multiple jobs
			if tt.multipleJobs != nil {
				for i, js := range tt.multipleJobs {
					job := &batchv1.Job{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("test-job-%d", i+1),
							Namespace: operatorclient.TargetNamespace,
							Labels: map[string]string{
								"app.kubernetes.io/name": pacemakerStatusCollectorName,
							},
							CreationTimestamp: metav1.NewTime(js.timestamp),
						},
						Status: batchv1.JobStatus{
							Conditions: js.conditions,
						},
					}
					_, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(context.Background(), job, metav1.CreateOptions{})
					require.NoError(t, err)
				}
			} else if tt.createJob {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job-1",
						Namespace: operatorclient.TargetNamespace,
						Labels: map[string]string{
							"app.kubernetes.io/name": pacemakerStatusCollectorName,
						},
						CreationTimestamp: metav1.NewTime(time.Now()),
					},
					Status: batchv1.JobStatus{
						Conditions: tt.jobConditions,
						Failed:     tt.jobFailedCount,
					},
				}
				_, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Create(context.Background(), job, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			failed, job, err := checkLastJobFailedWithJob(context.Background(), kubeClient)
			require.NoError(t, err)
			require.Equal(t, tt.wantFailed, failed)
			if tt.wantFailed {
				require.NotNil(t, job, "expected job to be returned when failed=true")
			}
		})
	}
}

func TestNextStatusCollectorNodeIndex(t *testing.T) {
	tests := []struct {
		name                string
		currentIndex        int
		currentTargetNodes  []string
		newNodeNames        []string
		lastJobFailed       bool
		expectedIndex       int
		expectedTargetNodes []string
	}{
		{
			name:                "success - stays on same node (sticky behavior)",
			currentIndex:        1,
			currentTargetNodes:  []string{"master-0", "master-1"},
			newNodeNames:        []string{"master-0", "master-1"},
			lastJobFailed:       false,
			expectedIndex:       1,
			expectedTargetNodes: []string{"master-0", "master-1"},
		},
		{
			name:                "failure - rotates to next node",
			currentIndex:        0,
			currentTargetNodes:  []string{"master-0", "master-1"},
			newNodeNames:        []string{"master-0", "master-1"},
			lastJobFailed:       true,
			expectedIndex:       1,
			expectedTargetNodes: []string{"master-0", "master-1"},
		},
		{
			name:                "failure on last node - wraps around to first",
			currentIndex:        1,
			currentTargetNodes:  []string{"master-0", "master-1"},
			newNodeNames:        []string{"master-0", "master-1"},
			lastJobFailed:       true,
			expectedIndex:       0,
			expectedTargetNodes: []string{"master-0", "master-1"},
		},
		{
			name:                "node list changed - resets to first node",
			currentIndex:        1,
			currentTargetNodes:  []string{"master-0", "master-1"},
			newNodeNames:        []string{"master-0", "master-2"}, // master-1 replaced with master-2
			lastJobFailed:       false,
			expectedIndex:       0,
			expectedTargetNodes: []string{"master-0", "master-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the pure helper function
			actualIndex, actualTargetNodes := nextStatusCollectorNodeIndex(
				tt.currentIndex,
				tt.currentTargetNodes,
				tt.newNodeNames,
				tt.lastJobFailed,
			)

			// Verify results
			require.Equal(t, tt.expectedIndex, actualIndex, "Node index mismatch")
			require.Equal(t, tt.expectedTargetNodes, actualTargetNodes, "Target nodes mismatch")
		})
	}
}
