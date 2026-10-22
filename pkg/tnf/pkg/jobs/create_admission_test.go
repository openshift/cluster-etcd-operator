package jobs

import (
	"context"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/clock"
)

func TestDeferredCreationPreservesCompletedStatus(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	op := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
		&operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{Generations: []operatorv1.GenerationStatus{{Group: "batch", Resource: "jobs", Namespace: operatorclient.TargetNamespace, Name: "tnf-setup-job", LastGeneration: 0}}, Conditions: []operatorv1.OperatorCondition{
			{Type: "TNFSetupJobAvailable", Status: operatorv1.ConditionTrue, Reason: "JobComplete"},
			{Type: "TNFSetupJobProgressing", Status: operatorv1.ConditionFalse},
		}}}, nil, nil)
	recorder := events.NewInMemoryRecorder("admission", clock.RealClock{})
	image := "operator:original"
	c := NewJobControllerBuilder("tnf-setup-job", bindata.MustAsset("tnfdeployment/job.yaml"), recorder, op, client, informers.NewSharedInformerFactory(client, 0).Batch().V1().Jobs()).
		WithConditions(AllConditions...).
		WithJobHooks(func(_ *operatorv1.OperatorSpec, job *batchv1.Job) (bool, error) {
			job.Name = "tnf-setup-job"
			job.Spec.Template.Spec.Containers[0].Image = image
			return true, nil
		})
	allowed := false
	c.WithCreateAdmissionHook(func(context.Context, *batchv1.Job) (bool, error) { return allowed, nil })
	syncCtx := factory.NewSyncContext("admission", recorder)
	sync := func() error {
		spec, status, _, err := op.GetOperatorState()
		if err != nil {
			return err
		}
		return c.syncManaged(ctx, spec, status, syncCtx)
	}
	spec, _, _, err := op.GetOperatorState()
	require.NoError(t, err)
	required, err := c.getJob(spec)
	require.NoError(t, err)
	job, _, err := ApplyJob(ctx, client.BatchV1(), recorder, required, -1)
	require.NoError(t, err)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	_, err = client.BatchV1().Jobs(job.Namespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, sync())
	image = "operator:replacement"
	require.ErrorContains(t, sync(), "old job is deleted")
	_, status, _, err := op.GetOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionTrue(status.Conditions, "TNFSetupJobAvailable"), "drift deletion must not clear completion")
	require.True(t, v1helpers.IsOperatorConditionFalse(status.Conditions, "TNFSetupJobProgressing"))
	require.NoError(t, sync())
	_, status, _, err = op.GetOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionTrue(status.Conditions, "TNFSetupJobAvailable"))
	require.True(t, v1helpers.IsOperatorConditionFalse(status.Conditions, "TNFSetupJobProgressing"))
	_, err = client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, "tnf-setup-job", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	allowed = true
	require.NoError(t, sync())
	_, err = client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, "tnf-setup-job", metav1.GetOptions{})
	require.NoError(t, err)
}
