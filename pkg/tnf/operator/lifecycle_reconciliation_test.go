package operator

import (
	"context"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	pacmkrv1 "github.com/openshift/api/etcd/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
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
	return &pacemakerLifecycleManager{kubeClient: client, operatorClient: op, controlPlaneNodeInformer: &mockNodeInformer{indexer: indexer, synced: true}, pacemakerInformer: &mockPacemakerInformer{indexer: pmIndexer}, configClient: configfake.NewSimpleClientset(&configv1.ClusterVersion{ObjectMeta: metav1.ObjectMeta{Name: "version"}}).ConfigV1()}
}

func TestGenerationPersistsAcrossRestartAndReplacement(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx)) // Persist before execution.
	records, err := generation.Records(ctx, c.kubeClient)
	require.NoError(t, err)
	require.Len(t, records, 1)
	copy := pacemakerLifecycleManager{kubeClient: c.kubeClient, operatorClient: c.operatorClient, controlPlaneNodeInformer: c.controlPlaneNodeInformer, pacemakerInformer: c.pacemakerInformer, configClient: c.configClient} // Simulate restart.
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

func TestGenerationWaitsForFailedPodsAndRetriesOnPeer(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	require.NoError(t, c.reconcileGeneration(ctx))
	require.NoError(t, c.reconcileGeneration(ctx))
	job, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 1), metav1.GetOptions{})
	require.NoError(t, err)
	job.UID = "attempt-1"
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue}}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "terminating", Namespace: generation.Namespace, Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	_, err = c.kubeClient.CoreV1().Pods(generation.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	list, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	pod.Status.Phase = corev1.PodFailed
	_, err = c.kubeClient.CoreV1().Pods(generation.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	peer, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(1, 2), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "master-1", peer.Spec.Template.Spec.NodeName)
}

func TestGenerationDefersRebootButPermitsMissingMember(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	cv, err := c.configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	require.NoError(t, err)
	cv.Status.Conditions = []configv1.ClusterOperatorStatusCondition{{Type: configv1.OperatorProgressing, Status: configv1.ConditionTrue}}
	_, err = c.configClient.ClusterVersions().UpdateStatus(ctx, cv, metav1.UpdateOptions{})
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
	job, err := c.kubeClient.BatchV1().Jobs(generation.Namespace).Get(ctx, generation.JobName(2, 1), metav1.GetOptions{})
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = c.kubeClient.BatchV1().Jobs(generation.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.reconcileGeneration(ctx))
	_, status, _, err = c.operatorClient.GetOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionFalse(status.Conditions, "TNFUpdateSetupJobDegraded"))
}
