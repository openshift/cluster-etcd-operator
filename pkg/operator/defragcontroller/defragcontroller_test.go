package defragcontroller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/integration"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

var (
	testTLSInfo = transport.TLSInfo{
		KeyFile:        u.MustAbsPath("../../testutils/testdata/server.key.insecure"),
		CertFile:       u.MustAbsPath("../../testutils/testdata/server.crt"),
		TrustedCAFile:  u.MustAbsPath("../../testutils/testdata/ca.crt"),
		ClientCertAuth: true,
	}
)

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
		memberHealth        *etcdcli.FakeMemberHealth
		dbInUse             int64
		dbSize              int64
		defragSuccessEvents int
		wantErr             bool
		wantErrMsg          string
	}{
		{
			name:                "defrag success",
			staticPodStatus:     u.StaticPodOperatorStatus(),
			dbSize:              minDefragBytes,     // 1GB
			dbInUse:             minDefragBytes / 2, // 500MB
			defragSuccessEvents: 3,
			clusterSize:         3,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
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
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// use integration etcd to create etcd members and status
			testServer := integration.NewClusterV3(t, &integration.ClusterConfig{Size: scenario.clusterSize})
			defer testServer.Terminate(t)

			// populate MemberList
			memberListResp, err := testServer.Client(0).MemberList(context.TODO())
			require.NoError(t, err)
			etcdMembers := memberListResp.Members

			// populate Status
			var status []*clientv3.StatusResponse
			for _, member := range testServer.Members {
				statusResp, err := testServer.Client(0).Status(context.TODO(), member.GRPCAddr())
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
			eventRecorder := events.NewInMemoryRecorder(t.Name())
			require.NoError(t, err)
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			controller := &DefragController{
				operatorClient:       fakeOperatorClient,
				etcdClient:           fakeEtcdClient,
				infrastructureLister: configv1listers.NewInfrastructureLister(indexer),
			}

			err = controller.sync(context.TODO(), factory.NewSyncContext("defrag-controller", eventRecorder))
			if err != nil && !scenario.wantErr {
				t.Fatalf("unexepected error %v", err)
			}
			if err == nil && scenario.wantErr {
				t.Fatal("expected error got nil")
			}
			if err != nil && scenario.wantErr {
				if !strings.HasPrefix(err.Error(), scenario.wantErrMsg) {
					t.Fatalf("unexepected error prefix want: %q got: %q", scenario.wantErrMsg, err.Error())
				}
			}
			var defragSuccessEvents int
			lastEvent := len(eventRecorder.Events()) - 1
			for i, event := range eventRecorder.Events() {
				if strings.HasPrefix(event.Message, "etcd member has been defragmented") {
					defragSuccessEvents++
				}
				// ensure the leader was defragged last
				if i == lastEvent {
					// last event will print leader ID
					regex := regexp.MustCompile(fmt.Sprint(status[0].Leader))
					if len(regex.FindAll([]byte(event.Message), -1)) != 1 {
						t.Fatalf("expected leader defrag event to be last got %q", event.Message)
					}
				}
			}
			if defragSuccessEvents != scenario.defragSuccessEvents {
				t.Fatalf("defragSuccessEvents invalid want %d got %d", scenario.defragSuccessEvents, defragSuccessEvents)
			}
			eventRecorder.Events()
		})
	}
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
