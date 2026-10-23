package updatesetup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
	client := fake.NewClientset(nodes[0], nodes[1])
	setMCOUpdating(t, client, progressing)
	commands := []string{}
	r := &recordedRunner{kube: client, snapshot: generation.Snapshot{Version: 1, Generation: 1, Nodes: desired}, current: "master-0", fencing: func(context.Context, kubernetes.Interface, []string) error { return nil }, alerts: func(context.Context) error { return nil }, waitForRevision: func(context.Context) error { return nil }}
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		commands = append(commands, command)
		switch {
		case strings.Contains(command, "cluster config show"):
			cfg := pacemaker.ClusterConfig{Nodes: []pacemaker.ClusterConfigNode{{Name: "master-0", Addrs: []pacemaker.ClusterConfigNodeAddr{{Addr: "10.0.0.1"}}}, {Name: "master-1", Addrs: []pacemaker.ClusterConfigNodeAddr{{Addr: "10.0.0.2"}}}}}
			data, err := json.Marshal(cfg)
			return string(data), "", err
		case strings.Contains(command, "pcs cluster cib"):
			return `<cib><configuration><crm_config><cluster_property_set id="bootstrap"><nvpair name="tnf_node_uid_master-0" value="uid-0"/><nvpair name="tnf_node_uid_master-1" value="uid-1"/></cluster_property_set></crm_config><resources><clone><primitive id="etcd"><instance_attributes><nvpair name="node_ip_map" value="master-0:10.0.0.1;master-1:10.0.0.2"/></instance_attributes></primitive></clone></resources></configuration></cib>`, "", nil
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
	r, commands := runnerForTest(t, true)
	r.snapshot.Repair = true // Historical repair intent does not make a reboot a membership change.
	execute := r.execute
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		if strings.Contains(command, "pcs status xml") {
			return `<pacemaker-result><nodes><node name="master-0" online="true"/><node name="master-1" online="false"/></nodes></pacemaker-result>`, "", nil
		}
		return execute(ctx, command)
	}
	require.ErrorContains(t, r.run(context.Background()), "deferring")
	setMCOUpdating(t, r.kube, false)
	require.ErrorContains(t, r.run(context.Background()), "master-1 is not online")
	for _, command := range *commands {
		require.NotContains(t, command, "cluster node remove")
		require.NotContains(t, command, "cluster node add")
		require.NotContains(t, command, "force_new_cluster")
	}
	r.snapshot.Discovery = true
	*commands = nil
	require.NoError(t, r.run(context.Background()))
	for _, command := range *commands {
		require.NotContains(t, command, "cluster start")
		require.NotContains(t, command, "cluster enable")
		require.NotContains(t, command, "crm_attribute")
		require.NotContains(t, command, "resource update")
	}
}

func TestLiveStateChangesStopFurtherMutation(t *testing.T) {
	r, commands := runnerForTest(t, false)
	r.snapshot.Previous = append([]generation.Node(nil), r.snapshot.Nodes...)
	r.snapshot.Previous[1].UID = "old-uid"
	execute := r.execute
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		if strings.Contains(command, "pcs cluster cib") {
			stdout, stderr, err := execute(ctx, command)
			return strings.ReplaceAll(stdout, `name="tnf_node_uid_master-1" value="uid-1"`, `name="tnf_node_uid_master-1" value="old-uid"`), stderr, err
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

func TestGenerationNeverFallsBackToLatest(t *testing.T) {
	r, _ := runnerForTest(t, false)
	t.Setenv(generation.ConfigMapEnv, "")
	t.Setenv(generation.ConfigMapUIDEnv, "")
	require.ErrorContains(t, runRecordedGeneration(context.Background(), r.kube, "master-0", r.waitForRevision), "explicit recorded generation")
	cm, err := r.snapshot.ConfigMap()
	require.NoError(t, err)
	cm.UID = "new-uid"
	_, err = r.kube.CoreV1().ConfigMaps(generation.Namespace).Create(context.Background(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Setenv(generation.ConfigMapEnv, cm.Name)
	t.Setenv(generation.ConfigMapUIDEnv, "old-uid")
	require.ErrorContains(t, runRecordedGeneration(context.Background(), r.kube, "master-0", r.waitForRevision), "UID changed")
}

func TestPartialReplacementResumesWithoutRemovingNodeAgain(t *testing.T) {
	ctx := context.Background()
	r, commands := runnerForTest(t, false)
	r.snapshot.Discovery = true
	waits, revisionReady := 0, false
	r.waitForRevision = func(context.Context) error {
		waits++
		if !revisionReady {
			return fmt.Errorf("revision is not stable")
		}
		return nil
	}
	// Model a replacement discovered from the CIB before a baseline
	// generation has recorded previous identities.
	properties := map[string]string{"tnf_node_uid_master-1": "old-uid"}
	failed, mapUpdated := false, false
	execute := r.execute
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		stdout, stderr, err := execute(ctx, command)
		if strings.Contains(command, "pcs cluster stop master-1 --force --request-timeout=300") {
			return "", "node unreachable", fmt.Errorf("injected stop failure")
		}
		if strings.Contains(command, "pcs cluster cib") {
			stdout = strings.ReplaceAll(stdout, `name="tnf_node_uid_master-1" value="uid-1"`, `name="tnf_node_uid_master-1" value="`+properties["tnf_node_uid_master-1"]+`"`)
			for _, key := range []string{"tnf_reconciliation_generation", "tnf_reconciliation_force_new_cluster"} {
				if value := properties[key]; value != "" {
					stdout = strings.Replace(stdout, "</cluster_property_set>", `<nvpair name="`+key+`" value="`+value+`"/></cluster_property_set>`, 1)
				}
			}
			if !mapUpdated {
				stdout = strings.ReplaceAll(stdout, "master-1:10.0.0.2", "master-1:10.0.0.99")
			}
		}
		if strings.HasPrefix(command, "crm_attribute --type crm_config") {
			args := strings.Fields(command)
			properties[args[4]] = args[6]
		}
		if strings.Contains(command, "resource update etcd") {
			if !failed {
				failed = true
				return "", "", fmt.Errorf("injected ip-map failure")
			}
			mapUpdated = true
		}
		return stdout, stderr, err
	}
	require.ErrorContains(t, r.run(ctx), "revision is not stable")
	for _, command := range *commands {
		require.NotContains(t, command, "cluster stop")
		require.NotContains(t, command, "cluster node")
		require.NotContains(t, command, "crm_attribute")
	}
	revisionReady = true
	require.ErrorContains(t, r.run(ctx), "injected ip-map failure")
	require.Equal(t, "uid-1", properties["tnf_node_uid_master-1"])
	claim := properties["tnf_reconciliation_force_new_cluster"]
	require.NotEmpty(t, claim)
	setMCOUpdating(t, r.kube, true)
	// A new process learns its progress only from durable CIB properties.
	retry := *r
	retry.continuingRepair, retry.repairClaim = false, ""
	require.NoError(t, retry.run(ctx))
	require.Equal(t, 2, waits, "a retry after completed membership changes must not wait again")
	stops, removes, adds, claims := 0, 0, 0, 0
	firstStop, firstRemove := -1, -1
	firstClaim, firstUpdate := -1, -1
	for i, command := range *commands {
		if strings.Contains(command, "pcs cluster stop master-1 --force --request-timeout=300") {
			stops++
			firstStop = i
		}
		if strings.Contains(command, "cluster node remove master-1") {
			removes++
			firstRemove = i
		}
		if strings.Contains(command, "cluster node add master-1") {
			adds++
		}
		if strings.Contains(command, "resource update etcd") && firstUpdate < 0 {
			firstUpdate = i
		}
		if strings.Contains(command, "--name force_new_cluster --update") {
			claims++
			if firstClaim < 0 {
				firstClaim = i
			}
			require.True(t, strings.HasSuffix(command, "--update "+claim))
		}
	}
	require.Equal(t, 1, stops)
	require.Equal(t, 1, removes)
	require.Equal(t, 1, adds)
	require.Greater(t, firstRemove, firstStop, "attempt the best-effort stop before removing the node")
	require.GreaterOrEqual(t, claims, 1)
	require.True(t, mapUpdated)
	require.Greater(t, firstUpdate, firstClaim, "record the recovery claim before changing etcd resources")
}

func TestMissingMemberDoesNotWaitForStableRevision(t *testing.T) {
	r, _ := runnerForTest(t, false)
	r.waitForRevision = func(context.Context) error {
		t.Fatal("adding back a missing member must not wait for revision stability")
		return nil
	}
	missing := true
	execute := r.execute
	r.execute = func(ctx context.Context, command string) (string, string, error) {
		stdout, stderr, err := execute(ctx, command)
		if missing && strings.Contains(command, "cluster config show") {
			return `{"nodes":[{"name":"master-0","addrs":[{"addr":"10.0.0.1"}]}]}`, "", nil
		}
		if strings.Contains(command, "cluster node add master-1") {
			missing = false
		}
		return stdout, stderr, err
	}
	require.NoError(t, r.run(context.Background()))
	require.False(t, missing)
}

func setMCOUpdating(t *testing.T, kube kubernetes.Interface, updating bool) {
	t.Helper()
	ctx := context.Background()
	node, err := kube.CoreV1().Nodes().Get(ctx, "master-0", metav1.GetOptions{})
	require.NoError(t, err)
	node.Annotations = map[string]string{"machineconfiguration.openshift.io/state": "Done"}
	if updating {
		node.Annotations["machineconfiguration.openshift.io/state"] = "Working"
	}
	_, err = kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
}
