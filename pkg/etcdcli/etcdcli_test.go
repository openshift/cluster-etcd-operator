package etcdcli

import (
	"fmt"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"k8s.io/apimachinery/pkg/labels"
	"testing"

	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

func TestEndpointFunc(t *testing.T) {
	scenarios := []struct {
		name              string
		objects           []runtime.Object
		expectedEndpoints []string
		expectedError     error
	}{
		{
			"happy path",
			[]runtime.Object{
				u.EndpointsConfigMap(
					u.WithEndpoint(0, "10.0.0.0:2379"),
					u.WithEndpoint(1, "10.0.0.1:2379"),
					u.WithEndpoint(2, "10.0.0.2:2379"),
				),
			},
			[]string{
				"https://10.0.0.0:2379",
				"https://10.0.0.1:2379",
				"https://10.0.0.2:2379",
			},
			nil,
		},
		{
			"happy path with bootstrap",
			[]runtime.Object{
				u.EndpointsConfigMap(
					u.WithEndpoint(0, "10.0.0.0:2379"),
					u.WithBootstrapIP("10.0.0.42"),
				),
			},
			[]string{
				"https://10.0.0.0:2379",
				"https://10.0.0.42:2379",
			},
			nil,
		},
		{
			"configmap not available with nodes",
			[]runtime.Object{
				u.FakeNetwork(false),
				u.FakeNode("0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.0")),
				u.FakeNode("2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
			},
			[]string{
				"https://10.0.0.0:2379",
				"https://10.0.0.2:2379",
			},
			nil,
		},
		{
			"configmap not available with ipv6 nodes",
			[]runtime.Object{
				u.FakeNetwork(true),
				u.FakeNode("0", u.WithMasterLabel(), u.WithNodeInternalIP("fda6:cfed:b298:2514:0000:0000:0000:0000")),
				u.FakeNode("2", u.WithMasterLabel(), u.WithNodeInternalIP("fda6:cfed:b298:2514:0000:0000:0000:0001")),
			},
			[]string{
				"https://[fda6:cfed:b298:2514:0000:0000:0000:0000]:2379",
				"https://[fda6:cfed:b298:2514:0000:0000:0000:0001]:2379",
			},
			nil,
		},
		{
			"configmap not available, no nodes",
			[]runtime.Object{
				u.FakeNetwork(false),
			},
			nil,
			fmt.Errorf("endpoints func found no etcd endpoints"),
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					require.NoError(t, err)
				}
			}

			nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
			require.NoError(t, err)

			endpoints, err := endpoints(
				corev1listers.NewNodeLister(indexer),
				nodeSelector,
				corev1listers.NewConfigMapLister(indexer),
				configv1listers.NewNetworkLister(indexer),
				syncTrue, syncTrue, syncTrue,
			)

			require.Equal(t, scenario.expectedError, err)
			require.Equal(t, scenario.expectedEndpoints, endpoints)
		})
	}
}

func TestFilterVotingMembers(t *testing.T) {
	scenarios := []struct {
		name     string
		input    []*etcdserverpb.Member
		expected int
	}{
		{
			"all voting members",
			[]*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
				u.FakeEtcdMemberWithoutServer(3),
			},
			3,
		},
		{
			"all learner members",
			[]*etcdserverpb.Member{
				u.AsLearner(u.FakeEtcdMemberWithoutServer(1)),
				u.AsLearner(u.FakeEtcdMemberWithoutServer(2)),
				u.AsLearner(u.FakeEtcdMemberWithoutServer(3)),
			},
			0,
		},
		{
			"one learner two voting members",
			[]*etcdserverpb.Member{
				u.AsLearner(u.FakeEtcdMemberWithoutServer(1)),
				u.FakeEtcdMemberWithoutServer(2),
				u.FakeEtcdMemberWithoutServer(3),
			},
			2,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if len(filterVotingMembers(scenario.input)) != scenario.expected {
				t.Errorf("expected to have %v voting member, but got %v instead", scenario.expected, len(filterVotingMembers(scenario.input)))
			}
		})
	}
}

func syncTrue() bool {
	return true
}
