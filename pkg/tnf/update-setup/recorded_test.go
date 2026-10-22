package updatesetup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

func runnerForTest(t *testing.T, progressing bool) (*recordedRunner, *[]string) {
	t.Helper()
	nodes := []*corev1.Node{}
	for i := 0; i < 2; i++ {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("master-%d", i), UID: types.UID(fmt.Sprintf("uid-%d", i)), Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: fmt.Sprintf("10.0.0.%d", i+1)}}}})
	}
	desired, err := generation.Nodes(nodes)
	require.NoError(t, err)
	status := configv1.ConditionFalse
	if progressing {
		status = configv1.ConditionTrue
	}
	client := fake.NewClientset(nodes[0], nodes[1])
	cfg := configfake.NewSimpleClientset(&configv1.ClusterVersion{ObjectMeta: metav1.ObjectMeta{Name: "version"}, Status: configv1.ClusterVersionStatus{Conditions: []configv1.ClusterOperatorStatusCondition{{Type: configv1.OperatorProgressing, Status: status}}}})
	commands := []string{}
	r := &recordedRunner{kube: client, config: cfg.ConfigV1(), snapshot: generation.Snapshot{Version: 1, Generation: 1, Nodes: desired}, current: "master-0", fencing: func(context.Context, kubernetes.Interface, []string) error { return nil }, alerts: func(context.Context) error { return nil }}
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		commands = append(commands, command)
		switch {
		case strings.Contains(command, "cluster config show"):
			cfg := pacemaker.ClusterConfig{Nodes: []pacemaker.ClusterConfigNode{{Name: "master-0", Addrs: []pacemaker.ClusterConfigNodeAddr{{Addr: "10.0.0.1"}}}, {Name: "master-1", Addrs: []pacemaker.ClusterConfigNodeAddr{{Addr: "10.0.0.2"}}}}}
			data, err := json.Marshal(cfg)
			return string(data), "", err
		case strings.Contains(command, "kubernetes_node_uid --query"):
			if strings.Contains(command, "master-0") {
				return "uid-0", "", nil
			}
			return "uid-1", "", nil
		case strings.Contains(command, "pcs cluster cib"):
			return `<cib><configuration><resources><clone><primitive id="etcd"><instance_attributes><nvpair name="node_ip_map" value="master-0:10.0.0.1;master-1:10.0.0.2"/></instance_attributes></primitive></clone></resources></configuration></cib>`, "", nil
		case strings.Contains(command, "pcs status xml"):
			return `<pacemaker-result><nodes><node name="master-0" online="true"/><node name="master-1" online="true"/></nodes></pacemaker-result>`, "", nil
		case strings.Contains(command, "member list -w json"):
			return `{"members":[]}`, "", nil
		default:
			return "", "", nil
		}
	}
	return r, &commands
}

func TestRegisteredOfflineNodeIsNotRemoved(t *testing.T) {
	r, commands := runnerForTest(t, false)
	execute := r.execute
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		if strings.Contains(command, "pcs status xml") {
			return `<pacemaker-result><nodes><node name="master-0" online="true"/><node name="master-1" online="false"/></nodes></pacemaker-result>`, "", nil
		}
		return execute(ctx, command)
	}
	require.ErrorContains(t, r.run(context.Background()), "master-1 is not online")
	for _, command := range *commands {
		require.NotContains(t, command, "cluster node remove")
		require.NotContains(t, command, "cluster node add")
		require.NotContains(t, command, "force_new_cluster")
	}
}

func TestReplacementIsIdempotentAcrossRetries(t *testing.T) {
	r, commands := runnerForTest(t, false)
	r.snapshot.Previous = append([]generation.Node(nil), r.snapshot.Nodes...)
	r.snapshot.Previous[1].UID = "old-uid"
	execute := r.execute
	repaired := false
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		if strings.Contains(command, "master-1 --name kubernetes_node_uid --query") && !repaired {
			return "old-uid", "", nil
		}
		if strings.Contains(command, "master-1 --name kubernetes_node_uid --update uid-1") {
			repaired = true
		}
		return execute(ctx, command)
	}
	require.NoError(t, r.run(context.Background()))
	require.True(t, repaired)
	require.NoError(t, r.run(context.Background()))
	removes := 0
	for _, cmd := range *commands {
		if strings.Contains(cmd, "cluster node remove master-1") {
			removes++
		}
		if strings.Contains(cmd, "force_new_cluster") {
			require.NotContains(t, cmd, "--update master-0")
		}
	}
	require.Equal(t, 1, removes, "retry must not remove the replacement again")
}

func TestLiveStateChangesStopFurtherMutation(t *testing.T) {
	r, commands := runnerForTest(t, false)
	r.snapshot.Previous = append([]generation.Node(nil), r.snapshot.Nodes...)
	r.snapshot.Previous[1].UID = "old-uid"
	execute := r.execute
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		if strings.Contains(command, "master-1 --name kubernetes_node_uid --query") {
			return "old-uid", "", nil
		}
		if strings.Contains(command, "cluster node remove master-1") {
			node, err := r.kube.CoreV1().Nodes().Get(ctx, "master-1", metav1.GetOptions{})
			require.NoError(t, err)
			node.UID = "another-replacement"
			_, err = r.kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
		return execute(ctx, command)
	}
	require.ErrorContains(t, r.run(context.Background()), "no longer matches")
	for _, cmd := range *commands {
		require.NotContains(t, cmd, "cluster node add")
	}
}

func TestUpgradeStopsRoutineMutation(t *testing.T) {
	r, commands := runnerForTest(t, true)
	r.snapshot.Repair = true // A stale collector observation must not authorize a routine reboot mutation.
	require.ErrorContains(t, r.run(context.Background()), "deferring")
	for _, command := range *commands {
		require.True(t, strings.Contains(command, "cluster config show") || strings.Contains(command, "tnf_reconciliation_generation --query"))
	}
}

func TestStaleSnapshotCannotMutate(t *testing.T) {
	r, commands := runnerForTest(t, false)
	node, err := r.kube.CoreV1().Nodes().Get(context.Background(), "master-1", metav1.GetOptions{})
	require.NoError(t, err)
	node.UID = "replacement"
	_, err = r.kube.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, r.run(context.Background()), "no longer matches")
	for _, command := range *commands {
		require.Contains(t, command, "tnf_reconciliation_generation --query")
	}
}

func TestGenerationNeverFallsBackToLatest(t *testing.T) {
	r, _ := runnerForTest(t, false)
	t.Setenv(generation.ConfigMapEnv, "")
	t.Setenv(generation.ConfigMapUIDEnv, "")
	require.ErrorContains(t, runRecordedGeneration(context.Background(), r.kube, r.config, "master-0"), "explicit recorded generation")
	cm, err := r.snapshot.ConfigMap()
	require.NoError(t, err)
	cm.UID = "new-uid"
	_, err = r.kube.CoreV1().ConfigMaps(generation.Namespace).Create(context.Background(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Setenv(generation.ConfigMapEnv, cm.Name)
	t.Setenv(generation.ConfigMapUIDEnv, "old-uid")
	require.ErrorContains(t, runRecordedGeneration(context.Background(), r.kube, r.config, "master-0"), "UID changed")
}
