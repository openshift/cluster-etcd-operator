package e2e

import (
	"context"
	"testing"
	"time"

	g "github.com/onsi/ginkgo/v2"
	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/defaultbackuppolicyinitializer"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {
	g.It("[Operator][OCPFeatureGate:AutomatedEtcdBackup] should initialize the default backup policy [Timeout:15m]", func() {
		TestDefaultBackupPolicyInitialization(g.GinkgoTB())
	})
})

// This test requires a fresh cluster: administrators may legitimately edit or delete the default policy.
func TestDefaultBackupPolicyInitialization(t testing.TB) {
	const policyName = "default"

	operatorClient := framework.NewOperatorClient(t)
	batchClient := framework.NewBatchClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var policy *operatorv1alpha1.EtcdBackupPolicy
	var cronJob *batchv1.CronJob

	g.By("Waiting for the default policy, initialization condition, and CronJob")
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var err error
		policy, err = operatorClient.OperatorV1alpha1().EtcdBackupPolicies().Get(ctx, policyName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}

		etcd, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		initialized := false
		for _, condition := range etcd.Status.Conditions {
			if condition.Type == defaultbackuppolicyinitializer.DefaultBackupPolicyInitializedCondition && condition.Status == operatorv1.ConditionTrue {
				initialized = true
				break
			}
		}
		if !initialized {
			return false, nil
		}

		cronJob, err = batchClient.CronJobs(operatorclient.TargetNamespace).Get(ctx, policyName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}

		return true, nil
	})
	require.NoError(t, err, "default backup policy, initialization condition and CronJob did not become ready")

	g.By("Verifying the default policy configuration")
	require.Equal(t, "@daily", policy.Spec.Schedule)
	require.Equal(t, operatorv1alpha1.EtcdBackupStorageTypeLocal, policy.Spec.Storage.Type)
	require.Equal(t, "/etc/kubernetes/cluster-backup", policy.Spec.Storage.Local.HostPath)
	require.Equal(t, operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes, policy.Spec.Storage.Local.Distribution)
	require.Len(t, policy.Spec.RetentionRules, 1)
	require.Equal(t, operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeQuantity, policy.Spec.RetentionRules[0].Type)
	require.EqualValues(t, 2, policy.Spec.RetentionRules[0].Quantity.MaxQuantity)
	require.Equal(t, policy.Spec.Schedule, cronJob.Spec.Schedule)
}
