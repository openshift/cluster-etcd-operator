package operator

import (
	"context"
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// cleanupOrphanedJobs removes retired node jobs and pods whose Job no longer exists.
// Live API reads keep deletion independent of stale informer snapshots.
func (c *pacemakerLifecycleManager) cleanupOrphanedJobs(ctx context.Context) error {
	spec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil || spec.ManagementState != operatorv1.Managed {
		return err
	}
	jobClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	jobs, err := jobClient.List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name in (" + tools.JobTypeAuth.GetNameLabelValue() + "," + tools.JobTypeAfterSetup.GetNameLabelValue() + ")"})
	if err != nil {
		return err
	}
	// Observe Jobs before Nodes so a newly created node job is checked against
	// a node list that is at least as recent as the Job observation.
	nodes, err := c.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: tools.ControlPlaneNodeLabelSelector})
	if err != nil {
		return fmt.Errorf("listing nodes for TNF cleanup: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil
	}
	uids, names := map[types.UID]bool{}, map[string]bool{}
	for _, node := range nodes.Items {
		uids[node.UID], names[node.Name] = true, true
	}
	for _, job := range jobs.Items {
		uid := types.UID(job.Labels["node"])
		stale := uid != "" && !uids[uid]
		if uid == "" && job.Spec.Template.Spec.NodeName != "" {
			stale = !names[job.Spec.Template.Spec.NodeName]
		}
		if !stale || job.DeletionTimestamp != nil {
			continue
		}
		options := metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground), Preconditions: &metav1.Preconditions{UID: &job.UID}}
		if err := jobClient.Delete(ctx, job.Name, options); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("deleting stale TNF job %s: %w", job.Name, err)
		}
	}
	podClient := c.kubeClient.CoreV1().Pods(operatorclient.TargetNamespace)
	pods, err := podClient.List(ctx, metav1.ListOptions{LabelSelector: "app=tnf-job"})
	if err != nil {
		return err
	}
	deleted := 0
	for _, pod := range pods.Items {
		if deleted >= 50 {
			break
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		orphan := len(pod.OwnerReferences) == 0 && (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed)
		if owner := metav1.GetControllerOf(&pod); owner != nil && owner.APIVersion == "batch/v1" && owner.Kind == "Job" {
			job, err := jobClient.Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			orphan = apierrors.IsNotFound(err) || job.UID != owner.UID
		}
		if !orphan {
			continue
		}
		if err := podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("deleting orphan TNF pod %s: %w", pod.Name, err)
		}
		deleted++
	}
	return nil
}
