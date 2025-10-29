package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestIsRunning(t *testing.T) {
	tests := []struct {
		name       string
		job        batchv1.Job
		wantResult bool
	}{
		{
			name: "Job with no conditions - running",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{},
				},
			},
			wantResult: true,
		},
		{
			name: "Job with Complete condition - not running",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Job with Failed condition - not running",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Job with Complete condition false - running",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: true,
		},
		{
			name: "Job with Failed condition false - running",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: true,
		},
		{
			name: "Job with both Complete and Failed conditions true - not running",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsRunning(tt.job)
			require.Equal(t, tt.wantResult, result,
				"Expected IsRunning() = %v, got %v", tt.wantResult, result)
		})
	}
}

func TestIsComplete(t *testing.T) {
	tests := []struct {
		name       string
		job        batchv1.Job
		wantResult bool
	}{
		{
			name: "Job with Complete condition true",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: true,
		},
		{
			name: "Job with Complete condition false",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Job with no conditions",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{},
				},
			},
			wantResult: false,
		},
		{
			name: "Job with only Failed condition",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsComplete(tt.job)
			require.Equal(t, tt.wantResult, result,
				"Expected IsComplete() = %v, got %v", tt.wantResult, result)
		})
	}
}

func TestIsFailed(t *testing.T) {
	tests := []struct {
		name       string
		job        batchv1.Job
		wantResult bool
	}{
		{
			name: "Job with Failed condition true",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: true,
		},
		{
			name: "Job with Failed condition false",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobFailed,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			wantResult: false,
		},
		{
			name: "Job with no conditions",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{},
				},
			},
			wantResult: false,
		},
		{
			name: "Job with only Complete condition",
			job: batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobComplete,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsFailed(tt.job)
			require.Equal(t, tt.wantResult, result,
				"Expected IsFailed() = %v, got %v", tt.wantResult, result)
		})
	}
}

func TestWaitForCompletion(t *testing.T) {
	tests := []struct {
		name          string
		setupClient   func() *fake.Clientset
		jobName       string
		jobNamespace  string
		timeout       time.Duration
		expectError   bool
		errorContains string
	}{
		{
			name: "Job completes successfully",
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{
							{
								Type:   batchv1.JobComplete,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				return fake.NewSimpleClientset(job)
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			timeout:      5 * time.Second,
			expectError:  false,
		},
		{
			name: "Job fails",
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{
							{
								Type:   batchv1.JobFailed,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				return fake.NewSimpleClientset(job)
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			timeout:      5 * time.Second,
			expectError:  false,
		},
		{
			name: "Job is deleted (not found)",
			setupClient: func() *fake.Clientset {
				client := fake.NewSimpleClientset()
				// No job exists, so Get will return NotFound error
				return client
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			timeout:      5 * time.Second,
			expectError:  false,
		},
		{
			name: "Job still running - timeout",
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{},
					},
				}
				return fake.NewSimpleClientset(job)
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			timeout:      1 * time.Second,
			expectError:  true,
		},
		{
			name: "Job transitions from running to complete",
			setupClient: func() *fake.Clientset {
				runningJob := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
					Status: batchv1.JobStatus{
						Conditions: []batchv1.JobCondition{},
					},
				}
				client := fake.NewSimpleClientset(runningJob)

				// After first Get, update the job to be complete
				callCount := 0
				client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					callCount++
					if callCount > 2 {
						completeJob := &batchv1.Job{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "test-job",
								Namespace: "test-namespace",
							},
							Status: batchv1.JobStatus{
								Conditions: []batchv1.JobCondition{
									{
										Type:   batchv1.JobComplete,
										Status: corev1.ConditionTrue,
									},
								},
							},
						}
						return true, completeJob, nil
					}
					return false, nil, nil
				})

				return client
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			timeout:      10 * time.Second,
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := tt.setupClient()
			ctx := context.Background()

			err := WaitForCompletion(ctx, client, tt.jobName, tt.jobNamespace, tt.timeout)

			if tt.expectError {
				require.Error(t, err, "Expected error but got none")
				if tt.errorContains != "" {
					require.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				require.NoError(t, err, "Expected no error but got: %v", err)
			}
		})
	}
}

func TestDeleteAndWait(t *testing.T) {
	tests := []struct {
		name         string
		setupClient  func() *fake.Clientset
		jobName      string
		jobNamespace string
		expectError  bool
	}{
		{
			name: "Delete existing job successfully",
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
				}
				client := fake.NewSimpleClientset(job)

				// After delete, subsequent Gets should return NotFound
				client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					// Simulate successful deletion
					return false, nil, nil
				})

				// Make Get return NotFound after deletion
				deleted := false
				client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					if deleted {
						return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), "test-job")
					}
					// First get after deletion initiates the wait loop
					deleted = true
					return false, nil, nil
				})

				return client
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			expectError:  false,
		},
		{
			name: "Delete job that doesn't exist - no error",
			setupClient: func() *fake.Clientset {
				client := fake.NewSimpleClientset()
				// Simulate NotFound error on Delete
				client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), "test-job")
				})
				return client
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			expectError:  false,
		},
		{
			name: "Delete fails with non-NotFound error",
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
				}
				client := fake.NewSimpleClientset(job)
				// Simulate a different error
				client.PrependReactor("delete", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewForbidden(batchv1.Resource("jobs"), "test-job", nil)
				})
				return client
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			expectError:  true,
		},
		{
			name: "Job deletion times out",
			setupClient: func() *fake.Clientset {
				job := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-job",
						Namespace: "test-namespace",
					},
				}
				client := fake.NewSimpleClientset(job)

				// Delete succeeds but Get never returns NotFound
				client.PrependReactor("get", "jobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
					// Always return the job (it never gets deleted)
					return true, job, nil
				})

				return client
			},
			jobName:      "test-job",
			jobNamespace: "test-namespace",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := tt.setupClient()
			ctx := context.Background()

			err := DeleteAndWait(ctx, client, tt.jobName, tt.jobNamespace)

			if tt.expectError {
				require.Error(t, err, "Expected error but got none")
			} else {
				require.NoError(t, err, "Expected no error but got: %v", err)
			}
		})
	}
}
