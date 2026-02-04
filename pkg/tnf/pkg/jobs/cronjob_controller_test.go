package jobs

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

func TestCronJobController_NeedsUpdate(t *testing.T) {
	controller := &CronJobController{name: "test"}

	tests := []struct {
		name     string
		existing *batchv1.CronJob
		desired  *batchv1.CronJob
		expected bool
	}{
		{
			name: "identical specs return false",
			existing: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			desired: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "different schedule returns true",
			existing: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			desired: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "0 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "different image returns true",
			existing: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			desired: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v2"},
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "different container command returns true",
			existing: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1", Command: []string{"old", "command"}},
									},
								},
							},
						},
					},
				},
			},
			desired: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1", Command: []string{"new", "command"}},
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "different concurrency policy returns true",
			existing: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					ConcurrencyPolicy: batchv1.AllowConcurrent,
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			desired: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule:          "*/5 * * * *",
					ConcurrencyPolicy: batchv1.ForbidConcurrent,
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "nil vs default values return false after normalization",
			existing: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule:                   "*/5 * * * *",
					Suspend:                    ptrBool(false),
					ConcurrencyPolicy:          batchv1.AllowConcurrent,
					SuccessfulJobsHistoryLimit: ptrInt32(3),
					FailedJobsHistoryLimit:     ptrInt32(1),
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							BackoffLimit: ptrInt32(6),
							Completions:  ptrInt32(1),
							Parallelism:  ptrInt32(1),
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy:                 "Never",
									TerminationGracePeriodSeconds: ptrInt64(30),
									DNSPolicy:                     "ClusterFirst",
									SchedulerName:                 "default-scheduler",
									SecurityContext:               &corev1.PodSecurityContext{},
									Containers: []corev1.Container{
										{
											Name:                     "test",
											Image:                    "test:v1",
											TerminationMessagePath:   "/dev/termination-log",
											TerminationMessagePolicy: "File",
											ImagePullPolicy:          corev1.PullIfNotPresent,
										},
									},
								},
							},
						},
					},
				},
			},
			desired: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					// All other fields nil - will be normalized to defaults
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{Name: "test", Image: "test:v1"},
									},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.needsUpdate(tt.existing, tt.desired)
			if result != tt.expected {
				t.Errorf("needsUpdate() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestPtrHelpers(t *testing.T) {
	t.Run("ptrBool", func(t *testing.T) {
		truePtr := ptrBool(true)
		if truePtr == nil || *truePtr != true {
			t.Error("ptrBool(true) failed")
		}
		falsePtr := ptrBool(false)
		if falsePtr == nil || *falsePtr != false {
			t.Error("ptrBool(false) failed")
		}
	})

	t.Run("ptrInt32", func(t *testing.T) {
		ptr := ptrInt32(42)
		if ptr == nil || *ptr != 42 {
			t.Error("ptrInt32(42) failed")
		}
	})

	t.Run("ptrInt64", func(t *testing.T) {
		ptr := ptrInt64(42)
		if ptr == nil || *ptr != 42 {
			t.Error("ptrInt64(42) failed")
		}
	})
}

func TestNormalizeCronJobSpec_MultipleContainers(t *testing.T) {
	cronJob := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "container1", Image: "image1:latest"},
								{Name: "container2", Image: "image2:v1.0"},
								{Name: "container3", Image: "image3"},
							},
						},
					},
				},
			},
		},
	}

	normalizeCronJobSpec(cronJob)

	containers := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers

	// container1 with :latest should get Always
	if containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("Container with :latest tag should have PullAlways, got %v", containers[0].ImagePullPolicy)
	}

	// container2 with specific tag should get IfNotPresent
	if containers[1].ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("Container with specific tag should have PullIfNotPresent, got %v", containers[1].ImagePullPolicy)
	}

	// container3 with no tag should get Always
	if containers[2].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("Container with no tag should have PullAlways, got %v", containers[2].ImagePullPolicy)
	}

	// All containers should have defaults applied
	for i, c := range containers {
		if c.TerminationMessagePath != "/dev/termination-log" {
			t.Errorf("Container %d should have TerminationMessagePath /dev/termination-log", i)
		}
		if c.TerminationMessagePolicy != "File" {
			t.Errorf("Container %d should have TerminationMessagePolicy File", i)
		}
	}
}

func TestNeedsUpdate_DeepEqualityAfterNormalization(t *testing.T) {
	controller := &CronJobController{name: "test"}

	// Create two specs that look different but should be equal after normalization
	existing := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			Schedule:                   "*/5 * * * *",
			Suspend:                    ptrBool(false),
			ConcurrencyPolicy:          batchv1.AllowConcurrent,
			SuccessfulJobsHistoryLimit: ptrInt32(3),
			FailedJobsHistoryLimit:     ptrInt32(1),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit:   ptrInt32(6),
					Completions:    ptrInt32(1),
					Parallelism:    ptrInt32(1),
					Suspend:        ptrBool(false),
					CompletionMode: func() *batchv1.CompletionMode { m := batchv1.NonIndexedCompletion; return &m }(),
					PodReplacementPolicy: func() *batchv1.PodReplacementPolicy {
						p := batchv1.TerminatingOrFailed
						return &p
					}(),
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy:                 "Never",
							TerminationGracePeriodSeconds: ptrInt64(30),
							DNSPolicy:                     "ClusterFirst",
							SchedulerName:                 "default-scheduler",
							SecurityContext:               &corev1.PodSecurityContext{},
							ServiceAccountName:            "test-sa",
							DeprecatedServiceAccount:      "test-sa",
							Containers: []corev1.Container{
								{
									Name:                     "test",
									Image:                    "test:v1",
									TerminationMessagePath:   "/dev/termination-log",
									TerminationMessagePolicy: "File",
									ImagePullPolicy:          corev1.PullIfNotPresent,
								},
							},
						},
					},
				},
			},
		},
	}

	// Desired spec with all nil/empty values that should normalize to same as existing
	desired := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							ServiceAccountName: "test-sa",
							Containers: []corev1.Container{
								{Name: "test", Image: "test:v1"},
							},
						},
					},
				},
			},
		},
	}

	// They should NOT need an update after normalization
	if controller.needsUpdate(existing, desired) {
		// Normalize both and compare for debugging
		existingCopy := existing.DeepCopy()
		desiredCopy := desired.DeepCopy()
		normalizeCronJobSpec(existingCopy)
		normalizeCronJobSpec(desiredCopy)

		equalAfterNormalization := equality.Semantic.DeepEqual(existingCopy.Spec, desiredCopy.Spec)
		t.Errorf("needsUpdate returned true but should have returned false; normalized specs equal: %v", equalAfterNormalization)
	}
}

func TestNeedsUpdate_ServiceAccountNameNormalization(t *testing.T) {
	controller := &CronJobController{name: "test"}

	// API server auto-populates DeprecatedServiceAccount to match ServiceAccountName
	existing := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							ServiceAccountName:       "my-service-account",
							DeprecatedServiceAccount: "my-service-account", // Set by API server
							Containers: []corev1.Container{
								{Name: "test", Image: "test:v1"},
							},
						},
					},
				},
			},
		},
	}

	// Desired only has ServiceAccountName, not the deprecated field
	desired := &batchv1.CronJob{
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							ServiceAccountName: "my-service-account",
							// DeprecatedServiceAccount not set in manifest
							Containers: []corev1.Container{
								{Name: "test", Image: "test:v1"},
							},
						},
					},
				},
			},
		},
	}

	// Should NOT need update - normalization should handle this
	if controller.needsUpdate(existing, desired) {
		t.Error("DeprecatedServiceAccount normalization should prevent false drift detection")
	}
}
