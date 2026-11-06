package generation

import (
	"context"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
)

func TestAfterSetupRequiresItsCompletedGeneration(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-0", UID: "a", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}}}
	nodes, err := Nodes([]*corev1.Node{node})
	require.NoError(t, err)
	s := Snapshot{Version: 1, Generation: 1, Nodes: nodes}
	cm, err := s.ConfigMap()
	require.NoError(t, err)
	cm.UID = "record-1"
	cm.Annotations = map[string]string{CompletedJob: JobName(1, 1)}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: JobName(1, 1), Namespace: Namespace, Labels: map[string]string{Label: "1"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ConfigMap", Name: cm.Name, UID: cm.UID}}}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
	kube := fake.NewClientset(node, cm, job)
	op := operatorfake.NewSimpleClientset(&operatorv1.Etcd{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: operatorv1.EtcdStatus{StaticPodOperatorStatus: operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{Conditions: []operatorv1.OperatorCondition{{Type: ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition, Status: operatorv1.ConditionTrue}}}}}})
	ready, err := SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.True(t, ready)
	// Once the controller has bound completion to this record UID, removing
	// the successful Job must not strand after-setup behind a completed generation.
	require.NoError(t, kube.BatchV1().Jobs(Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{}))
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.False(t, ready, "a Job name alone is not durable completion proof")
	cm.Annotations[CompletedAt] = time.Now().UTC().Format(time.RFC3339)
	cm.Annotations[CompletedRecordUID] = string(cm.UID)
	_, err = kube.CoreV1().ConfigMaps(Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.True(t, ready)
	// Copied completion cannot certify a recreated ConfigMap with a new UID.
	cm.UID = "recreated-record-1"
	_, err = kube.CoreV1().ConfigMaps(Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.False(t, ready)
	// A completed generation one must not satisfy generation two even for the same node UID.
	s.Generation = 2
	newRecord, err := s.ConfigMap()
	require.NoError(t, err)
	newRecord.UID = "record-2"
	_, err = kube.CoreV1().ConfigMaps(Namespace).Create(ctx, newRecord, metav1.CreateOptions{})
	require.NoError(t, err)
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.False(t, ready)
	// Fencing uses the durable transition condition even when the bootstrap Job is gone.
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), "", "")
	require.NoError(t, err)
	require.True(t, ready)
}
