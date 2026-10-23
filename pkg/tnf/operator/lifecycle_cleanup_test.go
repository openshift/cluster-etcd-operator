package operator

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func TestCleanupRetiresNodeJobsAndOrphanPods(t *testing.T) {
	ctx := context.Background()
	c := generationManager(t)
	jobClient := c.kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	for _, fixture := range []struct{ name, kind, nodeName, nodeUID string }{
		{"current-auth", tools.JobTypeAuth.GetNameLabelValue(), "master-0", "uid-0"},
		{"replaced-after-setup", tools.JobTypeAfterSetup.GetNameLabelValue(), "master-1", "old-uid"},
		{"deleted-auth", tools.JobTypeAuth.GetNameLabelValue(), "deleted-node", "deleted-uid"},
		{"completed-generation", tools.JobTypeUpdateSetup.GetNameLabelValue(), "", ""},
		{"collector", pacemakerStatusCollectorName, "", ""},
	} {
		_, err := jobClient.Create(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: fixture.name, UID: types.UID(fixture.name), Labels: map[string]string{"app.kubernetes.io/name": fixture.kind, "node": fixture.nodeUID}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: fixture.nodeName}}}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	podClient := c.kubeClient.CoreV1().Pods(operatorclient.TargetNamespace)
	for _, fixture := range []struct {
		name, owner, ownerUID string
		phase                 corev1.PodPhase
	}{
		{"current-pod", "current-auth", "current-auth", corev1.PodRunning},
		{"old-owner-pod", "current-auth", "old-job-uid", corev1.PodRunning},
		{"orphan-pod", "gone-job", "gone-job", corev1.PodRunning},
		{"collector-pod", "collector", "collector", corev1.PodRunning},
		{"unowned-finished", "", "", corev1.PodSucceeded},
		{"unowned-running", "", "", corev1.PodRunning},
	} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fixture.name, UID: types.UID(fixture.name), Labels: map[string]string{"app": "tnf-job"}}, Status: corev1.PodStatus{Phase: fixture.phase}}
		if fixture.owner != "" {
			pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: fixture.owner, UID: types.UID(fixture.ownerUID)}}, batchv1.SchemeGroupVersion.WithKind("Job"))}
		}
		_, err := podClient.Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	failNodes := true
	client := c.kubeClient.(*fake.Clientset)
	client.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failNodes {
			return true, nil, fmt.Errorf("node API unavailable")
		}
		return false, nil, nil
	})
	require.ErrorContains(t, c.cleanupOrphanedJobs(ctx), "node API unavailable")
	for _, action := range client.Actions() {
		require.NotEqual(t, "delete", action.GetVerb(), "do not infer retired nodes when node lookup fails")
	}
	failNodes = false
	require.NoError(t, c.cleanupOrphanedJobs(ctx))
	jobs, err := jobClient.List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	jobNames := []string{}
	for _, job := range jobs.Items {
		jobNames = append(jobNames, job.Name)
	}
	require.ElementsMatch(t, []string{"current-auth", "completed-generation", "collector"}, jobNames)
	pods, err := podClient.List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	podNames := []string{}
	for _, pod := range pods.Items {
		podNames = append(podNames, pod.Name)
	}
	require.ElementsMatch(t, []string{"current-pod", "collector-pod", "unowned-running"}, podNames)
}
