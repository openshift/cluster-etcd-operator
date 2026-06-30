package operator

import (
	"context"
	"fmt"
	"os"
	"sort"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// pacemakerStatusCollectorName is the name of the Pacemaker status collector CronJob
	pacemakerStatusCollectorName = "pacemaker-status-collector"
)

// runPacemakerStatusCollectorCronJob starts the CronJob controller for periodic pacemaker status collection.
// The CronJob runs "tnf-monitor collect" which executes "sudo -n pcs status xml" and updates the PacemakerCluster CR.
// validNodeFunc is called on each sync to determine which nodes are valid targets for the collector.
func runPacemakerStatusCollectorCronJob(
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	validNodeFunc jobs.TargetNodesFunc,
) {
	// Start the cronjob controller to create a CronJob for periodic status collection.
	statusCronJobController := jobs.NewCronJobController(
		pacemakerStatusCollectorName,
		bindata.MustAsset("tnfdeployment/cronjob.yaml"),
		operatorClient,
		kubeClient,
		controllerContext.EventRecorder,
		func(_ *operatorv1.OperatorSpec, cronJob *batchv1.CronJob) error {
			// Set the name and namespace
			cronJob.SetName(pacemakerStatusCollectorName)
			cronJob.SetNamespace(operatorclient.TargetNamespace)

			// Set the schedule - run every minute
			cronJob.Spec.Schedule = "* * * * *"

			// Initialize labels maps if nil and set labels at all levels
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

			// Configure the container
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command = []string{"tnf-monitor", "collect", "-v=4"}

			// Get valid nodes using the provided function (same logic as update-setup job)
			validNodes, err := validNodeFunc()
			if err != nil || len(validNodes) == 0 {
				klog.Warningf("Failed to determine valid nodes for status collector: %v - falling back to nodeSelector from manifest", err)
				// On error, rely on existing nodeSelector from manifest
				return nil
			}

			// Sort nodes for deterministic ordering
			sort.Slice(validNodes, func(i, j int) bool {
				return validNodes[i].Name < validNodes[j].Name
			})

			// Determine target node based on last job status
			targetNode, err := determineStatusCollectorNode(ctx, kubeClient, validNodes)
			if err != nil {
				klog.Warningf("Failed to determine target node for status collector: %v - using first node", err)
				targetNode = validNodes[0].Name
			}

			// Pin to specific node (rotate on failure, stick with success)
			cronJob.Spec.JobTemplate.Spec.Template.Spec.NodeName = targetNode
			// Clear affinity to ensure NodeName takes precedence
			cronJob.Spec.JobTemplate.Spec.Template.Spec.Affinity = nil

			klog.V(2).Infof("Status collector pinned to node: %s", targetNode)
			return nil
		},
	)
	go statusCronJobController.Run(ctx, 1)
}

// determineStatusCollectorNode selects the target node for the status collector job.
// Strategy:
// - No job exists: use first node in sorted list
// - Last node not in valid list: use first node (node was removed)
// - Job failed: rotate to next node in list
// - Job succeeded: keep same node (stick with success)
func determineStatusCollectorNode(ctx context.Context, kubeClient kubernetes.Interface, validNodes []*corev1.Node) (string, error) {
	if len(validNodes) == 0 {
		return "", fmt.Errorf("no valid nodes provided")
	}

	// Query recent Jobs created by the CronJob
	jobs, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + pacemakerStatusCollectorName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list jobs: %w", err)
	}

	// Find most recent job
	lastJob := getMostRecentJob(jobs.Items)
	if lastJob == nil {
		// No job exists yet - start with first node
		klog.V(2).Infof("Status collector: no prior jobs, starting with first node %s", validNodes[0].Name)
		return validNodes[0].Name, nil
	}

	// Get the node the last job ran on (or was scheduled to)
	lastNode := getNodeFromJob(lastJob)
	if lastNode == "" {
		klog.Warningf("Status collector: could not determine node from last job, using first node")
		return validNodes[0].Name, nil
	}

	// Check if last node is still in valid list
	nodeIndex := findNodeIndex(validNodes, lastNode)
	if nodeIndex == -1 {
		// Last node no longer valid (removed from cluster) - start over with first node
		klog.V(2).Infof("Status collector: last node %s no longer valid, starting over with first node %s", lastNode, validNodes[0].Name)
		return validNodes[0].Name, nil
	}

	// Check if last job failed
	if isJobFailed(lastJob) {
		// Job failed - rotate to next node
		nextNode := getNextNodeInRotation(validNodes, nodeIndex)
		klog.V(2).Infof("Status collector: job failed on %s, rotating to %s", lastNode, nextNode)
		return nextNode, nil
	}

	// Job succeeded - keep same node
	klog.V(2).Infof("Status collector: job succeeded on %s, keeping same node", lastNode)
	return lastNode, nil
}

// getMostRecentJob returns the job with the latest creation timestamp
func getMostRecentJob(jobs []batchv1.Job) *batchv1.Job {
	if len(jobs) == 0 {
		return nil
	}

	var mostRecent *batchv1.Job
	for i := range jobs {
		if mostRecent == nil || jobs[i].CreationTimestamp.After(mostRecent.CreationTimestamp.Time) {
			mostRecent = &jobs[i]
		}
	}
	return mostRecent
}

// isJobFailed returns true if the job has failed
func isJobFailed(job *batchv1.Job) bool {
	// Check conditions for Failed type
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	// Also check if failed count is non-zero
	return job.Status.Failed > 0
}

// getNodeFromJob extracts the node name from the job spec
func getNodeFromJob(job *batchv1.Job) string {
	// Check NodeName first (most reliable)
	if job.Spec.Template.Spec.NodeName != "" {
		return job.Spec.Template.Spec.NodeName
	}

	// Fallback: check node affinity if NodeName not set
	affinity := job.Spec.Template.Spec.Affinity
	if affinity != nil && affinity.NodeAffinity != nil && affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, expr := range term.MatchExpressions {
				if expr.Key == "kubernetes.io/hostname" && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
					return expr.Values[0]
				}
			}
		}
	}

	return ""
}

// findNodeIndex returns the index of the node in the list, or -1 if not found
func findNodeIndex(nodes []*corev1.Node, nodeName string) int {
	for i, node := range nodes {
		if node.Name == nodeName {
			return i
		}
	}
	return -1
}

// getNextNodeInRotation returns the next node in the sorted list (wraps around)
func getNextNodeInRotation(nodes []*corev1.Node, currentIndex int) string {
	nextIndex := (currentIndex + 1) % len(nodes)
	return nodes[nextIndex].Name
}
