package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
)

const (
	executionAnnotation = "tnf.etcd.openshift.io/execution"
	retryAttemptLabel   = "tnf.etcd.openshift.io/retry-attempt"
)

// executionState is mutable execution metadata on an immutable membership record.
// FirstAttempt separates retry budgets when the same executor set recurs. LastAttempt
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

// executionAttempts validates durable Job identities and counts only the current
// retry budget. The Job sequence is independent of that budget.
func executionAttempts(record *corev1.ConfigMap, snapshot generation.Snapshot, state *executionState, list []batchv1.Job) (*batchv1.Job, int, error) {
	var latest *batchv1.Job
	latestSequence, retries := 0, 0
	for i := range list {
		job := &list[i]
		if job.Labels[generation.Label] != strconv.FormatInt(snapshot.Generation, 10) {
			continue
		}
		n, err := strconv.Atoi(job.Labels[jobs.LabelAttempt])
		if err != nil || n < 1 {
			return nil, 0, fmt.Errorf("invalid generation attempt on %s", job.Name)
		}
		if job.Name != generation.JobName(snapshot.Generation, n) || !generation.JobMatchesRecord(job, record) {
			return nil, 0, fmt.Errorf("job %s is not bound to recorded generation %s UID %s", job.Name, record.Name, record.UID)
		}
		if n > state.LastAttempt {
			state.LastAttempt = n
		}
		if n < state.FirstAttempt {
			continue
		}
		retry := n - state.FirstAttempt + 1 // Compatibility with Jobs created before retry metadata.
		if value := job.Labels[retryAttemptLabel]; value != "" {
			retry, err = strconv.Atoi(value)
			if err != nil || retry < 1 {
				return nil, 0, fmt.Errorf("invalid retry attempt on %s", job.Name)
			}
		}
		if retry > retries {
			retries = retry
		}
		if n > latestSequence {
			latest, latestSequence = job, n
		}
	}
	return latest, retries, nil
}

func (c *pacemakerLifecycleManager) updateSetupAdmission(record *corev1.ConfigMap, snapshot generation.Snapshot) jobs.JobCreateAdmissionHook {
	return func(ctx context.Context, _ *batchv1.Job) (bool, error) {
		if err := snapshot.MatchesCurrent(ctx, c.kubeClient); err != nil {
			return false, err
		}
		mcoUpdating, err := generation.MCOUpdateInProgress(ctx, c.kubeClient)
		if err != nil {
			return false, err
		}
		configured, cr, err := c.getPacemakerNodesWithCR()
		membershipKnown := err == nil && !isPacemakerCRStale(cr)
		execution, err := readExecution(record)
		if err != nil {
			return false, err
		}
		completed := record.Annotations[generation.CompletedAt] != ""
		policySnapshot := snapshot
		if completed {
			// Previous UID differences describe already completed replacement.
			policySnapshot.Previous = nil
		}
		// Only unfinished recorded repairs may inspect a local durable claim.
		continuingRepair := snapshot.Repair && execution.LastAttempt > 0 && !completed
		if !membershipKnown {
			return !mcoUpdating || continuingRepair, nil
		}
		return !policySnapshot.DeferDuringMCO(mcoUpdating, configured) || continuingRepair, nil
	}
}
