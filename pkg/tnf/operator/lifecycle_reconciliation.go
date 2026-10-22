package operator

import (
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const generationAttempts = 6 // Per execution configuration.

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
	waitReason, waitJob := "", ""
	runningPods := map[types.UID]bool{}
	// A failed Job can still have terminating pods. Never overlap their mutations.
	for _, job := range list.Items {
		if !jobs.IsStopped(job) || job.Status.Active > 0 {
			waitReason, waitJob = "Running", job.Name
		}
		pods, err := c.kubeClient.CoreV1().Pods(generation.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/controller-uid=" + string(job.UID)})
		if err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				waitReason, waitJob = "Terminating", job.Name
				runningPods[job.UID] = true
			}
		}
	}
	pods, err := c.kubeClient.CoreV1().Pods(generation.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + tools.JobTypeUpdateSetup.GetNameLabelValue()})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			waitReason, waitJob = "Terminating", pod.Name
		}
	}
	// Old fixed-name jobs have no generation. Once stopped they cannot prove completion.
	for _, job := range list.Items {
		if job.Labels[generation.Label] == "" {
			if waitReason != "" {
				return c.generationStatus(ctx, waitReason, waitJob, false, true)
			}
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
	mcoUpdating, err := generation.MCOUpdateInProgress(ctx, c.kubeClient)
	if err != nil {
		return err
	}
	image := os.Getenv("OPERATOR_IMAGE")
	if image == "" {
		return fmt.Errorf("OPERATOR_IMAGE is not set")
	}
	changed := record == nil || !generation.SameNodes(snapshot.Nodes, desired) || snapshot.Image != image
	targets, err := c.getActivePacemakerNodes()
	if err != nil && !errors.Is(err, errNoReadyControlPlaneNodes) {
		return err
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	config, err := executorConfig(targets)
	if err != nil {
		return err
	}
	state := executionState{FirstAttempt: 1}
	var latest *batchv1.Job
	retries := 0
	deferRepair := false
	if record != nil {
		state, err = readExecution(record)
		if err != nil {
			return err
		}
		latest, retries, err = executionAttempts(record, snapshot, &state, list.Items)
		if err != nil {
			return err
		}
		if state.Config == "" {
			state.Config = config
		}
		record, err = c.saveExecution(ctx, record, state, false)
		if err != nil {
			return err
		}
		if waitReason == "" && !changed && latest != nil && jobs.IsComplete(*latest) &&
			(record.Annotations[generation.CompletedJob] == "" || record.Annotations[generation.CompletedRecordUID] != string(record.UID)) {
			copy := record.DeepCopy()
			if copy.Annotations == nil {
				copy.Annotations = map[string]string{}
			}
			copy.Annotations[generation.CompletedJob] = latest.Name
			copy.Annotations[generation.CompletedRecordUID] = string(record.UID)
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
		// Completion belongs to the membership generation, not its executor
		// set or the continued existence of the successful Job. Fresh membership
		// drift after success needs a new repair claim/generation.
		if !changed && record.Annotations[generation.CompletedAt] != "" && record.Annotations[generation.CompletedRecordUID] == string(record.UID) {
			remove, add := snapshot.MembershipChanges(configured)
			if len(remove)+len(add) == 0 && waitReason == "" {
				return c.cleanupGenerations(ctx, records)
			}
			if len(remove)+len(add) > 0 {
				completed, err := time.Parse(time.RFC3339, record.Annotations[generation.CompletedAt])
				if err != nil {
					return err
				}
				if cr.Status.LastUpdated.Time.Before(completed) || time.Since(completed) < 2*time.Minute {
					deferRepair = true
				} else {
					changed = true
				}
			}
		}

		reset := !changed && state.Config != "" && state.Config != config
		if reset {
			state.FirstAttempt = state.LastAttempt + 1
			latest, retries = nil, 0
		}
		state.Config = config
		record, err = c.saveExecution(ctx, record, state, reset)
		if err != nil {
			return err
		}
		// Persist the new retry boundary BEFORE deleting an old attempt. This
		// survives restart even when the cancelled Job has already disappeared.
		for _, job := range list.Items {
			n, _ := strconv.Atoi(job.Labels[jobs.LabelAttempt])
			stale := changed || (job.Labels[generation.Label] == strconv.FormatInt(snapshot.Generation, 10) && n < state.FirstAttempt)
			if stale && (!jobs.IsStopped(job) || job.Status.Active > 0 || runningPods[job.UID]) {
				if job.DeletionTimestamp == nil {
					err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationForeground), Preconditions: &metav1.Preconditions{UID: &job.UID}})
					if err != nil && !apierrors.IsNotFound(err) {
						return err
					}
				}
				return c.generationStatus(ctx, "Terminating", job.Name, false, true)
			}
		}
	}
	if waitReason != "" {
		return c.generationStatus(ctx, waitReason, waitJob, false, true)
	}
	if deferRepair {
		return c.generationStatus(ctx, "Deferred", "Waiting for fresh membership after the convergence window", false, false)
	}
	if !changed && retries >= generationAttempts {
		if latest == nil || time.Since(finishedTime(latest)) < 10*time.Minute {
			return c.generationStatus(ctx, "MaxRetriesExceeded", jobs.DegradedMessageMaxRetries, true, false)
		}
		state.FirstAttempt = state.LastAttempt + 1
		record, err = c.saveExecution(ctx, record, state, true)
		if err != nil {
			return err
		}
		retries = 0
	}
	if changed {
		next := generation.Snapshot{Version: 1, Generation: 1, Nodes: desired, Image: image}
		if record != nil {
			next.Generation = snapshot.Generation + 1
			next.Previous = snapshot.Nodes
		}
		next.Repair = next.NeedsMembershipRepair(configured)
		if next.DeferDuringMCO(mcoUpdating, configured) {
			return c.generationStatus(ctx, "Deferred", "Waiting for control-plane MCO activity to finish", false, false)
		}
		cm, err := next.ConfigMap()
		if err != nil {
			return err
		}
		executionData, err := json.Marshal(executionState{Config: config, FirstAttempt: 1})
		if err != nil {
			return err
		}
		cm.Annotations = map[string]string{executionAnnotation: string(executionData)}
		created, err := c.kubeClient.CoreV1().ConfigMaps(generation.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		// Re-read the persisted record and its UID before starting any job.
		return c.pruneGenerations(ctx, append(records, *created))
	}
	if len(targets) == 0 {
		return c.generationStatus(ctx, "Deferred", "Waiting for an eligible execution node", false, false)
	}
	target := targets[retries%len(targets)]
	job := generationJob(record, snapshot, target.Name, state.LastAttempt+1)
	job.Labels[retryAttemptLabel] = strconv.Itoa(retries + 1)
	admitted, err := c.updateSetupAdmission(record, snapshot)(ctx, job)
	if err != nil {
		return err
	}
	if !admitted {
		return c.generationStatus(ctx, "Deferred", "Waiting for control-plane MCO activity to finish", false, false)
	}
	// Reserve the sequence only after admission. API failures may leave gaps in
	// names, but only actual Jobs consume the retry budget.
	state.LastAttempt++
	record, err = c.saveExecution(ctx, record, state, false)
	if err != nil {
		return err
	}

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
	job.Spec.Template.Labels["app.kubernetes.io/name"] = tools.JobTypeUpdateSetup.GetNameLabelValue()
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
	progressingCondition := condition("Progressing", progressing)
	if progressing {
		progressingCondition.Reason = "JobRunning"
		progressingCondition.Message = fmt.Sprintf("%s: %s", reason, message)
	}
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(degradedCondition), v1helpers.UpdateConditionFn(progressingCondition))
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
