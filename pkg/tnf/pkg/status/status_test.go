package status

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakecore "k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func TestNewClusterStatus(t *testing.T) {
	ctx := context.Background()
	kubeClient := fakecore.NewSimpleClientset()

	testCases := []struct {
		name                  string
		isDualReplicaTopology bool
		bootstrapCompleted    bool
	}{
		{
			name:                  "dual replica enabled, bootstrap not completed",
			isDualReplicaTopology: true,
			bootstrapCompleted:    false,
		},
		{
			name:                  "dual replica enabled, bootstrap completed",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
		},
		{
			name:                  "dual replica disabled, bootstrap not completed",
			isDualReplicaTopology: false,
			bootstrapCompleted:    false,
		},
		{
			name:                  "dual replica disabled, bootstrap completed",
			isDualReplicaTopology: false,
			bootstrapCompleted:    true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewClusterStatus(ctx, kubeClient, tc.isDualReplicaTopology, tc.bootstrapCompleted)
			if cs == nil {
				t.Fatal("NewClusterStatus returned nil")
			}

			// Test that the cluster status implements the interface correctly
			if cs.IsDualReplicaTopology() != tc.isDualReplicaTopology {
				t.Errorf("expected IsDualReplicaTopology()=%v, got %v", tc.isDualReplicaTopology, cs.IsDualReplicaTopology())
			}

			// For disabled dual replica, bootstrap completion should be irrelevant
			if !tc.isDualReplicaTopology {
				if cs.IsBootstrapCompleted() {
					t.Errorf("expected IsBootstrapCompleted()=false for disabled dual replica topology, got true")
				}
				if cs.IsReadyForEtcdRemoval() {
					t.Errorf("expected IsReadyForEtcdRemoval()=false for disabled dual replica topology, got true")
				}
			}
		})
	}
}

func TestClusterStatusGetStatus(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name                  string
		isDualReplicaTopology bool
		bootstrapCompleted    bool
		setupJob              *batchv1.Job
		expectedStatus        DualReplicaClusterStatus
	}{
		{
			name:                  "dual replica disabled",
			isDualReplicaTopology: false,
			bootstrapCompleted:    false,
			setupJob:              nil,
			expectedStatus:        DualReplicaClusterStatusDisabled,
		},
		{
			name:                  "dual replica enabled, bootstrap not completed",
			isDualReplicaTopology: true,
			bootstrapCompleted:    false,
			setupJob:              nil,
			expectedStatus:        DualReplicaClusterStatusEnabled,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, no job",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			setupJob:              nil,
			expectedStatus:        DualReplicaClusterStatusBootstrapCompleted,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, job not ready",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			setupJob: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobConditionType(jobs.ReadyForEtcdContainerRemovalCondition),
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expectedStatus: DualReplicaClusterStatusBootstrapCompleted,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, job ready",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			setupJob: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobConditionType(jobs.ReadyForEtcdContainerRemovalCondition),
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedStatus: DualReplicaClusterStatusSetupReadyForEtcdRemoval,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var initialObjects []runtime.Object
			if tc.setupJob != nil {
				initialObjects = append(initialObjects, tc.setupJob)
			}

			kubeClient := fakecore.NewSimpleClientset(initialObjects...)
			cs := NewClusterStatus(ctx, kubeClient, tc.isDualReplicaTopology, tc.bootstrapCompleted).(*clusterStatus)

			status := cs.GetStatus()
			if status != tc.expectedStatus {
				t.Errorf("expected status %v, got %v", tc.expectedStatus, status)
			}
		})
	}
}

func TestClusterStatusMethods(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name                        string
		isDualReplicaTopology       bool
		bootstrapCompleted          bool
		setupJob                    *batchv1.Job
		expectedIsDualReplica       bool
		expectedIsBootstrapComplete bool
		expectedIsReadyForRemoval   bool
	}{
		{
			name:                        "dual replica disabled",
			isDualReplicaTopology:       false,
			bootstrapCompleted:          false,
			setupJob:                    nil,
			expectedIsDualReplica:       false,
			expectedIsBootstrapComplete: false,
			expectedIsReadyForRemoval:   false,
		},
		{
			name:                        "dual replica enabled, bootstrap not completed",
			isDualReplicaTopology:       true,
			bootstrapCompleted:          false,
			setupJob:                    nil,
			expectedIsDualReplica:       true,
			expectedIsBootstrapComplete: false,
			expectedIsReadyForRemoval:   false,
		},
		{
			name:                        "dual replica enabled, bootstrap completed, not ready",
			isDualReplicaTopology:       true,
			bootstrapCompleted:          true,
			setupJob:                    nil,
			expectedIsDualReplica:       true,
			expectedIsBootstrapComplete: true,
			expectedIsReadyForRemoval:   false,
		},
		{
			name:                  "dual replica enabled, bootstrap completed, ready for removal",
			isDualReplicaTopology: true,
			bootstrapCompleted:    true,
			setupJob: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tools.JobTypeSetup.GetNameLabelValue(),
					Namespace: operatorclient.TargetNamespace,
				},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{
						{
							Type:   batchv1.JobConditionType(jobs.ReadyForEtcdContainerRemovalCondition),
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedIsDualReplica:       true,
			expectedIsBootstrapComplete: true,
			expectedIsReadyForRemoval:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var initialObjects []runtime.Object
			if tc.setupJob != nil {
				initialObjects = append(initialObjects, tc.setupJob)
			}

			kubeClient := fakecore.NewSimpleClientset(initialObjects...)
			cs := NewClusterStatus(ctx, kubeClient, tc.isDualReplicaTopology, tc.bootstrapCompleted)

			if cs.IsDualReplicaTopology() != tc.expectedIsDualReplica {
				t.Errorf("expected IsDualReplicaTopology()=%v, got %v", tc.expectedIsDualReplica, cs.IsDualReplicaTopology())
			}

			if cs.IsBootstrapCompleted() != tc.expectedIsBootstrapComplete {
				t.Errorf("expected IsBootstrapCompleted()=%v, got %v", tc.expectedIsBootstrapComplete, cs.IsBootstrapCompleted())
			}

			if cs.IsReadyForEtcdRemoval() != tc.expectedIsReadyForRemoval {
				t.Errorf("expected IsReadyForEtcdRemoval()=%v, got %v", tc.expectedIsReadyForRemoval, cs.IsReadyForEtcdRemoval())
			}
		})
	}
}

func TestSetBootstrapCompleted(t *testing.T) {
	ctx := context.Background()
	kubeClient := fakecore.NewSimpleClientset()

	// Test with dual replica disabled initially
	cs := NewClusterStatus(ctx, kubeClient, true, false)

	if cs.IsBootstrapCompleted() {
		t.Fatal("expected IsBootstrapCompleted()=false initially")
	}

	cs.SetBootstrapCompleted()

	if !cs.IsBootstrapCompleted() {
		t.Fatal("expected IsBootstrapCompleted()=true after SetBootstrapCompleted()")
	}
}

func TestClusterStatusJobUpdate(t *testing.T) {
	ctx := context.Background()

	// Create a job without the ReadyForEtcdContainerRemoval condition
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tools.JobTypeSetup.GetNameLabelValue(),
			Namespace: operatorclient.TargetNamespace,
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{},
		},
	}

	kubeClient := fakecore.NewSimpleClientset(job)
	cs := NewClusterStatus(ctx, kubeClient, true, true).(*clusterStatus)

	// First call should set wasReady to false
	status := cs.GetStatus()
	if status != DualReplicaClusterStatusBootstrapCompleted {
		t.Errorf("expected status %v, got %v", DualReplicaClusterStatusBootstrapCompleted, status)
	}

	// Verify wasReady is false
	if cs.wasReady {
		t.Error("expected wasReady to be false before successful ready check")
	}

	// Now update the job to be ready
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:   batchv1.JobConditionType(jobs.ReadyForEtcdContainerRemovalCondition),
		Status: corev1.ConditionTrue,
	})
	kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})

	// Should now be ready
	status = cs.GetStatus()
	if status != DualReplicaClusterStatusSetupReadyForEtcdRemoval {
		t.Errorf("expected status %v, got %v", DualReplicaClusterStatusSetupReadyForEtcdRemoval, status)
	}

	// Verify wasReady is now true
	if !cs.wasReady {
		t.Error("expected wasReady to be true after successful ready check")
	}
}
