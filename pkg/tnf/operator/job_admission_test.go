package operator

import (
	"context"
	"testing"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

func TestSetupCreationAdmissionUsesTransitionAndReadiness(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "tnf-setup-job"}}
	allowed, err := c.admitSetupJob(ctx, job)
	require.NoError(t, err)
	require.False(t, allowed)
	_, _, err = v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{Type: ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition, Status: operatorv1.ConditionFalse}))
	require.NoError(t, err)
	allowed, err = c.admitSetupJob(ctx, job)
	require.NoError(t, err)
	require.True(t, allowed)
	node := mustListGenerationNodes(t, c)[0].DeepCopy()
	node.Status.Conditions[0].Status = corev1.ConditionFalse
	require.NoError(t, c.controlPlaneNodeInformer.GetIndexer().Update(node))
	allowed, err = c.admitSetupJob(ctx, job)
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestGenerationExecutorDriftCancelsPendingAndResetsFailedBudget(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	snapshot, err := generation.Decode(&records[0])
	require.NoError(t, err)
	// A pending attempt near exhaustion loses its eligible executor.
	job := generationJob(&records[0], snapshot, "master-0", generationAttempts-1)
	job.UID = "old-attempt"
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).Create(ctx, job, metav1.CreateOptions{})
	require.NoError(t, err)
	obj, _, err := c.pacemakerInformer.GetIndexer().GetByKey(pacemaker.PacemakerClusterResourceName)
	require.NoError(t, err)
	cr := obj.(*pacmkrv1.PacemakerCluster).DeepCopy()
	members := (*cr.Status.Nodes)[1:]
	cr.Status.Nodes = &members
	require.NoError(t, c.pacemakerInformer.GetIndexer().Update(cr))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending-a", Namespace: generation.Namespace, Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID), "app.kubernetes.io/name": "tnf-update-setup-job"}}, Spec: corev1.PodSpec{NodeName: "master-0"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	_, err = c.kubeClient.CoreV1().Pods(generation.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "cancel stale pending work without waiting for its deadline")
	require.NoError(t, c.reconcileGeneration(ctx))
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, generationAttempts), metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the surviving Pod must prevent overlapping attempts")
	pod.Status.Phase = corev1.PodFailed
	_, err = c.kubeClient.CoreV1().Pods(generation.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	restarted := pacemakerLifecycleManager{kubeClient: c.kubeClient, operatorClient: c.operatorClient, controlPlaneNodeInformer: c.controlPlaneNodeInformer, pacemakerInformer: c.pacemakerInformer}
	require.NoError(t, restarted.reconcileGeneration(ctx))
	next, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, generationAttempts), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "master-1", next.Spec.Template.Spec.NodeName)
	require.Equal(t, "1", next.Labels[retryAttemptLabel], "the new executor gets a fresh retry budget")
	current, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, current, 1)
	require.Equal(t, records[0].Data, current[0].Data, "placement changes retain the recorded membership")
}

func TestGenerationAdmissionDefersEveryRoutineRetry(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	job, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 1), metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, "master-0", metav1.GetOptions{})
	require.NoError(t, err)
	node.Annotations = map[string]string{"machineconfiguration.openshift.io/state": "Working"}
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 2), metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "a routine retry must wait for MCD")
	node.Annotations["machineconfiguration.openshift.io/state"] = "Done"
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	next, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 2), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "2", next.Labels[retryAttemptLabel], "deferral must not consume a retry")
}
