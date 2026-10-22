package generation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func testNode(name, uid, ip string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
}

func TestRecordedIdentityRoundTrip(t *testing.T) {
	a, b := testNode("master-0", "a", "2001:db8::1"), testNode("master-1", "b", "2001:0db8::2")
	nodes, err := Nodes([]*corev1.Node{b, a})
	require.NoError(t, err)
	s := Snapshot{Version: 1, Generation: 1, Nodes: nodes}
	cm, err := s.ConfigMap()
	require.NoError(t, err)
	decoded, err := Decode(cm)
	require.NoError(t, err)
	require.Equal(t, s, decoded)
	client := fake.NewClientset(a, b, cm)
	require.NoError(t, decoded.MatchesCurrent(context.Background(), client))
	b.UID = "replacement"
	_, err = client.CoreV1().Nodes().Update(context.Background(), b, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, decoded.MatchesCurrent(context.Background(), client), "no longer matches")
	cm.Labels[Label] = "2"
	_, err = Decode(cm)
	require.Error(t, err)
}

func TestRejectIncompleteDesiredState(t *testing.T) {
	for _, nodes := range [][]*corev1.Node{nil, {testNode("master-0", "", "10.0.0.1")}, {testNode("master-0", "a", "invalid")}, {testNode("master-0", "a", "10.0.0.1"), testNode("master-1", "b", "10.0.0.1")}} {
		_, err := Nodes(nodes)
		require.Error(t, err)
	}
}

func TestUpgradePolicy(t *testing.T) {
	nodes, err := Nodes([]*corev1.Node{testNode("master-0", "a", "10.0.0.1"), testNode("master-1", "b", "10.0.0.2")})
	require.NoError(t, err)
	for _, tc := range []struct {
		name                             string
		configured                       map[string]string
		progressing, replaced, wantDefer bool
	}{
		{"ordinary reboot", map[string]string{"master-0": "10.0.0.1", "master-1": "10.0.0.2"}, true, false, true},
		{"re-add unblocks MCO", map[string]string{"master-0": "10.0.0.1"}, true, false, false},
		{"same name replacement", map[string]string{"master-0": "10.0.0.1", "master-1": "10.0.0.2"}, true, true, false},
		{"settled", map[string]string{"master-0": "10.0.0.1", "master-1": "10.0.0.2"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{Version: 1, Generation: 1, Nodes: nodes}
			if tc.replaced {
				s.Previous = append([]Node(nil), nodes...)
				s.Previous[1].UID = "old"
			}
			require.Equal(t, tc.wantDefer, s.DeferDuringUpgrade(tc.progressing, tc.configured))
		})
	}
}
