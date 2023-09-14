package e2e

import (
	"context"
	"fmt"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	masterNodeLabel = "node-role.kubernetes.io/master"
	backupPath      = "/etc/kubernetes/cluster-backup"
	debugNamespace  = "default"
)

func TestBackupScript(t *testing.T) {
	clientSet := framework.NewClientSet("")
	masterNodes, err := clientSet.CoreV1Interface.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: masterNodeLabel})
	require.NoErrorf(t, err, "error while listing master nodes")

	// create debug pod on first master node
	// see https://www.redhat.com/sysadmin/how-oc-debug-works
	debugNodeName := masterNodes.Items[0].Name
	debugPodName := debugNodeName + "-debug"
	debugPodName = strings.ReplaceAll(debugPodName, ".", "-")

	go runDebugPod(t, debugNodeName)

	// wait for debug pod to be in Running phase
	err = wait.PollUntilContextTimeout(context.Background(), time.Second, 30*time.Minute, true, func(ctx context.Context) (done bool, err error) {
		pods, err := clientSet.CoreV1Interface.Pods(debugNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			klog.Infof("error listing pods within default namespace '%v'\n", err)
			return false, err
		}

		for _, pod := range pods.Items {
			if pod.Name == debugPodName && pod.Status.Phase == v1.PodRunning {
				return true, nil
			}
		}
		klog.Infof("could not find debug pod within default namespace\n")
		return false, nil
	})
	klog.Infof("polling finished with this error '%v'", err)
	require.NoErrorf(t, err, err.Error())

	// verify no backup exist
	cmdAsStr := fmt.Sprintf("ls -l /host%s", backupPath)
	output, err := exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
	require.Errorf(t, err, string(output))

	// run backup
	cmdAsStr = fmt.Sprintf("chroot /host /bin/bash -euxo pipefail /usr/local/bin/cluster-backup.sh --force %s", backupPath)
	output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
	require.NoErrorf(t, err, string(output))

	// verify backup created
	cmdAsStr = fmt.Sprintf("find /host%s", backupPath)
	output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
	require.NoError(t, err)
	files := strings.Split(string(output), "\n")
	requireBackupFilesFound(t, "", files)

	// clean up
	cmdAsStr = fmt.Sprintf("rm -rf /host%s", backupPath)
	output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
	require.NoError(t, err, fmt.Errorf("cleanup failed: %s", string(output)))
}

func getOcArgs(podName, cmdAsStr string) []string {
	return strings.Split(fmt.Sprintf("rsh -n default %s %s", podName, cmdAsStr), " ")
}

func runDebugPod(t *testing.T, debugNodeName string) {
	debugArgs := strings.Split(fmt.Sprintf("debug node/%s %s %s", debugNodeName, "--as-root=true", "-- sleep 1800s"), " ")
	output, err := exec.Command("oc", debugArgs...).CombinedOutput()
	require.NoErrorf(t, err, string(output))
}
