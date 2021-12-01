package clustermembercontroller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/assert"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/v3/mock/mockserver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestClusterMemberController_getEtcdPeerURLToScale(t *testing.T) {
	now := time.Now()

	scenerios := []struct {
		name        string
		objects     []runtime.Object
		etcdMembers []*etcdserverpb.Member

		want    string
		isIPv6  bool
		wantErr bool
	}{
		{
			name: "test oldest node already has member",
			objects: []runtime.Object{
				u.GetMockClusterConfig(3),
				u.FakeNode("master-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(2 * time.Second)})),
				u.FakeNode("master-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1"), u.WithCreatedTimeStamp(metav1.Time{Time: now})),
				u.FakeNode("master-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(1 * time.Second)})),
			},
			etcdMembers: []*etcdserverpb.Member{
				{PeerURLs: []string{"https://10.0.0.1:2380"}},
			},
			want: "https://10.0.0.2:2380",
		},
		{
			name: "test all nodes have members",
			objects: []runtime.Object{
				u.GetMockClusterConfig(3),
				u.FakeNode("master-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(2 * time.Second)})),
				u.FakeNode("master-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1"), u.WithCreatedTimeStamp(metav1.Time{Time: now})),
				u.FakeNode("master-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(1 * time.Second)})),
			},
			etcdMembers: []*etcdserverpb.Member{
				{PeerURLs: []string{"https://10.0.0.1:2380"}},
				{PeerURLs: []string{"https://10.0.0.2:2380"}},
				{PeerURLs: []string{"https://10.0.0.3:2380"}},
			},
			want: "",
		},
		{
			name: "test nodes exceed etcd surge limit",
			objects: []runtime.Object{
				u.GetMockClusterConfig(3),
				u.FakeNode("master-4", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.5"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(4 * time.Second)})),
				u.FakeNode("master-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(2 * time.Second)})),
				u.FakeNode("master-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1"), u.WithCreatedTimeStamp(metav1.Time{Time: now})),
				u.FakeNode("master-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(1 * time.Second)})),
				u.FakeNode("master-3", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.4"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(3 * time.Second)})),
			},
			etcdMembers: []*etcdserverpb.Member{
				{PeerURLs: []string{"https://10.0.0.1:2380"}},
				{PeerURLs: []string{"https://10.0.0.2:2380"}},
				{PeerURLs: []string{"https://10.0.0.3:2380"}},
				{PeerURLs: []string{"https://10.0.0.4:2380"}},
			},
			want: "",
		},
		{
			name: "test missing openshift-etcd/cluster-config-v1 configmap",
			objects: []runtime.Object{
				u.FakeNode("master-2", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.3"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(2 * time.Second)})),
				u.FakeNode("master-0", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.1"), u.WithCreatedTimeStamp(metav1.Time{Time: now})),
				u.FakeNode("master-1", u.WithMasterLabel(), u.WithNodeInternalIP("10.0.0.2"), u.WithCreatedTimeStamp(metav1.Time{Time: now.Add(1 * time.Second)})),
			},
			etcdMembers: []*etcdserverpb.Member{
				{PeerURLs: []string{"https://10.0.0.1:2380"}},
			},
			wantErr: true,
		},
	}
	for _, scenario := range scenerios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			assert.NoError(t, err)
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					assert.NoError(t, err)
				}
			}

			c := &ClusterMemberController{
				etcdClient:      fakeEtcdClient,
				networkLister:   u.FakeNetworkLister(t, scenario.isIPv6),
				nodeLister:      corev1listers.NewNodeLister(indexer),
				configMapLister: corev1listers.NewConfigMapLister(indexer),
			}
			got, err := c.getEtcdPeerURLToScale(context.TODO())
			if (err != nil) != scenario.wantErr {
				t.Errorf("getEtcdPeerURLToScale() error = %v, wantErr %v", err, scenario.wantErr)
				return
			}
			if !reflect.DeepEqual(got, scenario.want) {
				t.Errorf("getEtcdPeerURLToScale() got = %v, want %v", got, scenario.want)
			}
		})
	}
}

func TestClusterMemberController_ensureEtcdLearnerPromotion(t *testing.T) {
	type member struct {
		isLearner    bool
		isPromotable bool
	}

	scenerios := []struct {
		name          string
		etcdMemberMap map[string]*member
		wantEvents    []string
		wantErr       bool
	}{
		{
			name: "test promote all ready members",
			etcdMemberMap: map[string]*member{
				"etcd-0": {isLearner: true, isPromotable: true},
				"etcd-1": {isLearner: true, isPromotable: true},
				"etcd-2": {isLearner: true, isPromotable: true},
			},
			wantEvents: []string{msgLearnerPromoted, msgLearnerPromoted, msgLearnerPromoted},
		},
		{
			name: "test promote and skip unready learner",
			etcdMemberMap: map[string]*member{
				"etcd-0": {isLearner: false},
				"etcd-1": {isLearner: true, isPromotable: false},
				"etcd-2": {isLearner: true, isPromotable: true},
			},
			wantEvents: []string{msgLearnerPromoted, msgPromotionFailedNotReady},
		},
		{
			name: "test skip promote all unready learners",
			etcdMemberMap: map[string]*member{
				"etcd-0": {isLearner: true, isPromotable: false},
				"etcd-1": {isLearner: true, isPromotable: false},
				"etcd-2": {isLearner: true, isPromotable: false},
			},
			wantEvents: []string{msgPromotionFailedNotReady, msgPromotionFailedNotReady, msgPromotionFailedNotReady},
		},
		{
			name: "test no learners",
			etcdMemberMap: map[string]*member{
				"etcd-0": {isLearner: false},
				"etcd-1": {isLearner: false},
				"etcd-2": {isLearner: false},
			},
			wantEvents: []string{},
		},
	}
	for _, scenario := range scenerios {
		t.Run(scenario.name, func(t *testing.T) {
			etcdClusterSize := len(scenario.etcdMemberMap)
			mockEtcd, err := mockserver.StartMockServers(etcdClusterSize)
			assert.NoError(t, err)
			defer mockEtcd.Stop()

			var etcdMembers []*etcdserverpb.Member
			var unreadyLearnerIDs []uint64
			for i := 0; i < len(scenario.etcdMemberMap); i++ {
				member := scenario.etcdMemberMap[fmt.Sprintf("etcd-%d", i)]
				// Populate etcd members.
				etcdMembers = append(etcdMembers, u.FakeEtcdMember(i, member.isLearner, mockEtcd.Servers))
				if !member.isPromotable {
					unreadyLearnerIDs = append(unreadyLearnerIDs, etcdMembers[len(etcdMembers)-1].ID)
				}
			}

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(etcdMembers, etcdcli.WithFakeUnreadyLearnerIDs(unreadyLearnerIDs))
			assert.NoError(t, err)

			eventRecorder := events.NewInMemoryRecorder(t.Name())
			c := &ClusterMemberController{
				etcdClient: fakeEtcdClient,
			}
			err = c.ensureEtcdLearnerPromotion(context.TODO(), eventRecorder)
			if (err != nil) != scenario.wantErr {
				t.Errorf("ensureEtcdLearnerPromotion() error = %v, wantErr %v", err, scenario.wantErr)
				return
			}
			wantEvents := scenario.wantEvents
			var gotEvents []string
			for _, event := range eventRecorder.Events() {
				gotEvents = append(gotEvents, event.Message)
				for i, msg := range scenario.wantEvents {
					if strings.Contains(event.Message, msg) {
						// Remove from slice
						wantEvents = append(wantEvents[:i], wantEvents[i+1:]...)
						break
					}
				}
			}
			if len(wantEvents) != 0 {
				t.Errorf("ensureEtcdLearnerPromotion() wantEvents: %#v, gotEvents: %#v", scenario.wantEvents, gotEvents)
			}
		})
	}
}
