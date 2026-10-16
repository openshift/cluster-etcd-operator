package operator

import (
	"context"
	"os"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	pacemakerStatusCollectorName = "pacemaker-status-collector"
)

var (
	// statusCollectorState tracks node rotation state for the status collector CronJob.
	// Strategy: stick with success, rotate to next node on failure.
	// MaxRetryAttempts is not set - status collector failures don't trigger Degraded condition directly.
	// Instead, staleness is detected via PacemakerCluster CR's Status.LastUpdated timestamp
	// in the lifecycle manager (see isPacemakerCRStale in helpers.go).
	statusCollectorState = &jobs.JobRetryState{
		NodeIndex: 0,
	}
)

// runPacemakerStatusCollectorCronJob starts the status collector CronJob.
// Runs "tnf-monitor collect" every minute to update PacemakerCluster CR.
// Only starts if not already started (idempotent).
func (c *pacemakerLifecycleManager) runPacemakerStatusCollectorCronJob(ctx context.Context) {
	// Prevent duplicate starts
	c.statusCollectorMu.Lock()
	if c.statusCollectorStarted {
		c.statusCollectorMu.Unlock()
		klog.V(4).Infof("Status collector already started, skipping duplicate start")
		return
	}
	c.statusCollectorStarted = true
	c.statusCollectorMu.Unlock()

	schedulableNodesFunc := func() ([]*corev1.Node, error) {
		return c.getActivePacemakerNodes()
	}
	statusCronJobController := jobs.NewCronJobController(
		pacemakerStatusCollectorName,
		bindata.MustAsset("tnfdeployment/cronjob.yaml"),
		c.operatorClient,
		c.kubeClient,
		c.controllerContext.EventRecorder,
		func(_ *operatorv1.OperatorSpec, cronJob *batchv1.CronJob) error {
			cronJob.SetName(pacemakerStatusCollectorName)
			cronJob.SetNamespace(operatorclient.TargetNamespace)

			cronJob.Spec.Schedule = "* * * * *"

			if cronJob.Labels == nil {
				cronJob.Labels = make(map[string]string)
			}
			cronJob.Labels["app.kubernetes.io/name"] = pacemakerStatusCollectorName

			if cronJob.Spec.JobTemplate.Labels == nil {
				cronJob.Spec.JobTemplate.Labels = make(map[string]string)
			}
			cronJob.Spec.JobTemplate.Labels["app.kubernetes.io/name"] = pacemakerStatusCollectorName

			if cronJob.Spec.JobTemplate.Spec.Template.Labels == nil {
				cronJob.Spec.JobTemplate.Spec.Template.Labels = make(map[string]string)
			}
			cronJob.Spec.JobTemplate.Spec.Template.Labels["app.kubernetes.io/name"] = pacemakerStatusCollectorName

			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command = []string{"tnf-monitor", "collect"}

			// Get schedulable nodes (K8s ∩ Pacemaker intersection, ready only)
			// Nodes are already sorted by getActivePacemakerNodes()
			schedulableNodes, err := schedulableNodesFunc()
			if err != nil || len(schedulableNodes) == 0 {
				klog.V(4).Infof("Failed to determine schedulable nodes for status collector: %v - falling back to nodeSelector from manifest", err)
				return nil
			}

			currentNodeNames := tools.GetNodeNames(schedulableNodes)

			// Check last job status before acquiring lock (avoid blocking I/O while holding lock)
			lastJobFailed, failedJob, err := checkLastJobFailedWithJob(ctx, c.kubeClient)
			if err != nil {
				klog.V(4).Infof("Failed to check last job status: %v - using current node index", err)
			}

			statusCollectorState.Mu.Lock()
			newIndex, newTargetNodes := nextStatusCollectorNodeIndex(
				statusCollectorState.NodeIndex,
				statusCollectorState.TargetNodes,
				currentNodeNames,
				err == nil && lastJobFailed,
			)

			// Log state changes
			if !tools.StringSlicesEqual(statusCollectorState.TargetNodes, newTargetNodes) {
				klog.Infof("Status collector: node list changed from %v to %v - resetting to first node",
					statusCollectorState.TargetNodes, newTargetNodes)
			} else if err == nil && lastJobFailed {
				klog.Infof("Status collector: last job failed, rotating to node index %d (%s)",
					newIndex, schedulableNodes[newIndex].Name)
			}

			statusCollectorState.NodeIndex = newIndex
			statusCollectorState.TargetNodes = newTargetNodes
			targetNodeIndex := newIndex
			statusCollectorState.Mu.Unlock()

			// Delete failed job to unblock CronJob with Forbid concurrencyPolicy
			// Must be done after rotation state is updated (above) so we don't lose track of the failure
			if err == nil && lastJobFailed && failedJob != nil {
				deleteErr := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, failedJob.Name, metav1.DeleteOptions{
					PropagationPolicy: &[]metav1.DeletionPropagation{metav1.DeletePropagationBackground}[0],
				})
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
					klog.Warningf("Failed to delete failed job %s: %v - CronJob may be blocked by Forbid policy", failedJob.Name, deleteErr)
				} else {
					klog.V(4).Infof("Deleted failed job %s to unblock CronJob", failedJob.Name)
				}
			}

			// Pin to target node (sticky on success, rotate on failure)
			targetNode := schedulableNodes[targetNodeIndex].Name
			cronJob.Spec.JobTemplate.Spec.Template.Spec.NodeName = targetNode
			// Clear affinity to ensure NodeName takes precedence
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Affinity = nil

			klog.V(4).Infof("Status collector pinned to node: %s (index %d)", targetNode, targetNodeIndex)
			return nil
		},
	)
	go statusCronJobController.Run(ctx, 1)
}

// nextStatusCollectorNodeIndex determines the next node index for status collector based on
// node list changes and last job status. Returns the new node index and updated target nodes list.
//
// Behavior:
// - Node list changed → reset to index 0, update target nodes
// - Last job failed → rotate to next index (wrap around), preserve target nodes
// - Last job succeeded → keep current index (sticky), preserve target nodes
func nextStatusCollectorNodeIndex(
	currentIndex int,
	currentTargetNodes []string,
	newNodeNames []string,
	lastJobFailed bool,
) (newIndex int, newTargetNodes []string) {
	// Check if node list changed
	if !tools.StringSlicesEqual(currentTargetNodes, newNodeNames) {
		return 0, newNodeNames
	}

	// Node list unchanged - check if we should rotate
	if lastJobFailed && len(newNodeNames) > 0 {
		return (currentIndex + 1) % len(newNodeNames), currentTargetNodes
	}

	// Sticky behavior - stay on same node
	return currentIndex, currentTargetNodes
}

// checkLastJobFailedWithJob checks if the most recent job created by the status collector CronJob failed.
// Returns (failed bool, job *Job, error).
// Returns (false, nil, nil) if no jobs exist, job succeeded, or job is still running.
// Returns (true, job, nil) if the most recent completed job failed.
func checkLastJobFailedWithJob(ctx context.Context, kubeClient kubernetes.Interface) (bool, *batchv1.Job, error) {
	jobsList, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + pacemakerStatusCollectorName,
	})
	if err != nil {
		return false, nil, err
	}

	if len(jobsList.Items) == 0 {
		return false, nil, nil
	}

	// Find most recent COMPLETED job (ignore active jobs)
	// CronJob concurrencyPolicy: Forbid may leave jobs running past the next schedule,
	// so we must skip active jobs and only look at jobs that finished.
	var mostRecent *batchv1.Job
	for i := range jobsList.Items {
		job := &jobsList.Items[i]

		// Skip jobs that haven't completed - only examine finished jobs
		// FailureTarget is a newer condition (K8s 1.31+) for jobs that exceeded activeDeadlineSeconds
		hasCompletionCondition := false
		for _, condition := range job.Status.Conditions {
			if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget) &&
				condition.Status == corev1.ConditionTrue {
				hasCompletionCondition = true
				break
			}
		}
		if !hasCompletionCondition {
			continue // Job still running, skip it
		}

		if mostRecent == nil || job.CreationTimestamp.After(mostRecent.CreationTimestamp.Time) {
			mostRecent = job
		}
	}

	if mostRecent == nil {
		return false, nil, nil // No completed jobs yet
	}

	// Check if the completed job succeeded or failed
	for _, condition := range mostRecent.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return false, mostRecent, nil
		}
		if (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget) &&
			condition.Status == corev1.ConditionTrue {
			return true, mostRecent, nil
		}
	}

	return false, nil, nil
}
