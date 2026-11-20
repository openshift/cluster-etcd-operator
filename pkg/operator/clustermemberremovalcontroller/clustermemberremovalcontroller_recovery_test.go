package clustermemberremovalcontroller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
)

func TestSyncEndpointRevisionRecovery(t *testing.T) {
	for _, scenario := range []string{"repairs membership", "fault tolerant unhealthy recovery", "failed removal", "fresh health disagrees", "missing snapshot", "target revision advances", "mutable endpoints lag", "minimum quorum", "machine API unavailable", "bootstrap incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			configMaps := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, configMaps.Add(bootstrapComplete.DeepCopy()))
			machines := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			nodes := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			network := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, network.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: configv1.NetworkStatus{ServiceNetwork: []string{"172.30.0.0/16"}}}))
			infra := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, infra.Add(&configv1.Infrastructure{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: configv1.InfrastructureStatus{ControlPlaneTopology: configv1.HighlyAvailableTopologyMode}}))
			namespaces := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, namespaces.Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace}}))

			status := &operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 24}}
			endpoints := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: operatorclient.TargetNamespace}, Data: map[string]string{}}
			var members []*etcdserverpb.Member
			names := []string{"old-3", "new-4", "new-5", "old-1", "old-2"}
			for i, name := range names {
				ip := fmt.Sprintf("10.0.0.%d", i+1)
				machine := machineWithHooksFor(name, ip)
				machine.Status.NodeRef = &corev1.ObjectReference{Name: name}
				if name == "old-1" || name == "old-2" || name == "old-3" {
					now := metav1.Now()
					machine.DeletionTimestamp = &now
				}
				require.NoError(t, machines.Add(machine))
				require.NoError(t, nodes.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/master": ""}}, Status: corev1.NodeStatus{Addresses: machine.Status.Addresses}}))
				status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{NodeName: name, CurrentRevision: 24})
				members = append(members, &etcdserverpb.Member{Name: name, ID: uint64(i + 1), PeerURLs: []string{fmt.Sprintf("https://%s:2380", ip)}})
				endpoints.Data[fmt.Sprintf("%x", i+1)] = ip
			}
			require.NoError(t, configMaps.Add(endpoints))
			snapshot := endpoints.DeepCopy()
			snapshot.Name = "etcd-endpoints-24"
			require.NoError(t, configMaps.Add(snapshot))

			initialUnhealthy := 2
			if scenario == "fault tolerant unhealthy recovery" {
				initialUnhealthy = 1
			}
			newEtcd := func(ms []*etcdserverpb.Member, unhealthy int) etcdcli.EtcdClient {
				c, err := etcdcli.NewFakeEtcdClient(ms, etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: len(ms) - unhealthy, Unhealthy: unhealthy}))
				require.NoError(t, err)
				return &consistentMemberHealthClient{EtcdClient: c}
			}
			operatorClient := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ObservedConfig: runtime.RawExtension{Raw: []byte(wellKnownReplicasCountSet)}}}, status, nil, nil)
			target := &clusterMemberRemovalController{
				operatorClient:                    operatorClient,
				etcdClient:                        newEtcd(members, initialUnhealthy),
				masterMachineLister:               machinelistersv1beta1.NewMachineLister(machines),
				masterMachineSelector:             labels.SelectorFromSet(labels.Set{"machine.openshift.io/cluster-api-machine-role": "master"}),
				masterNodeLister:                  corev1listers.NewNodeLister(nodes),
				masterNodeSelector:                labels.SelectorFromSet(labels.Set{"node-role.kubernetes.io/master": ""}),
				networkLister:                     configv1listers.NewNetworkLister(network),
				infraLister:                       configv1listers.NewInfrastructureLister(infra),
				configMapLister:                   corev1listers.NewConfigMapLister(configMaps),
				configMapListerForTargetNamespace: corev1listers.NewConfigMapLister(configMaps).ConfigMaps(operatorclient.TargetNamespace),
				machineAPIChecker:                 &fakeMachineAPI{isMachineAPIFunctional: func() (bool, error) { return true, nil }},
			}
			syncCtx := factory.NewSyncContext("test", events.NewInMemoryRecorder("test", clock.RealClock{}))
			memberNames := func() []string {
				ms, err := target.etcdClient.MemberList(ctx)
				require.NoError(t, err)
				var out []string
				for _, m := range ms {
					out = append(out, m.Name)
				}
				return out
			}

			if scenario == "fault tolerant unhealthy recovery" {
				stale := snapshot.DeepCopy()
				stale.Data["removed"] = "10.0.0.6"
				require.NoError(t, configMaps.Update(stale))
				quorum := ceohelpers.NewQuorumChecker(corev1listers.NewNamespaceLister(namespaces), configv1listers.NewInfrastructureLister(infra), operatorClient, target.etcdClient)
				safe, err := quorum.IsSafeToUpdateRevision()
				require.NoError(t, err)
				require.True(t, safe)
				for i := 0; i < 5; i++ {
					require.NoError(t, target.sync(ctx, syncCtx))
				}
				require.Equal(t, []string{"old-3", "new-4", "new-5", "old-1"}, memberNames(), "a stale snapshot must preserve existing unhealthy recovery even while fault tolerant")
				return
			}

			require.NoError(t, target.sync(ctx, syncCtx))
			require.Equal(t, []string{"old-3", "new-4", "new-5", "old-2"}, memberNames(), "first dead member removed")

			remaining, err := target.etcdClient.MemberList(ctx)
			require.NoError(t, err)
			target.etcdClient = newEtcd(remaining, 1)
			endpoints = endpoints.DeepCopy()
			delete(endpoints.Data, "4")
			if scenario != "mutable endpoints lag" {
				require.NoError(t, configMaps.Update(endpoints))
			}

			quorum := ceohelpers.NewQuorumChecker(corev1listers.NewNamespaceLister(namespaces), configv1listers.NewInfrastructureLister(infra), operatorClient, target.etcdClient)
			safe, err := quorum.IsSafeToUpdateRevision()
			t.Logf("revision precondition after first removal: safe=%v err=%v", safe, err)
			require.False(t, safe, "revision 25 cannot be published")

			var failedClient *failedMemberRemovalClient
			switch scenario {
			case "failed removal":
				failedClient = &failedMemberRemovalClient{EtcdClient: target.etcdClient}
				target.etcdClient = failedClient
			case "fresh health disagrees":
				current, err := target.etcdClient.MemberList(ctx)
				require.NoError(t, err)
				target.etcdClient = &differentUnhealthyMemberClient{EtcdClient: target.etcdClient, member: current[0]}
			case "missing snapshot":
				require.NoError(t, configMaps.Delete(snapshot))
			case "target revision advances":
				_, _, err := v1helpers.UpdateStaticPodStatus(ctx, operatorClient, func(status *operatorv1.StaticPodOperatorStatus) error {
					status.NodeStatuses[0].TargetRevision = 25
					return nil
				})
				require.NoError(t, err)
			case "minimum quorum":
				c, err := etcdcli.NewFakeEtcdClient(remaining, etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 1, Unhealthy: len(remaining) - 1}))
				require.NoError(t, err)
				target.etcdClient = &consistentMemberHealthClient{EtcdClient: c}
			case "bootstrap incomplete":
				incomplete := bootstrapComplete.DeepCopy()
				incomplete.Data["status"] = "in-progress"
				require.NoError(t, configMaps.Update(incomplete))
			case "machine API unavailable":
				target.machineAPIChecker = &fakeMachineAPI{isMachineAPIFunctional: func() (bool, error) { return false, nil }}
			}
			if scenario != "repairs membership" {
				err := target.sync(ctx, syncCtx)
				switch scenario {
				case "failed removal":
					require.ErrorContains(t, err, "injected removal failure")
					require.NotEmpty(t, failedClient.attemptedIDs)
					for _, id := range failedClient.attemptedIDs {
						require.Equal(t, uint64(5), id, "recovery must not fall through to a healthy voter")
					}
				case "missing snapshot":
					require.ErrorContains(t, err, "failed to get etcd-endpoints-24 configmap")
				default:
					require.NoError(t, err)
				}
				require.Equal(t, []string{"old-3", "new-4", "new-5", "old-2"}, memberNames())
				return
			}

			for i := 0; i < 5; i++ {
				require.NoError(t, target.sync(ctx, syncCtx))
			}
			t.Logf("members after 5 more syncs: %v", memberNames())
			require.Equal(t, []string{"old-3", "new-4", "new-5"}, memberNames(), "second dead pending-deletion voter must be removable without a new revision")

			remaining, err = target.etcdClient.MemberList(ctx)
			require.NoError(t, err)
			quorum = ceohelpers.NewQuorumChecker(corev1listers.NewNamespaceLister(namespaces), configv1listers.NewInfrastructureLister(infra), operatorClient, newEtcd(remaining, 0))
			safe, err = quorum.IsSafeToUpdateRevision()
			t.Logf("revision precondition after second removal: safe=%v err=%v", safe, err)
			require.True(t, safe)
		})
	}
}

// consistentMemberHealthClient uses the same health assignment for quorum and removal.
type consistentMemberHealthClient struct {
	etcdcli.EtcdClient
}

func (c *consistentMemberHealthClient) UnhealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	health, err := c.MemberHealth(ctx)
	if err != nil {
		return nil, err
	}
	return health.GetUnhealthyMembers(), nil
}

func (c *consistentMemberHealthClient) HealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	health, err := c.MemberHealth(ctx)
	if err != nil {
		return nil, err
	}
	var voting []*etcdserverpb.Member
	for _, m := range health.GetHealthyMembers() {
		if !m.IsLearner {
			voting = append(voting, m)
		}
	}
	return voting, nil
}

type failedMemberRemovalClient struct {
	etcdcli.EtcdClient
	attemptedIDs []uint64
}

func (c *failedMemberRemovalClient) MemberRemove(_ context.Context, id uint64) error {
	c.attemptedIDs = append(c.attemptedIDs, id)
	return fmt.Errorf("injected removal failure")
}

type differentUnhealthyMemberClient struct {
	etcdcli.EtcdClient
	member *etcdserverpb.Member
}

func (c *differentUnhealthyMemberClient) UnhealthyMembers(context.Context) ([]*etcdserverpb.Member, error) {
	return []*etcdserverpb.Member{c.member}, nil
}
