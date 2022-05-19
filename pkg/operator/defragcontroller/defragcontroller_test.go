package defragcontroller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	"go.etcd.io/etcd/tests/v3/integration"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
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
		wantCondition       operatorv1.ConditionStatus
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
				configmapLister:      corev1listers.NewConfigMapLister(indexer),
				// to speed the tests up, in real life we use minDefragWaitDuration
				defragWaitDuration: 1 * time.Second,
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
			errSyncLoops:        maxDefragFailuresBeforeDegrade,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			fakeClientOpts: []etcdcli.FakeClientOption{
				etcdcli.WithFakeDefragErrors(generateErrors(maxDefragFailuresBeforeDegrade * 3)),
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
			syncLoops:           maxDefragFailuresBeforeDegrade + 1,
			errSyncLoops:        maxDefragFailuresBeforeDegrade,
			memberHealth:        &etcdcli.FakeMemberHealth{Healthy: 3},
			objects: []runtime.Object{
				u.FakeInfrastructureTopology(configv1.HighlyAvailableTopologyMode),
			},
			fakeClientOpts: []etcdcli.FakeClientOption{
				etcdcli.WithFakeDefragErrors(generateErrors(maxDefragFailuresBeforeDegrade * 3)),
			},
			// ignoring errors here since the first maxDefragFailuresBeforeDegrade invocations will return an error, the ones after won't
			wantDisabledCondition: operatorv1.ConditionFalse,
			wantDegradedCondition: operatorv1.ConditionFalse,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			integration.BeforeTestExternal(t)
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

			fakeOpts := []etcdcli.FakeClientOption{
				etcdcli.WithFakeClusterHealth(scenario.memberHealth),
				etcdcli.WithFakeStatus(status),
			}

			for _, o := range scenario.fakeClientOpts {
				fakeOpts = append(fakeOpts, o)
			}

			fakeEtcdClient, _ := etcdcli.NewFakeEtcdClient(etcdMembers, fakeOpts...)
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
				configmapLister:      corev1listers.NewConfigMapLister(indexer),
				// to speed the tests up, in real life we use minDefragWaitDuration
				defragWaitDuration: 1 * time.Second,
			}

			numSyncErr := 0
			for i := 0; i < scenario.syncLoops; i++ {
				err = controller.sync(context.TODO(), factory.NewSyncContext("defrag-controller", eventRecorder))
				if err != nil {
					numSyncErr++
					fmt.Printf("error on sync: %v\n", err)
				}
			}

			assert.Equal(t, scenario.errSyncLoops, numSyncErr)

			var defragSuccessEvents int
			for _, event := range eventRecorder.Events() {
				if strings.HasPrefix(event.Message, "etcd member has been defragmented") {
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
	for i := 0; i < n; i++ {
		errs = append(errs, errors.New("fail"))
	}
	return errs
}
