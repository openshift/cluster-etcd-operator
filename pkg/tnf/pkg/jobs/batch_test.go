package jobs

import (
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestReadCronJobV1OrDie(t *testing.T) {
	tests := []struct {
		name         string
		input        []byte
		expectedName string
		expectedNS   string
	}{
		{
			name: "valid YAML CronJob",
			input: []byte(`
apiVersion: batch/v1
kind: CronJob
metadata:
  name: test-cronjob
  namespace: test-namespace
spec:
  schedule: "*/5 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: test
            image: busybox
          restartPolicy: OnFailure
`),
			expectedName: "test-cronjob",
			expectedNS:   "test-namespace",
		},
		{
			name: "valid JSON CronJob",
			input: []byte(`{
  "apiVersion": "batch/v1",
  "kind": "CronJob",
  "metadata": {
    "name": "json-cronjob",
    "namespace": "json-namespace"
  },
  "spec": {
    "schedule": "0 * * * *",
    "jobTemplate": {
      "spec": {
        "template": {
          "spec": {
            "containers": [{"name": "test", "image": "busybox"}],
            "restartPolicy": "OnFailure"
          }
        }
      }
    }
  }
}`),
			expectedName: "json-cronjob",
			expectedNS:   "json-namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ReadCronJobV1OrDie(tt.input)

			require.NotNil(t, result, "ReadCronJobV1OrDie should return non-nil CronJob")
			require.IsType(t, &batchv1.CronJob{}, result, "result should be *batchv1.CronJob")
			require.Equal(t, tt.expectedName, result.Name, "CronJob name should match")
			require.Equal(t, tt.expectedNS, result.Namespace, "CronJob namespace should match")
		})
	}
}

func TestReadCronJobV1OrDie_Panics(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "invalid YAML",
			input: []byte(`this is not valid yaml: [`),
		},
		{
			name:  "empty input",
			input: []byte{},
		},
		{
			name: "wrong kind",
			input: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-cronjob
`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Panics(t, func() {
				ReadCronJobV1OrDie(tt.input)
			}, "ReadCronJobV1OrDie should panic on invalid input")
		})
	}
}

func TestReadJobV1OrDie(t *testing.T) {
	tests := []struct {
		name         string
		input        []byte
		expectedName string
		expectedNS   string
	}{
		{
			name: "valid YAML Job",
			input: []byte(`
apiVersion: batch/v1
kind: Job
metadata:
  name: test-job
  namespace: test-namespace
spec:
  template:
    spec:
      containers:
      - name: test
        image: busybox
      restartPolicy: Never
`),
			expectedName: "test-job",
			expectedNS:   "test-namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ReadJobV1OrDie(tt.input)

			require.NotNil(t, result, "ReadJobV1OrDie should return non-nil Job")
			require.IsType(t, &batchv1.Job{}, result, "result should be *batchv1.Job")
			require.Equal(t, tt.expectedName, result.Name, "Job name should match")
			require.Equal(t, tt.expectedNS, result.Namespace, "Job namespace should match")
		})
	}
}

func TestReadJobV1OrDie_Panics(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "invalid YAML",
			input: []byte(`this is not valid yaml: [`),
		},
		{
			name:  "empty input",
			input: []byte{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Panics(t, func() {
				ReadJobV1OrDie(tt.input)
			}, "ReadJobV1OrDie should panic on invalid input")
		})
	}
}

func TestNormalizeCronJobSpec(t *testing.T) {
	tests := []struct {
		name     string
		input    *batchv1.CronJob
		validate func(t *testing.T, cronJob *batchv1.CronJob)
	}{
		{
			name: "applies CronJobSpec defaults",
			input: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule: "*/5 * * * *",
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										// Use a tagged image so ImagePullPolicy defaults to IfNotPresent
										// (untagged images default to Always per Kubernetes semantics)
										{Name: "test", Image: "busybox:1.36"},
									},
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, cronJob *batchv1.CronJob) {
				// CronJobSpec defaults
				require.NotNil(t, cronJob.Spec.Suspend, "Suspend should be set")
				require.False(t, *cronJob.Spec.Suspend, "Suspend should default to false")
				require.Equal(t, batchv1.AllowConcurrent, cronJob.Spec.ConcurrencyPolicy, "ConcurrencyPolicy should default to Allow")
				require.NotNil(t, cronJob.Spec.SuccessfulJobsHistoryLimit, "SuccessfulJobsHistoryLimit should be set")
				require.Equal(t, int32(3), *cronJob.Spec.SuccessfulJobsHistoryLimit, "SuccessfulJobsHistoryLimit should default to 3")
				require.NotNil(t, cronJob.Spec.FailedJobsHistoryLimit, "FailedJobsHistoryLimit should be set")
				require.Equal(t, int32(1), *cronJob.Spec.FailedJobsHistoryLimit, "FailedJobsHistoryLimit should default to 1")

				// JobSpec defaults
				jobSpec := &cronJob.Spec.JobTemplate.Spec
				require.NotNil(t, jobSpec.BackoffLimit, "BackoffLimit should be set")
				require.Equal(t, int32(6), *jobSpec.BackoffLimit, "BackoffLimit should default to 6")
				require.NotNil(t, jobSpec.Completions, "Completions should be set")
				require.Equal(t, int32(1), *jobSpec.Completions, "Completions should default to 1")
				require.NotNil(t, jobSpec.Parallelism, "Parallelism should be set")
				require.Equal(t, int32(1), *jobSpec.Parallelism, "Parallelism should default to 1")

				// PodSpec defaults
				podSpec := &jobSpec.Template.Spec
				require.Equal(t, corev1.RestartPolicyNever, podSpec.RestartPolicy, "RestartPolicy should default to Never")
				require.NotNil(t, podSpec.TerminationGracePeriodSeconds, "TerminationGracePeriodSeconds should be set")
				require.Equal(t, int64(30), *podSpec.TerminationGracePeriodSeconds, "TerminationGracePeriodSeconds should default to 30")
				require.Equal(t, corev1.DNSClusterFirst, podSpec.DNSPolicy, "DNSPolicy should default to ClusterFirst")
				require.Equal(t, "default-scheduler", podSpec.SchedulerName, "SchedulerName should default to default-scheduler")

				// Container defaults - tagged image defaults to IfNotPresent
				container := &podSpec.Containers[0]
				require.Equal(t, "/dev/termination-log", container.TerminationMessagePath)
				require.Equal(t, corev1.TerminationMessageReadFile, container.TerminationMessagePolicy)
				require.Equal(t, corev1.PullIfNotPresent, container.ImagePullPolicy)
			},
		},
		{
			name: "preserves explicitly set values",
			input: &batchv1.CronJob{
				Spec: batchv1.CronJobSpec{
					Schedule:                   "0 * * * *",
					Suspend:                    ptrBool(true),
					ConcurrencyPolicy:          batchv1.ForbidConcurrent,
					SuccessfulJobsHistoryLimit: ptrInt32(5),
					FailedJobsHistoryLimit:     ptrInt32(2),
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							BackoffLimit: ptrInt32(3),
							Completions:  ptrInt32(2),
							Parallelism:  ptrInt32(2),
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy:                 corev1.RestartPolicyOnFailure,
									TerminationGracePeriodSeconds: ptrInt64(60),
									DNSPolicy:                     corev1.DNSDefault,
									SchedulerName:                 "custom-scheduler",
									Containers: []corev1.Container{
										{
											Name:                     "test",
											Image:                    "busybox",
											TerminationMessagePath:   "/custom/path",
											TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
											ImagePullPolicy:          corev1.PullAlways,
										},
									},
								},
							},
						},
					},
				},
			},
			validate: func(t *testing.T, cronJob *batchv1.CronJob) {
				// Verify explicitly set values are preserved
				require.True(t, *cronJob.Spec.Suspend, "Suspend should remain true")
				require.Equal(t, batchv1.ForbidConcurrent, cronJob.Spec.ConcurrencyPolicy)
				require.Equal(t, int32(5), *cronJob.Spec.SuccessfulJobsHistoryLimit)
				require.Equal(t, int32(2), *cronJob.Spec.FailedJobsHistoryLimit)

				jobSpec := &cronJob.Spec.JobTemplate.Spec
				require.Equal(t, int32(3), *jobSpec.BackoffLimit)
				require.Equal(t, int32(2), *jobSpec.Completions)
				require.Equal(t, int32(2), *jobSpec.Parallelism)

				podSpec := &jobSpec.Template.Spec
				require.Equal(t, corev1.RestartPolicyOnFailure, podSpec.RestartPolicy)
				require.Equal(t, int64(60), *podSpec.TerminationGracePeriodSeconds)
				require.Equal(t, corev1.DNSDefault, podSpec.DNSPolicy)
				require.Equal(t, "custom-scheduler", podSpec.SchedulerName)

				container := &podSpec.Containers[0]
				require.Equal(t, "/custom/path", container.TerminationMessagePath)
				require.Equal(t, corev1.TerminationMessageFallbackToLogsOnError, container.TerminationMessagePolicy)
				require.Equal(t, corev1.PullAlways, container.ImagePullPolicy)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cronJob := tt.input.DeepCopy()
			normalizeCronJobSpec(cronJob)
			tt.validate(t, cronJob)
		})
	}
}

func TestInferImagePullPolicy(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		expected corev1.PullPolicy
	}{
		// Images that should default to Always
		{name: "empty image", image: "", expected: corev1.PullAlways},
		{name: "no tag", image: "busybox", expected: corev1.PullAlways},
		{name: "latest tag", image: "busybox:latest", expected: corev1.PullAlways},
		{name: "registry with no tag", image: "gcr.io/my-project/image", expected: corev1.PullAlways},
		{name: "registry with latest tag", image: "quay.io/openshift/origin:latest", expected: corev1.PullAlways},
		{name: "registry with port and no tag", image: "registry.example.com:5000/image", expected: corev1.PullAlways},

		// Images that should default to IfNotPresent
		{name: "specific tag", image: "busybox:1.36", expected: corev1.PullIfNotPresent},
		{name: "registry with specific tag", image: "gcr.io/my-project/image:v1.2.3", expected: corev1.PullIfNotPresent},
		{name: "registry with port and tag", image: "registry.example.com:5000/image:v2", expected: corev1.PullIfNotPresent},
		// Valid SHA256 digests must be 64 hex characters
		{name: "digest", image: "busybox@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4", expected: corev1.PullIfNotPresent},
		{name: "registry with digest", image: "gcr.io/my-project/image@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", expected: corev1.PullIfNotPresent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := inferImagePullPolicy(tt.image)
			require.Equal(t, tt.expected, result, "ImagePullPolicy for %q", tt.image)
		})
	}
}
