package operator

import (
	"context"
	"fmt"
	"testing"
	"time"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

func generationManager(t *testing.T) *pacemakerLifecycleManager {
	t.Helper()
	t.Setenv("OPERATOR_IMAGE", "operator:v1")
	objects := []runtime.Object{}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	pmNodes := []pacmkrv1.PacemakerClusterNodeStatus{}
	for i := 0; i < 2; i++ {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("master-%d", i), UID: types.UID(fmt.Sprintf("uid-%d", i)), Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: fmt.Sprintf("10.0.0.%d", i+1)}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		require.NoError(t, indexer.Add(node))
		objects = append(objects, node)
		pmNodes = append(pmNodes, pacmkrv1.PacemakerClusterNodeStatus{NodeName: node.Name, Addresses: []pacmkrv1.PacemakerNodeAddress{{Address: node.Status.Addresses[0].Address}}})
	}
	client := fake.NewClientset(objects...)
	client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		cm := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap)
		cm.UID = types.UID(cm.Name)
		return false, nil, nil
	})
	pmIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, pmIndexer.Add(&pacmkrv1.PacemakerCluster{ObjectMeta: metav1.ObjectMeta{Name: pacemaker.PacemakerClusterResourceName}, Status: pacmkrv1.PacemakerClusterStatus{Nodes: &pmNodes, LastUpdated: metav1.Now()}}))
	op := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}}, &operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{Conditions: []operatorv1.OperatorCondition{{Type: ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition, Status: operatorv1.ConditionTrue}}}}, nil, nil)
	return &pacemakerLifecycleManager{kubeClient: client, operatorClient: op, controlPlaneNodeInformer: &mockNodeInformer{indexer: indexer, synced: true}, pacemakerInformer: &mockPacemakerInformer{indexer: pmIndexer}}
}

func TestGenerationPersistsAcrossRestartAndReplacement(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx)) // Persist before execution.
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 1)
	copy := pacemakerLifecycleManager{kubeClient: c.kubeClient, operatorClient: c.operatorClient, controlPlaneNodeInformer: c.controlPlaneNodeInformer, pacemakerInformer: c.pacemakerInformer} // Simulate restart.
	require.NoError(t, copy.reconcileGeneration(ctx))
	job, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 1), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "master-0", job.Spec.Template.Spec.NodeName)
	require.Equal(t, string(records[0].UID), job.Spec.Template.Spec.Containers[0].Env[len(job.Spec.Template.Spec.Containers[0].Env)-1].Value)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, job.Name, records[0].Annotations[generation.CompletedJob])
	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, "master-1", metav1.GetOptions{})
	require.NoError(t, err)

	node.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.controlPlaneNodeInformer.GetIndexer().Update(node))
	require.NoError(t, copy.reconcileGeneration(ctx))
	list, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "readiness changes must not rerun a completed generation")
	require.NoError(t, c.kubeClient.BatchV1().Jobs(generation.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{}))
	require.NoError(t, copy.reconcileGeneration(ctx))
	list, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, list.Items, "recorded completion survives Job cleanup")
	node.Status.Conditions[0].Status = corev1.ConditionTrue
	node.UID = "replacement"
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.controlPlaneNodeInformer.GetIndexer().Update(node))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 2)
	s, err := generation.Decode(&records[1])
	require.NoError(t, err)
	require.Equal(t, types.UID("replacement"), s.Nodes[1].UID)
	require.Equal(t, types.UID("uid-1"), s.Previous[1].UID)
}

func TestGenerationDefersRebootButPermitsMissingMember(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, "master-1", metav1.GetOptions{})
	require.NoError(t, err)
	node.Annotations = map[string]string{"machineconfiguration.openshift.io/currentConfig": "old", "machineconfiguration.openshift.io/desiredConfig": "new"}
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Empty(t, records)
	obj, _, err := c.pacemakerInformer.GetIndexer().GetByKey(pacemaker.PacemakerClusterResourceName)
	require.NoError(t, err)
	cr := obj.(*pacmkrv1.PacemakerCluster).DeepCopy()
	nodes := (*cr.Status.Nodes)[:1]
	cr.Status.Nodes = &nodes
	require.NoError(t, c.pacemakerInformer.GetIndexer().Update(cr))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 1)
	s, err := generation.Decode(&records[0])
	require.NoError(t, err)
	require.True(t, s.Repair)
	require.NoError(t, c.reconcileGeneration(ctx))
	jobClient := c.kubeClient.BatchV1().Jobs(generation.Namespace)
	job, err := jobClient.Get(ctx, generation.JobName(1, 1), metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	_, err = jobClient.UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.pacemakerInformer.GetIndexer().Delete(cr))
	require.NoError(t, c.reconcileGeneration(ctx))
	_, err = jobClient.Get(ctx, generation.JobName(1, 2), metav1.GetOptions{})
	require.NoError(t, err, "a previously started recorded repair can inspect PCS even if status disappears during MCD")
}

func TestSuccessfulGenerationDoesNotHideNewDrift(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	job, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 1), metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-3 * time.Minute)}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	obj, _, err := c.pacemakerInformer.GetIndexer().GetByKey(pacemaker.PacemakerClusterResourceName)
	require.NoError(t, err)
	cr := obj.(*pacmkrv1.PacemakerCluster).DeepCopy()
	members := (*cr.Status.Nodes)[:1]
	cr.Status.Nodes = &members
	cr.Status.LastUpdated = metav1.Now()
	require.NoError(t, c.pacemakerInformer.GetIndexer().Update(cr))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 2)
	s, err := generation.Decode(&records[1])
	require.NoError(t, err)
	require.True(t, s.Repair)
}

func TestGenerationDiscoveryWithoutPacemakerStatus(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	indexer := c.pacemakerInformer.GetIndexer()
	cr, _, err := indexer.GetByKey(pacemaker.PacemakerClusterResourceName)
	require.NoError(t, err)
	require.NoError(t, indexer.Delete(cr))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 1)
	snapshot, err := generation.Decode(&records[0])
	require.NoError(t, err)
	require.False(t, snapshot.Repair, "missing status does not prove membership drift")

	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, "master-0", metav1.GetOptions{})
	require.NoError(t, err)
	node.Annotations = map[string]string{"machineconfiguration.openshift.io/state": "Working"}
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	emptyCR := cr.(*pacmkrv1.PacemakerCluster).DeepCopy()
	emptyMembers := []pacmkrv1.PacemakerClusterNodeStatus{}
	emptyCR.Status.Nodes = &emptyMembers
	require.NoError(t, indexer.Add(emptyCR))
	require.NoError(t, c.reconcileGeneration(ctx))
	jobClient := c.kubeClient.BatchV1().Jobs(generation.Namespace)
	list, err := jobClient.List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, list.Items, "unknown membership must not bypass MCD admission")
	require.NoError(t, indexer.Delete(emptyCR))
	node.Annotations = nil
	_, err = c.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	job, err := jobClient.Get(ctx, generation.JobName(1, 1), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "master-0", job.Spec.Template.Spec.NodeName)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	_, err = jobClient.UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	job, err = jobClient.Get(ctx, generation.JobName(1, 2), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "master-1", job.Spec.Template.Spec.NodeName, "try the next Ready candidate after failure")
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	_, err = jobClient.UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 2)
	snapshot, err = generation.Decode(&records[1])
	require.NoError(t, err)
	require.True(t, snapshot.Discovery, "verify unchanged desired state in a new generation")
	require.NoError(t, c.reconcileGeneration(ctx))
	job, err = jobClient.Get(ctx, generation.JobName(2, 1), metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	_, err = jobClient.UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 2, "a collector outage must not create a new discovery job every sync")
}

func TestFailedGenerationRemainsDegradedUntilSuccess(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	s, err := generation.Decode(&records[0])
	require.NoError(t, err)
	failed := generationJob(&records[0], s, "master-1", generationAttempts)
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).Create(ctx, failed, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	_, status, _, err := c.operatorClient.GetOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionTrue(status.Conditions, "TNFUpdateSetupJobDegraded"))
	failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, failed, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	_, status, _, err = c.operatorClient.GetOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionTrue(status.Conditions, "TNFUpdateSetupJobDegraded"))
	job, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, generationAttempts+1), metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	_, status, _, err = c.operatorClient.GetOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionFalse(status.Conditions, "TNFUpdateSetupJobDegraded"))
}

func TestGenerationRetiresLegacyJobBeforeRecording(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	legacy := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "tnf-update-setup-job", Namespace: generation.Namespace, Labels: map[string]string{"app.kubernetes.io/name": "tnf-update-setup-job"}}}
	_, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Create(ctx, legacy, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Empty(t, records, "legacy execution must stop first")
	legacy.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, legacy, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	list, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, list.Items)
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Empty(t, records, "legacy success cannot prove a recorded generation")
	legacyPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "legacy-orphan", Namespace: generation.Namespace, Labels: map[string]string{"app": "tnf-job", "batch.kubernetes.io/job-name": legacy.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	_, err = c.kubeClient.CoreV1().Pods(generation.Namespace).Create(ctx, legacyPod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Empty(t, records, "a legacy Pod without the newer application label must stop before migration")
	require.NoError(t, c.kubeClient.CoreV1().Pods(generation.Namespace).Delete(ctx, legacyPod.Name, metav1.DeleteOptions{}))
	require.NoError(t, c.reconcileGeneration(ctx))
	records, err = generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 1)
}

func mustListGenerationNodes(t *testing.T, c *pacemakerLifecycleManager) []*corev1.Node {
	t.Helper()
	objects := c.controlPlaneNodeInformer.GetIndexer().List()
	nodes := make([]*corev1.Node, 0, len(objects))
	for _, obj := range objects {
		nodes = append(nodes, obj.(*corev1.Node))
	}
	return nodes
}
