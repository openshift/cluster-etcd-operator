package etcdcli

import (
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
				u.EndpointsConfigMap(),
				u.FakeNode("0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.0")),
				u.FakeNode("1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1")),
				u.FakeNode("2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2")),
				u.FakeNetwork(false),
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
				u.EndpointsConfigMap(u.WithBootstrapIP("10.0.0.0")),
				u.FakeNode("1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.42")),
				u.FakeNetwork(false),
			},
			[]string{
				"https://10.0.0.0:2379",
				"https://10.0.0.42:2379",
			},
			nil,
		},
		{
			"happy path with ipv6 nodes",
			[]runtime.Object{
				u.FakeNetwork(true),
				u.EndpointsConfigMap(),
				u.FakeNode("0", u.WithMasterLabel(), u.WithNodeInternalIP("fda6:cfed:b298:2514:0000:0000:0000:0000")),
				u.FakeNode("2", u.WithMasterLabel(), u.WithNodeInternalIP("fda6:cfed:b298:2514:0000:0000:0000:0001")),
			},
			[]string{
				"https://[fda6:cfed:b298:2514:0000:0000:0000:0000]:2379",
				"https://[fda6:cfed:b298:2514:0000:0000:0000:0001]:2379",
			},
			nil,
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

			endpoints, err := endpoints(
				corev1listers.NewNodeLister(indexer),
				corev1listers.NewConfigMapLister(indexer),
				configv1listers.NewNetworkLister(indexer),
				syncTrue, syncTrue, syncTrue,
			)

			require.Equal(t, scenario.expectedError, err)
			require.Equal(t, scenario.expectedEndpoints, endpoints)
		})
	}
}

func syncTrue() bool {
	return true
}
