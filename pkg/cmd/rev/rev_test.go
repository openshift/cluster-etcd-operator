package rev

import (
	"context"
	"encoding/json"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/integration"
	"os"
	"path"
	"testing"
	"time"
)

func TestHappyPathRevisionSaving(t *testing.T) {
	testServer, outputPath, clientPool := setupTestCluster(t)
	client, err := testServer.ClusterClient()
	require.NoError(t, err)

	trySaveRevision(context.Background(), client.Endpoints(), outputPath, clientPool, 5*time.Second)
	initialRev := readRevStruct(t, outputPath)
	require.Equal(t, 3, len(initialRev.RaftIndexByEndpoint))
	ensureRevStructConsistency(t, initialRev)

	for i := 0; i < 100; i++ {
		_, err := client.Put(context.Background(), "a", "b")
		require.NoError(t, err)

		trySaveRevision(context.Background(), client.Endpoints(), outputPath, clientPool, 5*time.Second)
		revStruct := readRevStruct(t, outputPath)
		ensureRevStructConsistency(t, revStruct)
		// due to CPU constraints we might lag behind a bit in prow, this accounts for a little slack after putting a revision
		require.InDelta(t, initialRev.MaxRaftIndex+uint64(i+1), revStruct.MaxRaftIndex, 2)
	}
}

func TestNodeDownRevisionSaving(t *testing.T) {
	testServer, outputPath, clientPool := setupTestCluster(t)
	client, err := testServer.ClusterClient()
	require.NoError(t, err)

	list, err := client.MemberList(context.Background())
	require.NoError(t, err)

	clusterId := list.Header.ClusterId

	trySaveRevision(context.Background(), client.Endpoints(), outputPath, clientPool, 5*time.Second)
	initialRev := readRevStruct(t, outputPath)
	require.Equal(t, 3, len(initialRev.RaftIndexByEndpoint))
	require.Equal(t, clusterId, initialRev.ClusterId)
	ensureRevStructConsistency(t, initialRev)

	// ensure we move the leader somewhere else to avoid flakes, that only succeeds when the member is the leader
	_, _ = testServer.Client(1).MoveLeader(context.Background(), list.Members[0].ID)
	testServer.Members[1].Terminate(t)

	for i := 0; i < 5; i++ {
		_, err := client.Put(context.Background(), "a", "b")
		require.NoError(t, err)

		// have a more aggressive timeout here, since member-1 will never respond anyway
		trySaveRevision(context.Background(), client.Endpoints(), outputPath, clientPool, 100*time.Millisecond)
		revStruct := readRevStruct(t, outputPath)
		ensureRevStructConsistency(t, revStruct)
		require.InDelta(t, initialRev.MaxRaftIndex+uint64(i+1), revStruct.MaxRaftIndex, 2)
		require.Equal(t, clusterId, revStruct.ClusterId)
	}
}

func TestNoQuorumRevisionSaving(t *testing.T) {
	testServer, outputPath, clientPool := setupTestCluster(t)
	client, err := testServer.ClusterClient()
	require.NoError(t, err)

	trySaveRevision(context.Background(), client.Endpoints(), outputPath, clientPool, 5*time.Second)
	initialRev := readRevStruct(t, outputPath)
	require.Equal(t, 3, len(initialRev.RaftIndexByEndpoint))
	ensureRevStructConsistency(t, initialRev)

	testServer.Members[0].Terminate(t)
	testServer.Members[1].Terminate(t)

	for i := 0; i < 5; i++ {
		func() {
			// to make it fail quick
			timeout, cancelFunc := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancelFunc()

			_, err := client.Put(timeout, "a", "b")
			require.Error(t, err)
		}()

		// have a more aggressive timeout here, since member-1 will never respond anyway
		trySaveRevision(context.Background(), client.Endpoints(), outputPath, clientPool, 100*time.Millisecond)
		revStruct := readRevStruct(t, outputPath)
		ensureRevStructConsistency(t, revStruct)
		// no change in max should be expected
		require.Equal(t, initialRev.MaxRaftIndex, revStruct.MaxRaftIndex)
		// we can only reach a single endpoint
		require.Equal(t, 1, len(revStruct.RaftIndexByEndpoint))
	}
}

func readRevStruct(t *testing.T, outputPath string) outputStruct {
	o, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	osx := outputStruct{}
	require.NoError(t, json.Unmarshal(o, &osx))
	return osx
}

func ensureRevStructConsistency(t *testing.T, o outputStruct) {
	maxRev := uint64(0)
	for _, u := range o.RaftIndexByEndpoint {
		maxRev = max(maxRev, u)
	}
	require.Equal(t, maxRev, o.MaxRaftIndex)
}

func setupTestCluster(t *testing.T) (*integration.ClusterV3, string, *etcdcli.EtcdClientPool) {
	integration.BeforeTestExternal(t)
	testServer := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 3})

	tmpDir, err := os.MkdirTemp("", "TestRevisionSaving")
	require.NoError(t, err)
	outputPath := path.Join(tmpDir, "rev.json")

	t.Cleanup(func() {
		testServer.Terminate(t)
		require.NoError(t, os.RemoveAll(tmpDir))
	})

	clientPool := etcdcli.NewEtcdClientPool(
		// newFunc
		func() (*clientv3.Client, error) { return testServer.ClusterClient() },
		// endpointsFunc
		func() ([]string, error) {
			client, err := testServer.ClusterClient()
			if err != nil {
				return nil, err
			}
			return client.Endpoints(), nil
		},
		// healthFunc
		func(client *clientv3.Client) error { return nil },
		// closeFunc
		func(client *clientv3.Client) error { return client.Close() },
	)

	return testServer, outputPath, clientPool
}
