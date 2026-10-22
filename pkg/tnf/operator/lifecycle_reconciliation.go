package operator

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const generationAttempts = 6 // Three rounds across a two-node cluster.

func (c *pacemakerLifecycleManager) reconcileGeneration(ctx context.Context) error {
	spec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	if spec.ManagementState != operatorv1.Managed {
		return nil
	}
	transition, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, c.operatorClient)
	if err != nil || !transition {
		return err
	}
	nodes, err := tools.ListNodesFromInformer(c.controlPlaneNodeInformer)
	if err != nil {
		return err
	}
	desired, err := generation.Nodes(nodes)
	if err != nil {
		return err
	}
	records, err := generation.Records(ctx, c.kubeClient)
	if err != nil {
		return err
	}
	list, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + tools.JobTypeUpdateSetup.GetNameLabelValue()})
	if err != nil {
		return err
	}
	// A failed Job can still have terminating pods. Never overlap their mutations.
	for _, job := range list.Items {
		if !jobs.IsStopped(job) || job.Status.Active > 0 {
			return c.generationStatus(ctx, "Running", job.Name, false, true)
		}
		pods, err := c.kubeClient.CoreV1().Pods(generation.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/controller-uid=" + string(job.UID)})
		if err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				return c.generationStatus(ctx, "Terminating", job.Name, false, true)
			}
		}
	}
	// Old fixed-name jobs have no generation. Once stopped they cannot prove completion.
	for _, job := range list.Items {
		if job.Labels[generation.Label] == "" {
			if err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			return nil
		}
	}
	var record *corev1.ConfigMap
	var snapshot generation.Snapshot
	if len(records) > 0 {
		record = &records[len(records)-1]
		snapshot, err = generation.Decode(record)
		if err != nil {
			return err
		}
	}
	configured, cr, err := c.getPacemakerNodesWithCR()
	if err != nil || isPacemakerCRStale(cr) {
		return fmt.Errorf("waiting for fresh Pacemaker membership before reconciliation: %v", err)
	}

	if len(configured) != len(*cr.Status.Nodes) {
		return fmt.Errorf("incomplete Pacemaker member identities")
	}
	for _, ip := range configured {
		parsed := net.ParseIP(ip)
		if parsed == nil || !parsed.IsGlobalUnicast() {
			return fmt.Errorf("invalid Pacemaker member address %q", ip)
		}
	}
	progressing, err := generation.UpgradeInProgress(ctx, c.kubeClient, c.configClient)
	if err != nil {
		return err
	}
	image := os.Getenv("OPERATOR_IMAGE")
	if image == "" {
		return fmt.Errorf("OPERATOR_IMAGE is not set")
	}
	changed := record == nil || !generation.SameNodes(snapshot.Nodes, desired) || snapshot.Image != image
	var latest *batchv1.Job
	attempt := 0
	for i := range list.Items {
		job := &list.Items[i]
		if record == nil || job.Labels[generation.Label] != strconv.FormatInt(snapshot.Generation, 10) {
			continue
		}
		n, err := strconv.Atoi(job.Labels[jobs.LabelAttempt])
		if err != nil || n < 1 {
			return fmt.Errorf("invalid generation attempt on %s", job.Name)
		}
		if n > attempt {
			attempt = n
			latest = job
		}
	}
	if !changed && latest != nil && jobs.IsComplete(*latest) {
		if record.Annotations[generation.CompletedJob] == "" {
			copy := record.DeepCopy()
			if copy.Annotations == nil {
				copy.Annotations = map[string]string{}
			}
			copy.Annotations[generation.CompletedJob] = latest.Name
			completed := time.Now()
			if latest.Status.CompletionTime != nil {
				completed = latest.Status.CompletionTime.Time
			}
			copy.Annotations[generation.CompletedAt] = completed.UTC().Format(time.RFC3339)
			if _, err := c.kubeClient.CoreV1().ConfigMaps(generation.Namespace).Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
				return err
			}
			return c.generationStatus(ctx, "Completed", latest.Name, false, false)
		}
		remove, add := snapshot.MembershipChanges(configured)
		if len(remove)+len(add) == 0 {
			return c.cleanupGenerations(ctx, records)
		}
		completed, err := time.Parse(time.RFC3339, record.Annotations[generation.CompletedAt])
		if err != nil {
			return err
		}
		if cr.Status.LastUpdated.Time.Before(completed) || time.Since(completed) < 2*time.Minute {
			return nil
		}
		changed = true // Observed drift recurred after success; record a new repair, not an old success.
	}
	if !changed && attempt >= generationAttempts {
		if latest == nil || time.Since(finishedTime(latest)) < 10*time.Minute {
			return c.generationStatus(ctx, "MaxRetriesExceeded", jobs.DegradedMessageMaxRetries, true, false)
		}
		changed = true
	}
	if changed {
		next := generation.Snapshot{Version: 1, Generation: 1, Nodes: desired, Image: image}
		if record != nil {
			next.Generation = snapshot.Generation + 1
			next.Previous = snapshot.Nodes
		}
		remove, add := next.MembershipChanges(configured)
		next.Repair = len(remove)+len(add) > 0
		if next.DeferDuringUpgrade(progressing, configured) {
			return c.generationStatus(ctx, "Deferred", "Waiting for upgrade/MCO activity to finish", false, false)
		}
		cm, err := next.ConfigMap()
		if err != nil {
			return err
		}
		created, err := c.kubeClient.CoreV1().ConfigMaps(generation.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		// Re-read the persisted record and its UID before starting any job.
		return c.pruneGenerations(ctx, append(records, *created))
	}
	if snapshot.DeferDuringUpgrade(progressing, configured) && !(snapshot.Repair && attempt > 0) {
		return c.generationStatus(ctx, "Deferred", "Waiting for upgrade/MCO activity to finish", false, false)
	}
	if err := snapshot.MatchesCurrent(ctx, c.kubeClient); err != nil {
		return err
	}
	targets, err := c.getActivePacemakerNodes()
	if err != nil {
		return err
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	target := targets[attempt%len(targets)]
	job := generationJob(record, snapshot, target.Name, attempt+1)
	if _, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}
	return c.generationStatus(ctx, "Running", job.Name, false, true)
}

func generationJob(record *corev1.ConfigMap, snapshot generation.Snapshot, target string, attempt int) *batchv1.Job {
	job := jobs.ReadJobV1OrDie(bindata.MustAsset("tnfdeployment/job.yaml"))
	job.Name = generation.JobName(snapshot.Generation, attempt)
	job.Labels["app.kubernetes.io/name"] = tools.JobTypeUpdateSetup.GetNameLabelValue()
	job.Labels[generation.Label] = strconv.FormatInt(snapshot.Generation, 10)
	job.Labels[jobs.LabelAttempt] = strconv.Itoa(attempt)
	job.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: record.Name, UID: record.UID}}
	job.Spec.BackoffLimit = ptr.To(int32(0))
	job.Spec.ActiveDeadlineSeconds = ptr.To(int64(900))
	job.Spec.Template.Spec.NodeName = target
	container := &job.Spec.Template.Spec.Containers[0]
	container.Image = snapshot.Image
	container.Command[1] = tools.JobTypeUpdateSetup.GetSubCommand()
	container.Env = append(container.Env, corev1.EnvVar{Name: generation.ConfigMapEnv, Value: record.Name}, corev1.EnvVar{Name: generation.ConfigMapUIDEnv, Value: string(record.UID)})
	return job
}

func (c *pacemakerLifecycleManager) generationStatus(ctx context.Context, reason, message string, degraded, progressing bool) error {
	condition := func(kind string, value bool) operatorv1.OperatorCondition {
		status := operatorv1.ConditionFalse
		if value {
			status = operatorv1.ConditionTrue
		}
		return operatorv1.OperatorCondition{Type: "TNFUpdateSetupJob" + kind, Status: status, Reason: reason, Message: message}
	}

	degradedCondition := condition("Degraded", degraded)
	if !degraded && reason != "Completed" && reason != "AsExpected" {
		_, status, _, err := c.operatorClient.GetOperatorState()
		if err != nil {
			return err
		}
		old := v1helpers.FindOperatorCondition(status.Conditions, "TNFUpdateSetupJobDegraded")
		if old != nil && old.Status == operatorv1.ConditionTrue {
			degradedCondition = *old
		}
	}
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(degradedCondition), v1helpers.UpdateConditionFn(condition("Progressing", progressing)))
	return err
}

func (c *pacemakerLifecycleManager) cleanupGenerations(ctx context.Context, records []corev1.ConfigMap) error {
	if err := c.pruneGenerations(ctx, records); err != nil {
		return err
	}
	return c.generationStatus(ctx, "AsExpected", "Recorded generation is reconciled", false, false)
}

func (c *pacemakerLifecycleManager) pruneGenerations(ctx context.Context, records []corev1.ConfigMap) error {
	// Reconciliation verified that all jobs and pods have stopped before pruning.

	for i := 0; i < len(records)-5; i++ {
		record := records[i]

		if err := c.kubeClient.CoreV1().ConfigMaps(generation.Namespace).Delete(ctx, record.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func finishedTime(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobFailed || c.Type == batchv1.JobFailureTarget) && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Now()
}
