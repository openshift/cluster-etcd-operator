package e2e

import (
	"context"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"

	"github.com/stretchr/testify/require"
)

const (
	operatorNamespace      = "openshift-etcd-operator"
	operatorDeploymentName = "etcd-operator"
	operatorUpdatedImage   = "quay.io/melbeher/ceo-1900"
	operatorImageKey       = "OPERATOR_IMAGE"
	operandNameSpace       = "openshift-etcd"
	etcdCRName             = "cluster"
	etcdLabelSelector      = "app=etcd"
	quotaEnvName           = "ETCD_QUOTA_BACKEND_BYTES"
	etcdPodName            = "etcd"
)

func TestEtcdDBScaling(t *testing.T) {
	dbSize32GB := int32(32)
	opClientSet := framework.NewOperatorClient(t)
	clientSet := framework.NewClientSet("")

	etcdCR, err := opClientSet.OperatorV1().Etcds().Get(context.Background(), etcdCRName, metav1.GetOptions{})
	require.NoError(t, err)

	etcdCR.Spec.BackendQuotaGiB = dbSize32GB
	etcdCR, err = opClientSet.OperatorV1().Etcds().Update(context.Background(), etcdCR, metav1.UpdateOptions{})
	require.NoError(t, err)

	var etcdPod v1.Pod
	// wait for new etcd pods to be rolled out
	err = wait.PollUntilContextTimeout(context.Background(), time.Second, 10*time.Minute, true, func(ctx context.Context) (done bool, err error) {
		etcdPods, err := clientSet.Pods(operandNameSpace).List(context.Background(), metav1.ListOptions{LabelSelector: etcdLabelSelector})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		for _, pod := range etcdPods.Items {
			if pod.Status.Phase == v1.PodRunning && time.Now().Sub(pod.CreationTimestamp.Time) < 1*time.Minute {
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err)

	assertPodsSpec(t, etcdPod, etcdenvvar.GibibytesToBytesString(dbSize32GB))
}

func assertPodsSpec(t testing.TB, etcdPod v1.Pod, expectedSize string) {
	t.Helper()

	var etcdContainer v1.Container
	for _, c := range etcdPod.Spec.Containers {
		if c.Name == etcdPodName {
			etcdContainer = c
			break
		}
	}
	for _, env := range etcdContainer.Env {
		if env.Name == quotaEnvName {
			require.Equal(t, expectedSize, env.Value)
			return
		}
	}
}
