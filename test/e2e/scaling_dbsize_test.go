package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/openshift/library-go/test/library"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
)

const (
	operandNameSpace  = "openshift-etcd"
	etcdCRName        = "cluster"
	etcdLabelSelector = "app=etcd"
	quotaEnvName      = "ETCD_QUOTA_BACKEND_BYTES"
	etcdPodName       = "etcd"
)

func TestEtcdDBScaling(t *testing.T) {
	dbSize32GB := int32(32)
	opClientSet := framework.NewOperatorClient(t)
	clientSet := framework.NewClientSet("")
	etcdPodsClient := clientSet.CoreV1Interface.Pods(operandNameSpace)

	etcdCR, err := opClientSet.OperatorV1().Etcds().Get(context.Background(), etcdCRName, metav1.GetOptions{})
	require.NoError(t, err)

	etcdCR.Spec.BackendQuotaGiB = dbSize32GB
	etcdCR, err = opClientSet.OperatorV1().Etcds().Update(context.Background(), etcdCR, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = library.WaitForPodsToStabilizeOnTheSameRevision(t, etcdPodsClient, etcdLabelSelector, 3, 10*time.Second, 5*time.Second, 30*time.Minute)
	require.NoError(t, err)

	etcdPods, err := etcdPodsClient.List(context.Background(), metav1.ListOptions{LabelSelector: etcdLabelSelector})
	require.NoError(t, err)
	etcdPod := etcdPods.Items[0]
	require.True(t, etcdPod.Status.Phase == v1.PodRunning)

	assertPodsSpec(t, &etcdPod, etcdenvvar.GibibytesToBytesString(int64(dbSize32GB)))
}

func assertPodsSpec(t testing.TB, etcdPod *v1.Pod, expectedSize string) {
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
