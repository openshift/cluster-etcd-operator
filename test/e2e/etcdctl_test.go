package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestEtcdctlCommands executes all known etcdctl commands inside of the etcdctl container.
// The test is not intended to be a functional test yet a sanity test that the container
// ENV is populated correctly and that etcdctl consumes that ENV properly.
func TestEtcdctlCommands(t *testing.T) {
	// setup
	cs := framework.NewClientSet("")
	pod, err := cs.CoreV1Interface.Pods("").List(context.TODO(), metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	require.NoError(t, err)
	podName := pod.Items[0].Name

	testCases := []struct {
		command       string
		skip          bool
		skipReason    string
		expectedError string
	}{
		{"etcdctl alarm disarm", false, "", ""},
		{"etcdctl alarm list", false, "", ""},
		{"etcdctl check perf", true, "OpenShift disabled command", ""},
		{"etcdctl compaction 0", false, "", "Error: etcdserver: mvcc: required revision has been compacted"},
		{"etcdctl defrag", false, "", ""},
		{"etcdctl elect myelection", false, "", "no proposal argument but -l not set"},
		// endpoint
		{"etcdctl endpoint hashkv", false, "", ""},
		{"etcdctl endpoint health", false, "", ""},
		{"etcdctl endpoint status", false, "", ""},
		// lease
		{"etcdctl lease grant 10", false, "", ""},
		{"etcdctl lease keep-alive 694d74da6ca8020b", false, "", ""},
		{"etcdctl lease list", false, "", ""},
		{"etcdctl lease revoke 694d74da6ca8020b", false, "", "etcdserver: requested lease not found"},
		{"etcdctl lease timetolive 694d74da6ca8020b", false, "", ""},
		// member
		{"etcdctl member list", false, "", ""},
		{"etcdctl member add tester", false, "", "member peer urls not provided"},
		{"etcdctl member promote 8e9e05c52164694d", false, "", "etcdserver: member not found"},
		{"etcdctl member remove 8e9e05c52164694d", false, "", "etcdserver: member not found"},
		{"etcdctl member update 8e9e05c52164694d", false, "", "member peer urls not provided"},
		// kvs
		{"etcdctl del --prefix /etcdctl-check-perf/", false, "", ""},
		{"etcdctl get /foo", false, "", ""},
		{"etcdctl put /etcdctl-check-perf/foo bar", false, "", ""},
		// snapshot
		{"env ETCDCTL_ENDPOINTS=https://127.0.0.1:2379 etcdctl snapshot save ./backup.db", false, "", ""},
		{"etcdctl snapshot status ./backup.db", false, "", ""},
		{"etcdctl snapshot restore ./backup.db", false, "", ""},
		{"etcdctl version", false, "", ""},
		// skip
		{"etcdctl auth enable", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl auth disable", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl lock", true, "distributed locks should not be done via etcdctl", ""},
		{"etcdctl make-mirror", true, "make-mirror takes one destination argument", ""},
		{"etcdctl migrate", true, "migrate from v2 is not support for OCP4", ""},
		{"etcdctl role add", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl role delete", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl role get", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl role grant-permission", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl role list", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl role revoke-permission", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl user add", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl user delete", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl user get", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl user grant-role", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl user list", true, "k8s does not use internal etcd RBAC", ""},
		{"etcdctl user revoke-role", true, "k8s does not use internal etcd RBAC", ""},
		// broke
		{"etcdctl move-leader 8e9e05c52164694d", true, "https://bugzilla.redhat.com/show_bug.cgi?id=1918413", ""},
		{"etcdctl check datascale", true, "TODO currently fails in test but works fine in container directly", ""},
		{"etcdctl txn", true, "TODO figure out multiline command to work in test", ""},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s", tc.command), func(t *testing.T) {
			if tc.skip {
				t.Skip(tc.skipReason)
			}
			actual, err := exec.Command("oc", getOcExecArgs(tc.command, podName)...).CombinedOutput()
			if len(tc.expectedError) > 0 {
				require.Contains(t, string(actual), tc.expectedError, "expected error not reported: %q", tc.expectedError)
			} else {
				require.NoError(t, err)
			}
		})
	}

	t.Cleanup(func() {
		if actual, err := exec.Command("oc", getOcExecArgs("rm -rf ./default.etcd", podName)...).CombinedOutput(); err != nil {
			require.NoError(t, fmt.Errorf("cleanup data-dir failed: %s", actual))
		}
		if actual, err := exec.Command("oc", getOcExecArgs("rm -f ./backup.db", podName)...).CombinedOutput(); err != nil {
			require.NoError(t, fmt.Errorf("cleanup db file failed: %s", actual))
		}
	})
}

func getOcExecArgs(command, podName string) []string {
	return strings.Split(fmt.Sprintf("rsh -c etcdctl -n openshift-etcd %s %s", podName, command), " ")
}
