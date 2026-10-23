package generation

import (
	"context"
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
)

const NodeUIDEnv = "TNF_NODE_UID"

// SetupReady uses the durable transition condition after bootstrap, not the continued
// existence of a one-time setup Job. After-setup additionally needs its node's generation.
func SetupReady(ctx context.Context, kube kubernetes.Interface, operator operatorclient.EtcdInterface, nodeName string, uid types.UID) (bool, error) {
	etcd, err := operator.Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	transition := false
	for _, condition := range etcd.Status.Conditions {
		if condition.Type == ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition && condition.Status == operatorv1.ConditionTrue {
			transition = true
		}
	}
	if !transition {
		job, err := kube.BatchV1().Jobs(Namespace).Get(ctx, "tnf-setup-job", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return complete(job), nil
	}
	if nodeName == "" {
		return true, nil
	}
	if uid == "" {
		return false, fmt.Errorf("after-setup requires its scheduled node UID")
	}
	node, err := kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if node.UID != uid {
		return false, fmt.Errorf("node %s was replaced", nodeName)
	}
	records, err := Records(ctx, kube)
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return false, nil
	}
	cm := &records[len(records)-1]
	snapshot, err := Decode(cm)
	if err != nil {
		return false, err
	}
	if err := snapshot.MatchesCurrent(ctx, kube); err != nil {
		return false, err
	}
	found := false
	for _, n := range snapshot.Nodes {
		if n.Name == nodeName && n.UID == uid {
			found = true
		}
	}
	if !found || cm.Annotations[CompletedJob] == "" {
		return false, nil
	}
	job, err := kube.BatchV1().Jobs(Namespace).Get(ctx, cm.Annotations[CompletedJob], metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// The lifecycle controller recorded this only after validating a
		// successful Job's ownership. Bind that durable proof to this exact
		// record incarnation so cleanup of the Job does not block after-setup.
		return cm.UID != "" && cm.Annotations[CompletedRecordUID] == string(cm.UID) && cm.Annotations[CompletedAt] != "", nil
	}
	if err != nil {
		return false, err
	}
	return JobMatchesRecord(job, cm) && complete(job), nil
}

// JobMatchesRecord rejects success from an older incarnation of the same record name.
func JobMatchesRecord(job *batchv1.Job, cm *corev1.ConfigMap) bool {
	if job.Labels[Label] != cm.Labels[Label] {
		return false
	}
	for _, owner := range job.OwnerReferences {
		if owner.Kind == "ConfigMap" && owner.Name == cm.Name && owner.UID == cm.UID {
			return true
		}
	}
	return false
}

func complete(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
