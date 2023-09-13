package e2e

import (
	"context"
	"fmt"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	masterNodeLabel = "node-role.kubernetes.io/master"
	backupPath      = "/etc/kubernetes/cluster-backup"
)

func TestBackupScript(t *testing.T) {
	clientSet := framework.NewClientSet("")
	masterNodes, err := clientSet.CoreV1Interface.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: masterNodeLabel})
	require.NoErrorf(t, err, "error while listing master nodes")

	// create debug pod on first master node
	// see https://www.redhat.com/sysadmin/how-oc-debug-works
	debugNodeName := masterNodes.Items[0].Name
	go runDebugPod(t, debugNodeName)
	time.Sleep(500 * time.Millisecond)

	debugPodName := debugNodeName + "-debug"
	debugPodName = strings.ReplaceAll(debugPodName, ".", "-")

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
	debugArgs := strings.Split(fmt.Sprintf("debug node/%s %s %s", debugNodeName, "--as-root=true", "-- sleep 200s"), " ")
	err := exec.Command("oc", debugArgs...).Run()
	require.NoError(t, err)
}
