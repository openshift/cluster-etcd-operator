package defragcontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/integration"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

// waitForMembersWithClientURLs waits until all etcd members have their ClientURLs populated.
// This is necessary because members publish their ClientURLs asynchronously after joining the cluster.
func waitForMembersWithClientURLs(t *testing.T, testServer *integration.Cluster) []*etcdserverpb.Member {
	var etcdMembers []*etcdserverpb.Member
	require.Eventually(t, func() bool {
		memberListResp, err := testServer.Client(0).MemberList(context.TODO())
		if err != nil {
			return false
		}
		for _, member := range memberListResp.Members {
			if len(member.ClientURLs) == 0 {
				return false
			}
		}
		etcdMembers = memberListResp.Members
		return true
	}, 10*time.Second, 100*time.Millisecond, "timed out waiting for all members to have ClientURLs")
	return etcdMembers
}

func TestNewDefragController(t *testing.T) {
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	scenarios := []struct {
		name                string
		staticPodStatus     *operatorv1.StaticPodOperatorStatus
		objects             []runtime.Object
		clusterSize         int
		syncLoops           int
		memberHealth        *etcdcli.FakeMemberHealth
		dbInUse             int64
		dbSize              int64
		defragSuccessEvents int
		wantErr             bool
		wantErrMsg          string
		wantCondition       operatorv1.ConditionStatus
	}{
		{
			name:                "defrag success",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,     // 1GB
			dbInUse:             minDefragBytes / 2, // 500MB
			defragSuccessEvents: 3,
			clusterSize:         3,
			syncLoops:           4, // 2 non-leader defrags + 1 leader transfer + 1 former-leader defrag
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			wantCondition: operatorv1.ConditionFalse,
		},
		{
			name:                "defrag two node with fencing success",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,     // 1GB
			dbInUse:             minDefragBytes / 2, // 500MB
			defragSuccessEvents: 2,
			clusterSize:         2,
			syncLoops:           3, // 1 non-leader defrag + 1 leader transfer + 1 former-leader defrag
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 2},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.DualReplicaTopologyMode),
			},
			wantCondition: operatorv1.ConditionFalse,
		},
		{
			name:                "defrag two node with arbiter success",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,     // 1GB
			dbInUse:             minDefragBytes / 2, // 500MB
			defragSuccessEvents: 3,
			clusterSize:         3,
			syncLoops:           4, // 2 non-leader defrags + 1 leader transfer + 1 former-leader defrag
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableArbiterMode),
			},
			wantCondition: operatorv1.ConditionFalse,
		},
		{
			name:                "defrag controller disabled SNO",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,     // 1GB
			dbInUse:             minDefragBytes / 2, // 500MB
			defragSuccessEvents: 0,
			clusterSize:         1,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 1},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.SingleReplicaTopologyMode),
			},
			wantCondition: operatorv1.ConditionTrue,
		},
		{
			name:                "defrag controller disabled manual override",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,     // 1GB
			dbInUse:             minDefragBytes / 2, // 500MB
			defragSuccessEvents: 0,
			clusterSize:         3,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
				u.FakeConfigMap(operatorclient.OperatorNamespace, defragDisableConfigmapName, map[string]string{}),
			},
			wantCondition: operatorv1.ConditionTrue,
		},
		{
			name:                "no defrag required dbSize below minDefragBytes",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes / 2,
			dbInUse:             minDefragBytes / 4, // maxFragmentedPercentage
			defragSuccessEvents: 0,
			clusterSize:         3,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			wantCondition: operatorv1.ConditionFalse,
		},
		{
			name:                "no defrag required dbSize above minDefragBytes and below maxFragmentedPercentage",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes + 1,
			dbInUse:             minDefragBytes,
			defragSuccessEvents: 0,
			clusterSize:         3,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			wantCondition: operatorv1.ConditionFalse,
		},
		{
			name:                "defrag failed cluster is unhealthy: 2 of 3 members are available",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,
			dbInUse:             minDefragBytes / 2,
			defragSuccessEvents: 0,
			clusterSize:         3,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 2, Unhealthy: 1},
			wantErr:             true,
			wantErrMsg:          "cluster is unhealthy: 2 of 3 members are available",
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			wantCondition: operatorv1.ConditionFalse,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			integration.BeforeTestExternal(t)
			// use integration etcd to create etcd members and status
			testServer := integration.NewCluster(t, &integration.ClusterConfig{Size: scenario.clusterSize})
			defer testServer.Terminate(t)

			// Wait for all members to have ClientURLs populated (they publish asynchronously)
			etcdMembers := waitForMembersWithClientURLs(t, testServer)

			// populate Status
			var status []*clientv3.StatusResponse
			for _, member := range testServer.Members {
				statusResp, err := testServer.Client(0).Status(context.TODO(), member.GRPCURL)
				require.NoError(t, err)
				statusResp.DbSizeInUse = scenario.dbInUse
				statusResp.DbSize = scenario.dbSize
				status = append(status, statusResp)
			}

			fakeEtcdClient, _ := etcdcli.NewFakeEtcdClient(
				etcdMembers,
				etcdcli.WithFakeClusterHealth(scenario.memberHealth),
				etcdcli.WithFakeStatus(status),
			)
			eventRecorder := events.NewInMemoryRecorder(t.Name(), clock.RealClock{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			controller := &DefragController{
				operatorClient:       fakeOperatorClient,
				memberLister:         fakeEtcdClient,
				defragClient:         fakeEtcdClient,
				statusClient:         fakeEtcdClient,
				leaderMover:          fakeEtcdClient,
				infrastructureLister: configv1listers.NewInfrastructureLister(indexer),
				configmapLister:      corev1listers.NewConfigMapLister(indexer),
			}

			syncLoops := scenario.syncLoops
			if syncLoops == 0 {
				syncLoops = 1
			}
			var err error
			for i := 0; i < syncLoops; i++ {
				err = controller.sync(context.TODO(), factory.NewSyncContext("defrag-controller", eventRecorder))
				if err != nil && !scenario.wantErr {
					t.Fatalf("unexpected error on sync %d: %v", i, err)
				}
			}
			if err == nil && scenario.wantErr {
				t.Fatal("expected error got nil")
			}
			if err != nil && scenario.wantErr {
				if !strings.Contains(err.Error(), scenario.wantErrMsg) {
					t.Fatalf("unexpected error prefix want: %q got: %q", scenario.wantErrMsg, err.Error())
				}
			}
			var defragSuccessEvents int
			for _, event := range eventRecorder.Events() {
				if strings.Contains(event.Message, "etcd member has been defragmented") {
					defragSuccessEvents++
				}
			}
			if defragSuccessEvents != scenario.defragSuccessEvents {
				t.Fatalf("defragSuccessEvents invalid want %d got %d", scenario.defragSuccessEvents, defragSuccessEvents)
			}

			_, currentState, _, _ := fakeOperatorClient.GetOperatorState()
			controllerDisabledCondition := v1helpers.FindOperatorCondition(currentState.Conditions, defragDisabledCondition)
			if scenario.wantCondition != controllerDisabledCondition.Status {
				t.Fatalf("operator condition invalid want %s got %s", scenario.wantCondition, controllerDisabledCondition.Status)
			}
		})
	}
}

// similar to the above, but across multiple loops of sync
func TestNewDefragControllerMultiSyncs(t *testing.T) {
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	scenarios := []struct {
		name                  string
		staticPodStatus       *operatorv1.StaticPodOperatorStatus
		objects               []runtime.Object
		fakeClientOpts        []etcdcli.FakeClientOption
		clusterSize           int
		syncLoops             int
		errSyncLoops          int
		memberHealth          *etcdcli.FakeMemberHealth
		dbInUse               int64
		dbSize                int64
		defragSuccessEvents   int
		wantDisabledCondition operatorv1.ConditionStatus
		wantDegradedCondition operatorv1.ConditionStatus
	}{
		{
			name:                "defrag degrades after several unsuccessful loops",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,
			dbInUse:             minDefragBytes / 2,
			defragSuccessEvents: 0,
			clusterSize:         3,
			syncLoops:           maxDefragFailuresBeforeDegrade,
			errSyncLoops:        0,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			fakeClientOpts: []etcdcli.FakeClientOption{
				etcdcli.WithFakeDefragErrors(generateErrors(maxDefragFailuresBeforeDegrade)),
			},
			wantDisabledCondition: operatorv1.ConditionFalse,
			wantDegradedCondition: operatorv1.ConditionTrue,
		},
		{
			name:                "defrag degrades and recovers after several unsuccessful loops",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,
			dbInUse:             minDefragBytes / 2,
			defragSuccessEvents: 3,
			clusterSize:         3,
			syncLoops:           maxDefragFailuresBeforeDegrade + 4, // +1 for leader transfer requeue
			errSyncLoops:        0,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			fakeClientOpts: []etcdcli.FakeClientOption{
				etcdcli.WithFakeDefragErrors(generateErrors(maxDefragFailuresBeforeDegrade)),
			},
			wantDisabledCondition: operatorv1.ConditionFalse,
			wantDegradedCondition: operatorv1.ConditionFalse,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			integration.BeforeTestExternal(t)
			// use integration etcd to create etcd members and status
			testServer := integration.NewCluster(t, &integration.ClusterConfig{Size: scenario.clusterSize})
			defer testServer.Terminate(t)

			// Wait for all members to have ClientURLs populated (they publish asynchronously)
			etcdMembers := waitForMembersWithClientURLs(t, testServer)

			// populate Status
			var status []*clientv3.StatusResponse
			for _, member := range testServer.Members {
				statusResp, err := testServer.Client(0).Status(context.TODO(), member.GRPCURL)
				require.NoError(t, err)
				statusResp.DbSizeInUse = scenario.dbInUse
				statusResp.DbSize = scenario.dbSize
				status = append(status, statusResp)
			}

			fakeOpts := []etcdcli.FakeClientOption{
				etcdcli.WithFakeClusterHealth(scenario.memberHealth),
				etcdcli.WithFakeStatus(status),
			}

			for _, o := range scenario.fakeClientOpts {
				fakeOpts = append(fakeOpts, o)
			}

			fakeEtcdClient, _ := etcdcli.NewFakeEtcdClient(etcdMembers, fakeOpts...)
			eventRecorder := events.NewInMemoryRecorder(t.Name(), clock.RealClock{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			controller := &DefragController{
				operatorClient:       fakeOperatorClient,
				memberLister:         fakeEtcdClient,
				statusClient:         fakeEtcdClient,
				defragClient:         fakeEtcdClient,
				leaderMover:          fakeEtcdClient,
				infrastructureLister: configv1listers.NewInfrastructureLister(indexer),
				configmapLister:      corev1listers.NewConfigMapLister(indexer),
			}

			numSyncErr := 0
			for i := 0; i < scenario.syncLoops; i++ {
				err := controller.sync(context.TODO(), factory.NewSyncContext("defrag-controller", eventRecorder))
				if err != nil {
					numSyncErr++
					fmt.Printf("error on sync: %v\n", err)
				}
			}

			assert.Equal(t, scenario.errSyncLoops, numSyncErr)

			var defragSuccessEvents int
			for _, event := range eventRecorder.Events() {
				if strings.Contains(event.Message, "etcd member has been defragmented") {
					defragSuccessEvents++
				}
			}
			assert.Equal(t, scenario.defragSuccessEvents, defragSuccessEvents)

			_, currentState, _, _ := fakeOperatorClient.GetOperatorState()
			controllerDisabledCondition := v1helpers.FindOperatorCondition(currentState.Conditions, defragDisabledCondition)
			assert.Equal(t, scenario.wantDisabledCondition, controllerDisabledCondition.Status)

			controllerDegradedCondition := v1helpers.FindOperatorCondition(currentState.Conditions, defragDegradedCondition)
			assert.Equal(t, scenario.wantDegradedCondition, controllerDegradedCondition.Status)
		})
	}
}

func TestDefragMovesLeadershipBeforeDefrag(t *testing.T) {
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	integration.BeforeTestExternal(t)
	testServer := integration.NewCluster(t, &integration.ClusterConfig{Size: 3})
	defer testServer.Terminate(t)

	etcdMembers := waitForMembersWithClientURLs(t, testServer)

	var status []*clientv3.StatusResponse
	var leaderID uint64
	for _, member := range testServer.Members {
		statusResp, err := testServer.Client(0).Status(context.TODO(), member.GRPCURL)
		require.NoError(t, err)
		if leaderID == 0 {
			leaderID = statusResp.Leader
		}
		// Only the leader is fragmented, so it's the sole defrag target
		// and must trigger a leader transfer.
		statusResp.DbSize = minDefragBytes / 2
		statusResp.DbSizeInUse = minDefragBytes / 2
		for _, m := range etcdMembers {
			if m.ID == leaderID && statusResp.Header.MemberId == leaderID {
				statusResp.DbSize = minDefragBytes
				statusResp.DbSizeInUse = minDefragBytes / 2
			}
		}
		status = append(status, statusResp)
	}

	fakeEtcdClient, _ := etcdcli.NewFakeEtcdClient(
		etcdMembers,
		etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3}),
		etcdcli.WithFakeStatus(status),
	)
	eventRecorder := events.NewInMemoryRecorder(t.Name(), clock.RealClock{})
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, indexer.Add(u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode)))

	controller := &DefragController{
		operatorClient:       fakeOperatorClient,
		memberLister:         fakeEtcdClient,
		defragClient:         fakeEtcdClient,
		statusClient:         fakeEtcdClient,
		leaderMover:          fakeEtcdClient,
		infrastructureLister: configv1listers.NewInfrastructureLister(indexer),
		configmapLister:      corev1listers.NewConfigMapLister(indexer),
		defragSettleTime:     1 * time.Millisecond,
	}

	syncCtx := factory.NewSyncContext("defrag-controller", eventRecorder)

	// First sync: leader is the most fragmented, so leadership is transferred and sync requeues.
	err := controller.sync(context.TODO(), syncCtx)
	require.NoError(t, err)

	var leaderTransferEvents int
	for _, event := range eventRecorder.Events() {
		if strings.Contains(event.Message, "Moved leadership away from member") {
			leaderTransferEvents++
		}
	}
	assert.Equal(t, 1, leaderTransferEvents, "expected exactly one leader transfer event")

	// Wait for the requeue item to appear on the queue (added by AddAfter after leader transfer).
	item, shutdown := syncCtx.Queue().Get()
	require.False(t, shutdown, "queue should not be shut down")
	syncCtx.Queue().Done(item)

	// Second sync driven by the queue: the former leader is now a follower and gets defragged.
	err = controller.sync(context.TODO(), syncCtx)
	require.NoError(t, err)

	var defragSuccessEvents int
	for _, event := range eventRecorder.Events() {
		if strings.Contains(event.Message, "etcd member has been defragmented") {
			defragSuccessEvents++
		}
	}
	assert.Equal(t, 1, defragSuccessEvents, "expected exactly one defrag success event")
}

// TestDefragLeaderNotStarvedInHighChurn verifies that in a high-churn environment
// where non-leader members re-fragment between sync cycles, the leader is eventually
// defragged rather than being perpetually skipped.
func TestDefragLeaderNotStarvedInHighChurn(t *testing.T) {
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	integration.BeforeTestExternal(t)
	testServer := integration.NewCluster(t, &integration.ClusterConfig{Size: 3})
	defer testServer.Terminate(t)

	etcdMembers := waitForMembersWithClientURLs(t, testServer)

	var status []*clientv3.StatusResponse
	for _, member := range testServer.Members {
		statusResp, err := testServer.Client(0).Status(context.TODO(), member.GRPCURL)
		require.NoError(t, err)
		// All members are fragmented.
		statusResp.DbSize = minDefragBytes
		statusResp.DbSizeInUse = minDefragBytes / 2
		status = append(status, statusResp)
	}

	fakeEtcdClient, _ := etcdcli.NewFakeEtcdClient(
		etcdMembers,
		etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3}),
		etcdcli.WithFakeStatus(status),
	)

	eventRecorder := events.NewInMemoryRecorder(t.Name(), clock.RealClock{})
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, indexer.Add(u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode)))

	controller := &DefragController{
		operatorClient:       fakeOperatorClient,
		memberLister:         fakeEtcdClient,
		defragClient:         fakeEtcdClient,
		statusClient:         fakeEtcdClient,
		leaderMover:          fakeEtcdClient,
		infrastructureLister: configv1listers.NewInfrastructureLister(indexer),
		configmapLister:      corev1listers.NewConfigMapLister(indexer),
	}

	syncCtx := factory.NewSyncContext("defrag-controller", eventRecorder)

	// Simulate high churn: after each sync, re-fragment all members.
	// Without the defraggedMembers tracking, the leader would be starved
	// because len(defragTargets) > 1 is always true.
	//
	// Expected sequence for a 3-node cluster:
	//   sync 1: defrag non-leader A
	//   sync 2: defrag non-leader B (A re-fragmented but already tracked)
	//   sync 3: leader transfer (all non-leaders defragged this cycle)
	//   sync 4: defrag former leader
	maxSyncs := 10
	var leaderDefragged bool
	for range maxSyncs {
		// Re-fragment all members to simulate high churn.
		for _, s := range status {
			s.DbSize = minDefragBytes
			s.DbSizeInUse = minDefragBytes / 2
		}

		err := controller.sync(context.TODO(), syncCtx)
		require.NoError(t, err)

		// Check if the leader was defragged by looking for a defrag attempt
		// on the leader member.
		for _, event := range eventRecorder.Events() {
			if strings.Contains(event.Message, "Moved leadership away from member") {
				leaderDefragged = true
				break
			}
		}
		if leaderDefragged {
			break
		}
	}

	assert.True(t, leaderDefragged, "leader should have been selected for defrag (via leader transfer) within %d syncs", maxSyncs)
}

func Test_isEndpointBackendFragmented(t *testing.T) {
	scenarios := []struct {
		name             string
		dbInUse          int64
		dbSize           int64
		wantIsFragmented bool
	}{
		{
			name:             "endpoint backend fragmented",
			dbSize:           minDefragBytes,
			dbInUse:          minDefragBytes / 2,
			wantIsFragmented: true,
		},
		{
			name:             "endpoint backend size meets defrag criteria, store is not fragmented",
			dbSize:           minDefragBytes,
			dbInUse:          minDefragBytes,
			wantIsFragmented: false,
		},
		{
			name:             "endpoint backend size below criteria, store is fragmented",
			dbSize:           2 * 1000,
			dbInUse:          minDefragBytes,
			wantIsFragmented: false,
		},
		{
			name:             "endpoint backend size and fragmentation below criteria",
			dbSize:           0,
			dbInUse:          0,
			wantIsFragmented: false,
		},
	}
	for i, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			member := &etcdserverpb.Member{
				Name: fmt.Sprintf("etcd-%d", i),
				ID:   12043791033664010000,
			}
			status := &clientv3.StatusResponse{
				DbSize:      scenario.dbSize,
				DbSizeInUse: scenario.dbInUse,
				Header: &etcdserverpb.ResponseHeader{
					MemberId: member.ID,
				},
			}
			if gotIsFragmented := isEndpointBackendFragmented(member, status); gotIsFragmented != scenario.wantIsFragmented {
				t.Fatalf("isEndpointBackendFragmented: want %v, got %v", scenario.wantIsFragmented, gotIsFragmented)
			}
		})
	}
}

func generateErrors(n int) []error {
	var errs []error
	for range n {
		errs = append(errs, errors.New("fail"))
	}
	return errs
}
