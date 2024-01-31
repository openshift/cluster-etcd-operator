package e2e

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
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
	dbSize64GB := int64(64)
	opClientSet := framework.NewOperatorClient(t)
	clientSet := framework.NewClientSet("")

	etcdCR, err := opClientSet.OperatorV1().Etcds().Get(context.Background(), etcdCRName, metav1.GetOptions{})
	require.NoError(t, err)

	etcdCR.Spec.BackendQuotaGiB = dbSize64GB
	etcdCR, err = opClientSet.OperatorV1().Etcds().Update(context.Background(), etcdCR, metav1.UpdateOptions{})
	require.NoError(t, err)

	scaleCVODown(t)
	applyEtcdCRD(t)
	scaleCEODown(t)
	updateCEOImage(t, clientSet)
	scaleCEOUp(t)

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

	assertPodsSpec(t, etcdPod, strconv.FormatInt(dbSize64GB, 10))
}

func scaleCVODown(t testing.TB) {
	t.Helper()

	cmdAsArgs := strings.Split("scale -n openshift-cluster-version deployment/cluster-version-operator --replicas=0", " ")
	output, err := exec.Command("oc", cmdAsArgs...).CombinedOutput()
	require.NoErrorf(t, err, string(output))
}

func scaleCEODown(t testing.TB) {
	t.Helper()

	cmdAsArgs := strings.Split("scale -n openshift-etcd-operator deployment.apps/etcd-operator --replicas=0", " ")
	output, err := exec.Command("oc", cmdAsArgs...).CombinedOutput()
	require.NoErrorf(t, err, string(output))
}

func scaleCEOUp(t testing.TB) {
	t.Helper()

	cmdAsArgs := strings.Split("scale -n openshift-etcd-operator deployment.apps/etcd-operator --replicas=1", " ")
	output, err := exec.Command("oc", cmdAsArgs...).CombinedOutput()
	require.NoErrorf(t, err, string(output))
}

func updateCEOImage(t testing.TB, clientSet *framework.ClientSet) {
	deploy, err := clientSet.AppsV1Interface.Deployments(operatorNamespace).Get(context.Background(), operatorDeploymentName, metav1.GetOptions{})
	require.NoError(t, err)

	deploy.Spec.Template.Spec.Containers[0].Image = operatorUpdatedImage
	for idx, env := range deploy.Spec.Template.Spec.Containers[0].Env {
		if env.Name == operatorImageKey {
			deploy.Spec.Template.Spec.Containers[0].Env[idx].Value = operatorUpdatedImage
			break
		}
	}

	_, err = clientSet.AppsV1Interface.Deployments(operatorNamespace).Update(context.Background(), deploy, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func applyEtcdCRD(t testing.TB) {
	t.Helper()

	cmdAsArgs := strings.Split("apply -f /Users/mustafa/workspace/go/src/etcd/cluster-etcd-operator/vendor/github.com/openshift/api/operator/v1/0000_12_etcd-operator_01_config-TechPreviewNoUpgrade.crd.yaml", " ")
	output, err := exec.Command("oc", cmdAsArgs...).CombinedOutput()
	require.NoErrorf(t, err, string(output))
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
