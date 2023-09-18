package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	masterNodeLabel = "node-role.kubernetes.io/master"
	backupPath      = "/etc/kubernetes/backup-happy-path-2023-08-03_152313"
	debugNamespace  = "default"
	backupName      = "backup-happy-path"
)

func TestBackupScript(t *testing.T) {
	clientSet := framework.NewClientSet("")
	masterNodes, err := clientSet.CoreV1Interface.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: masterNodeLabel})
	require.NoErrorf(t, err, "error while listing master nodes")

	// create debug pod on first master node
	// see https://www.redhat.com/sysadmin/how-oc-debug-works
	debugNodeName := masterNodes.Items[0].Name
	var debugPodName string

	go runDebugPod(t, debugNodeName)

	// wait for debug pod to be in Running phase
	err = wait.PollUntilContextTimeout(context.Background(), time.Second, 30*time.Minute, true, func(ctx context.Context) (done bool, err error) {
		pods, err := clientSet.CoreV1Interface.Pods(debugNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		for _, pod := range pods.Items {
			if strings.Contains(pod.Name, "debug") && pod.Status.Phase == v1.PodRunning {
				debugPodName = pod.Name
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err)

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
	requireBackupFilesFound(t, "backup-happy-path", trimFilesPrefix(files))

	t.Cleanup(func() {
		// clean up
		cmdAsStr = fmt.Sprintf("rm -rf /host%s", backupPath)
		output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
		require.NoError(t, err, fmt.Errorf("cleanup failed: %s", string(output)))
	})
}

func getOcArgs(podName, cmdAsStr string) []string {
	return strings.Split(fmt.Sprintf("rsh -n default %s %s", podName, cmdAsStr), " ")
}

func runDebugPod(t *testing.T, debugNodeName string) {
	debugArgs := strings.Split(fmt.Sprintf("debug node/%s %s %s %s", debugNodeName, "--to-namespace=default", "--as-root=true", "-- sleep 1800s"), " ")
	output, err := exec.Command("oc", debugArgs...).CombinedOutput()
	require.NoErrorf(t, err, string(output))
}

func trimFilesPrefix(files []string) []string {
	trimmedFiles := make([]string, len(files))
	for i, f := range files {
		trimmedFiles[i], _ = strings.CutPrefix(f, "/host/etc/kubernetes/")
		trimmedFiles[i] = "./backup-" + trimmedFiles[i]
	}
	return trimmedFiles
}
