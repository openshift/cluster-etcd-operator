package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
)

const executionAnnotation = "tnf.etcd.openshift.io/execution"

// executionState is mutable execution metadata on an immutable membership record.
// FirstAttempt excludes cancelled work when the same executor set recurs. LastAttempt
// reserves unique Job names even after cancellation or an ambiguous create response.
type executionState struct {
	Config       string `json:"config"`
	FirstAttempt int    `json:"firstAttempt"`
	LastAttempt  int    `json:"lastAttempt"`
}

func executorConfig(nodes []*corev1.Node) (string, error) {
	type identity struct {
		Name string
		UID  types.UID
	}
	identities := make([]identity, 0, len(nodes))
	for _, node := range nodes {
		identities = append(identities, identity{node.Name, node.UID})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Name < identities[j].Name })
	data, err := json.Marshal(identities)
	return string(data), err
}

func readExecution(record *corev1.ConfigMap) (executionState, error) {
	state := executionState{FirstAttempt: 1}
	if data := record.Annotations[executionAnnotation]; data != "" {
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			return state, err
		}
	}
	if state.FirstAttempt < 1 || state.LastAttempt < 0 || state.FirstAttempt > state.LastAttempt+1 {
		return state, fmt.Errorf("invalid execution metadata on %s", record.Name)
	}
	return state, nil
}

func (c *pacemakerLifecycleManager) saveExecution(ctx context.Context, record *corev1.ConfigMap, state executionState, reset bool) (*corev1.ConfigMap, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if record.Annotations[executionAnnotation] == string(data) && !reset {
		return record, nil
	}
	copy := record.DeepCopy()
	if copy.Annotations == nil {
		copy.Annotations = map[string]string{}
	}
	copy.Annotations[executionAnnotation] = string(data)
	if reset {
		delete(copy.Annotations, generation.CompletedJob)
	}
	return c.kubeClient.CoreV1().ConfigMaps(generation.Namespace).Update(ctx, copy, metav1.UpdateOptions{})
}

// executionAttempts validates Job identities and finds the latest non-cancelled attempt.
func executionAttempts(record *corev1.ConfigMap, snapshot generation.Snapshot, state *executionState, list []batchv1.Job) (*batchv1.Job, error) {
	var latest *batchv1.Job
	latestSequence := 0
	for i := range list {
		job := &list[i]
		if job.Labels[generation.Label] != strconv.FormatInt(snapshot.Generation, 10) {
			continue
		}
		n, err := strconv.Atoi(job.Labels[jobs.LabelAttempt])
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid generation attempt on %s", job.Name)
		}
		if job.Name != generation.JobName(snapshot.Generation, n) || !generation.JobMatchesRecord(job, record) {
			return nil, fmt.Errorf("job %s is not bound to recorded generation %s UID %s", job.Name, record.Name, record.UID)
		}
		if n > state.LastAttempt {
			state.LastAttempt = n
		}
		if n < state.FirstAttempt {
			continue
		}
		if n > latestSequence {
			latest, latestSequence = job, n
		}
	}
	return latest, nil
}

// Reconstruct installer-style backoff from the retained attempt across restarts.
func generationRetryAfter(job *batchv1.Job, firstAttempt int, now time.Time) time.Duration {
	attempt, _ := strconv.Atoi(job.Labels[jobs.LabelAttempt])
	backoff := wait.Backoff{Duration: 10 * time.Second, Factor: 1.5, Steps: attempt - firstAttempt + 1, Cap: 10 * time.Minute}
	for backoff.Steps > 0 {
		backoff.Step()
	}
	var failedAt time.Time
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget) && condition.LastTransitionTime.After(failedAt) {
			failedAt = condition.LastTransitionTime.Time
		}
	}
	return failedAt.Add(backoff.Duration).Sub(now)
}

func (c *pacemakerLifecycleManager) updateSetupAdmission(snapshot generation.Snapshot) jobs.JobCreateAdmissionHook {
	return func(ctx context.Context, _ *batchv1.Job) (bool, error) {
		if err := snapshot.MatchesCurrent(ctx, c.kubeClient); err != nil {
			return false, err
		}
		configured, cr, err := c.getPacemakerNodesWithCR()
		// Unknown membership is checked by the runner against live PCS state.
		if err != nil || isPacemakerCRStale(cr) || !snapshot.NeedsMemberRemoval(configured) {
			return true, nil
		}
		mcoUpdating, err := generation.MCOUpdateInProgress(ctx, c.kubeClient)
		return !mcoUpdating, err
	}
}
