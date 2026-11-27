package generation

import (
	"context"
	"errors"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

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

func dependencyClients(t *testing.T) (*fake.Clientset, *operatorfake.Clientset, *corev1.Node, *corev1.ConfigMap) {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "master-0", UID: "new-node", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}}}
	nodes, err := Nodes([]*corev1.Node{node})
	require.NoError(t, err)
	cm, err := (Snapshot{Version: 1, Generation: 1, Nodes: nodes}).ConfigMap()
	require.NoError(t, err)
	cm.UID = "record-1"
	cm.Annotations = map[string]string{CompletedJob: JobName(1, 1), CompletedAt: time.Now().UTC().Format(time.RFC3339), CompletedRecordUID: string(cm.UID)}
	op := operatorfake.NewSimpleClientset(&operatorv1.Etcd{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: operatorv1.EtcdStatus{StaticPodOperatorStatus: operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{Conditions: []operatorv1.OperatorCondition{{Type: ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition, Status: operatorv1.ConditionTrue}}}}}})
	return fake.NewClientset(node, cm), op, node, cm
}

func TestAfterSetupWaitsForReplacementGeneration(t *testing.T) {
	ctx := context.Background()
	kube, op, node, cm := dependencyClients(t)
	// The replacement's job already has the new UID, but the newest recorded
	// generation still describes the previous incarnation of the same name/IP.
	snapshot, err := Decode(cm)
	require.NoError(t, err)
	snapshot.Nodes[0].UID = "old-node"
	oldRecord, err := snapshot.ConfigMap()
	require.NoError(t, err)
	oldRecord.UID = cm.UID
	_, err = kube.CoreV1().ConfigMaps(Namespace).Update(ctx, oldRecord, metav1.UpdateOptions{})
	require.NoError(t, err)
	ready, err := SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err, "a stale generation must remain retryable")
	require.False(t, ready)
	snapshot.Nodes[0].UID = node.UID
	snapshot.Generation = 2
	cm, err = snapshot.ConfigMap()
	require.NoError(t, err)
	cm.UID = "record-2"
	_, err = kube.CoreV1().ConfigMaps(Namespace).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.False(t, ready, "new generation must finish before after-setup proceeds")
	cm.Annotations = map[string]string{CompletedJob: JobName(2, 1), CompletedAt: time.Now().UTC().Format(time.RFC3339), CompletedRecordUID: string(cm.UID)}
	_, err = kube.CoreV1().ConfigMaps(Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	ready, err = SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
	require.NoError(t, err)
	require.True(t, ready)
}

func TestSetupReadyReadErrors(t *testing.T) {
	for _, endpoint := range []struct{ verb, resource string }{{"get", "etcds"}, {"get", "nodes"}, {"list", "configmaps"}, {"list", "nodes"}, {"get", "jobs"}} {
		for _, failure := range []struct {
			name  string
			err   error
			retry bool
		}{
			{"timeout", apierrors.NewTimeoutError("read timed out", 1), true},
			{"server timeout", apierrors.NewServerTimeout(schema.GroupResource{Resource: endpoint.resource}, endpoint.verb, 1), true},
			{"throttled", apierrors.NewTooManyRequests("retry later", 1), true},
			{"unavailable", apierrors.NewServiceUnavailable("retry later"), true},
			{"internal", apierrors.NewInternalError(errors.New("temporary API failure")), true},
			{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: endpoint.resource}, "cluster", errors.New("denied")), false},
		} {
			t.Run(endpoint.verb+"/"+endpoint.resource+"/"+failure.name, func(t *testing.T) {
				kube, op, node, _ := dependencyClients(t)
				reactor := func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, failure.err }
				if endpoint.resource == "etcds" {
					op.PrependReactor(endpoint.verb, endpoint.resource, reactor)
				} else {
					kube.PrependReactor(endpoint.verb, endpoint.resource, reactor)
				}
				ready, err := SetupReady(context.Background(), kube, op.OperatorV1().Etcds(), node.Name, node.UID)
				require.False(t, ready)
				if failure.retry {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, failure.err)
				}
			})
		}
	}
}

func TestSetupReadyRejectsInvalidDependencyState(t *testing.T) {
	for _, kind := range []string{"malformed record", "invalid identity", "replaced execution node"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			kube, op, node, cm := dependencyClients(t)
			switch kind {
			case "malformed record":
				cm.Data[DataKey] = "invalid json"
				_, err := kube.CoreV1().ConfigMaps(Namespace).Update(ctx, cm, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "invalid identity":
				node.Status.Addresses = nil
				_, err := kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "replaced execution node":
				node = node.DeepCopy()
				node.UID = "old-node"
			}
			ready, err := SetupReady(ctx, kube, op.OperatorV1().Etcds(), node.Name, node.UID)
			require.False(t, ready)
			require.Error(t, err)
		})
	}
}
