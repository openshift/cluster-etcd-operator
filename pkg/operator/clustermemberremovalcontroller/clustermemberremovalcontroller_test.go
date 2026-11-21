package clustermemberremovalcontroller

import (
	"context"
	"fmt"
	"testing"

	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/stretchr/testify/require"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
)

var (
	bootstrapComplete = u.BootstrapConfigMap(u.WithBootstrapStatus("complete"))
)

func TestAttemptToRemoveLearningMember(t *testing.T) {
	scenarios := []struct {
		name                                     string
		initialObjectsForMachineLister           []runtime.Object
		initialObjectsForConfigMapTargetNSLister []runtime.Object
		initialEtcdMemberList                    []*etcdserverpb.Member
		validateFn                               func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
	}{
		{
			name: "learning member pending deletion is removed",
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:      "m-4",
					ID:        4,
					IsLearner: true,
					PeerURLs:  []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 4 {
						t.Fatalf("expected the member: %v to be removed from the etcd cluster but it wasn't", member)
					}
				}
			},
		},

		{
			name:                  "voting member pending deletion is NOT removed",
			initialEtcdMemberList: wellKnownEtcdMemberList(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				m0 := machines[0].(*machinev1beta1.Machine)
				m0.DeletionTimestamp = &metav1.Time{}
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
			},
		},

		{
			name: "excessive voting member pending deletion is NOT removed",
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			eventRecorder := events.NewRecorder(fake.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-member-removal-controller", &corev1.ObjectReference{}, clock.RealClock{})
			configMapTargetNSIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForConfigMapTargetNSLister {
				configMapTargetNSIndexer.Add(initialObj)
			}
			configMapTargetNSLister := corev1listers.NewConfigMapLister(configMapTargetNSIndexer).ConfigMaps("openshift-etcd")

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
			if err != nil {
				t.Fatal(err)
			}

			// act
			target := clusterMemberRemovalController{
				etcdClient:                        fakeEtcdClient,
				masterMachineLister:               machineLister,
				masterMachineSelector:             machineSelector,
				configMapListerForTargetNamespace: configMapTargetNSLister,
			}
			err = target.attemptToRemoveLearningMember(context.TODO(), eventRecorder)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient)
			}
		})
	}
}

func TestClusterMemberRemovalController(t *testing.T) {
	alwaysTrueIsFunctionalMachineAPIFn := func() (bool, error) { return true, nil }

	scenarios := []struct {
		name                             string
		isFunctionalMachineAPIFn         func() (bool, error)
		initialObjectsForConfigMapLister []runtime.Object
		initialObjectsForNodeLister      []runtime.Object
		initialObjectsForMachineLister   []runtime.Object
		initialEtcdMemberList            []*etcdserverpb.Member
		fakeEtcdClientOptions            []etcdcli.FakeClientOption
		validateFn                       func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
		serviceNetwork                   string
		expectError                      bool
	}{
		// scenario 1
		{
			name:                             "happy path: an etcd member has a corresponding machine and node resources",
			serviceNetwork:                   "172.30.0.0/16",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialObjectsForNodeLister:      []runtime.Object{wellKnownMasterNode()},
			initialObjectsForMachineLister:   []runtime.Object{wellKnownMasterMachine()},
			initialEtcdMemberList:            wellKnownEtcdMemberList(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly one etcd member, got %v", memberList)
				}
			},
		},

		// scenario 1 (ipv6)
		{
			name:                             "happy path (ipv6): an etcd member has a corresponding machine and node resources",
			serviceNetwork:                   "fd02::/112",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMapIpv6()},
			initialObjectsForNodeLister:      []runtime.Object{wellKnownMasterNodeIpv6()},
			initialObjectsForMachineLister:   []runtime.Object{wellKnownMasterMachineIpv6()},
			initialEtcdMemberList:            wellKnownEtcdMemberListIpv6(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly one etcd member, got %v", memberList)
				}
			},
		},

		// scenario 2
		{
			name:                             "an etcd member doesn't have a corresponding machine nor node resource and it is removed",
			serviceNetwork:                   "172.30.0.0/16",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialEtcdMemberList:            wellKnownEtcdMemberList(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 0 {
					t.Errorf("expected an empty member list, got %v", memberList)
				}
			},
		},

		// scenario 2 (ipv6)
		{
			name:                             "(ipv6) an etcd member doesn't have a corresponding machine nor node resource and it is removed",
			serviceNetwork:                   "fd02::/112",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMapIpv6()},
			initialEtcdMemberList:            wellKnownEtcdMemberListIpv6(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 0 {
					t.Errorf("expected an empty member list, got %v", memberList)
				}
			},
		},

		// scenario 3
		{
			name:                             "an etcd member with only a corresponding machine resource is not removed",
			serviceNetwork:                   "172.30.0.0/16",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialObjectsForMachineLister:   []runtime.Object{wellKnownMasterMachine()},
			initialEtcdMemberList:            wellKnownEtcdMemberList(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly one etcd member, got %v", memberList)
				}
			},
		},

		// scenario 3 (ipv6)
		{
			name:                             "(ipv6) an etcd member with only a corresponding machine resource is not removed",
			serviceNetwork:                   "fd02::/112",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMapIpv6()},
			initialObjectsForMachineLister:   []runtime.Object{wellKnownMasterMachineIpv6()},
			initialEtcdMemberList:            wellKnownEtcdMemberListIpv6(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly one etcd member, got %v", memberList)
				}
			},
		},

		// scenario 4
		{
			name:                             "an etcd member with only a corresponding node resource is not removed",
			serviceNetwork:                   "172.30.0.0/16",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialObjectsForNodeLister:      []runtime.Object{wellKnownMasterNode()},
			initialEtcdMemberList:            wellKnownEtcdMemberList(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly one etcd member, got %v", memberList)
				}
			},
		},

		// scenario 4 (ipv6)
		{
			name:                             "(ipv6) an etcd member with only a corresponding node resource is not removed",
			serviceNetwork:                   "fd02::/112",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMapIpv6()},
			initialObjectsForNodeLister:      []runtime.Object{wellKnownMasterNodeIpv6()},
			initialEtcdMemberList:            wellKnownEtcdMemberListIpv6(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly one etcd member, got %v", memberList)
				}
			},
		},

		// scenario 5: half-orphan - node is gone but the machine is a tombstone (pending deletion)
		{
			name:                             "an etcd member whose node is gone and whose machine is pending deletion is removed",
			serviceNetwork:                   "172.30.0.0/16",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialObjectsForMachineLister: []runtime.Object{
				machinePendingDeletionFor("m-1", "10.0.139.78"),
				machinePendingDeletionFor("m-2", "10.0.139.79"),
				machinePendingDeletionFor("m-3", "10.0.139.80"),
			},
			initialEtcdMemberList: wellKnownEtcdMemberList(),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 0 {
					t.Errorf("expected an empty member list, got %v", memberList)
				}
			},
		},

		// scenario 6: node and machine are gone but the member still reports healthy -> not removed, error
		{
			name:                             "an etcd member whose node is gone but that reports healthy is not removed and errors",
			serviceNetwork:                   "172.30.0.0/16",
			isFunctionalMachineAPIFn:         alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForConfigMapLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			initialEtcdMemberList:            wellKnownEtcdMemberList(),
			// all members report healthy, so the unbacked member must not be removed
			fakeEtcdClientOptions: []etcdcli.FakeClientOption{etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3})},
			expectError:           true,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected all three etcd members to remain, got %v", memberList)
				}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: scenario.isFunctionalMachineAPIFn}

			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForConfigMapLister {
				configMapIndexer.Add(initialObj)
			}
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("openshift-etcd")

			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForNodeLister {
				nodeIndexer.Add(initialObj)
			}
			nodeLister := corev1listers.NewNodeLister(nodeIndexer)
			nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
			if err != nil {
				t.Fatal(err)
			}

			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{scenario.serviceNetwork}}})
			networkLister := configv1listers.NewNetworkLister(networkIndexer)

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList, scenario.fakeEtcdClientOptions...)
			if err != nil {
				t.Fatal(err)
			}

			// act
			target := clusterMemberRemovalController{
				etcdClient:                        fakeEtcdClient,
				machineAPIChecker:                 fakeMachineAPIChecker,
				configMapListerForTargetNamespace: configMapLister,
				masterNodeSelector:                nodeSelector,
				masterNodeLister:                  nodeLister,
				masterMachineSelector:             machineSelector,
				masterMachineLister:               machineLister,
				networkLister:                     networkLister,
			}
			err = target.removeUnhealthyMemberWithoutNode(context.TODO())
			if scenario.expectError {
				require.Error(t, err)
			} else if err != nil {
				t.Fatal(err)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient)
			}
		})
	}
}

// TestSync exercises the full sync() gate ordering, in particular that dead members (node gone)
// are removed even while a revision rollout is in progress, while the quorum-sensitive scale-down
// and learner paths remain gated behind revision stability.
func TestSync(t *testing.T) {
	votingMember4 := &etcdserverpb.Member{Name: "m-4", ID: 4, PeerURLs: []string{"https://10.0.139.81:1234"}}
	learnerMember4 := &etcdserverpb.Member{Name: "m-4", ID: 4, IsLearner: true, PeerURLs: []string{"https://10.0.139.81:1234"}}

	members4 := func() []*etcdserverpb.Member { return append(wellKnownEtcdMemberList(), votingMember4) }
	membersLearner := func() []*etcdserverpb.Member { return append(wellKnownEtcdMemberList(), learnerMember4) }

	nodes123 := func() []runtime.Object {
		return []runtime.Object{
			masterNodeFor("m-1", "10.0.139.78"),
			masterNodeFor("m-2", "10.0.139.79"),
			masterNodeFor("m-3", "10.0.139.80"),
		}
	}
	nodes1234 := func() []runtime.Object { return append(nodes123(), masterNodeFor("m-4", "10.0.139.81")) }

	machines123AndPendingM4 := func() []runtime.Object {
		return append(wellKnownMasterMachines(), machinePendingDeletionFor("m-4", "10.0.139.81"))
	}

	endpoints4 := map[string]string{"m-1": "10.0.139.78", "m-2": "10.0.139.79", "m-3": "10.0.139.80", "m-4": "10.0.139.81"}
	endpoints3 := map[string]string{"m-1": "10.0.139.78", "m-2": "10.0.139.79", "m-3": "10.0.139.80"}

	scenarios := []struct {
		name                 string
		bootstrapComplete    bool
		machineAPIFunctional bool
		revisionStable       bool
		members              []*etcdserverpb.Member
		health               *etcdcli.FakeMemberHealth
		nodes                []runtime.Object
		machines             []runtime.Object
		endpoints            map[string]string
		expectError          bool
		expectedRemainingIDs []uint64
	}{
		{
			// A: dead member with no node and no machine is removed mid-rollout
			name:                 "true orphan is removed while a revision rollout is in progress",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       false,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1},
			nodes:                nodes123(),
			machines:             wellKnownMasterMachines(),
			endpoints:            endpoints4,
			expectedRemainingIDs: []uint64{1, 2, 3},
		},
		{
			// J: half-orphan (node gone, machine pending deletion) is removed mid-rollout
			name:                 "half-orphan with a tombstone machine is removed while a revision rollout is in progress",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       false,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1},
			nodes:                nodes123(),
			machines:             machines123AndPendingM4(),
			endpoints:            endpoints4,
			expectedRemainingIDs: []uint64{1, 2, 3},
		},
		{
			// B: a member pending deletion whose node still exists is not removed during a rollout
			name:                 "voting member pending deletion with a node is not removed during a rollout",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       false,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 4},
			nodes:                nodes1234(),
			machines:             machines123AndPendingM4(),
			endpoints:            endpoints4,
			expectedRemainingIDs: []uint64{1, 2, 3, 4},
		},
		{
			// C: a learner pending deletion is not removed during a rollout
			name:                 "learner pending deletion is not removed during a rollout",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       false,
			members:              membersLearner(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 4},
			nodes:                nodes123(),
			machines:             machines123AndPendingM4(),
			endpoints:            endpoints3, // learners are excluded from the etcd-endpoints configmap
			expectedRemainingIDs: []uint64{1, 2, 3, 4},
		},
		{
			// D: with a stable revision and matching endpoints, scale-down removes the member pending deletion
			name:                 "scale-down removes a member pending deletion when the revision is stable",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       true,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 4},
			nodes:                nodes1234(),
			machines:             machines123AndPendingM4(),
			endpoints:            endpoints4,
			expectedRemainingIDs: []uint64{1, 2, 3},
		},
		{
			// E: stable revision but the endpoints configmap lags live membership -> nothing removed
			name:                 "nothing is removed when the etcd-endpoints configmap lags live membership",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       true,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 4},
			nodes:                nodes123(),
			machines:             machines123AndPendingM4(),
			endpoints:            endpoints3, // does not include the 4th live voting member
			expectedRemainingIDs: []uint64{1, 2, 3, 4},
		},
		{
			// F: machine API not functional -> the whole controller is a no-op, even for a true orphan
			name:                 "no removal when the machine API is not functional",
			bootstrapComplete:    true,
			machineAPIFunctional: false,
			revisionStable:       false,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1},
			nodes:                nodes123(),
			machines:             wellKnownMasterMachines(),
			endpoints:            endpoints4,
			expectedRemainingIDs: []uint64{1, 2, 3, 4},
		},
		{
			// G: bootstrap not complete -> the whole controller is a no-op
			name:                 "no removal when bootstrap is not complete",
			bootstrapComplete:    false,
			machineAPIFunctional: true,
			revisionStable:       false,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1},
			nodes:                nodes123(),
			machines:             wellKnownMasterMachines(),
			endpoints:            endpoints4,
			expectedRemainingIDs: []uint64{1, 2, 3, 4},
		},
		{
			// H+I: an unbacked member that still reports healthy is not removed and the error is
			// returned (aggregated) even though the revision is in progress (previously swallowed).
			name:                 "unbacked but healthy member is not removed and the error is surfaced during a rollout",
			bootstrapComplete:    true,
			machineAPIFunctional: true,
			revisionStable:       false,
			members:              members4(),
			health:               &etcdcli.FakeMemberHealth{Healthy: 4}, // IsMemberHealthy reports healthy
			nodes:                nodes123(),
			machines:             wellKnownMasterMachines(),
			endpoints:            endpoints4,
			expectError:          true,
			expectedRemainingIDs: []uint64{1, 2, 3, 4},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: func() (bool, error) { return scenario.machineAPIFunctional, nil }}

			// general configmap lister (kube-system/bootstrap for IsBootstrapComplete)
			generalCMIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if scenario.bootstrapComplete {
				require.NoError(t, generalCMIndexer.Add(bootstrapComplete))
			}
			generalCMLister := corev1listers.NewConfigMapLister(generalCMIndexer)

			// target-namespace configmap lister (etcd-endpoints)
			targetCMIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, targetCMIndexer.Add(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
				Data:       scenario.endpoints,
			}))
			targetCMLister := corev1listers.NewConfigMapLister(targetCMIndexer).ConfigMaps("openshift-etcd")

			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.nodes {
				require.NoError(t, nodeIndexer.Add(obj))
			}
			nodeLister := corev1listers.NewNodeLister(nodeIndexer)
			nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
			require.NoError(t, err)

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.machines {
				require.NoError(t, machineIndexer.Add(obj))
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			require.NoError(t, err)

			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{"172.30.0.0/16"}}}))
			networkLister := configv1listers.NewNetworkLister(networkIndexer)

			infraIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, infraIndexer.Add(&configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Status:     configv1.InfrastructureStatus{ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
			}))
			infraLister := configv1listers.NewInfrastructureLister(infraIndexer)

			var status *operatorv1.StaticPodOperatorStatus
			if scenario.revisionStable {
				status = u.StaticPodOperatorStatus(u.WithLatestRevision(1), u.WithNodeStatusAtCurrentRevision(1), u.WithNodeStatusAtCurrentRevision(1), u.WithNodeStatusAtCurrentRevision(1))
			} else {
				status = u.StaticPodOperatorStatus(u.WithLatestRevision(2), u.WithNodeStatusAtCurrentRevision(1))
			}
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{ObservedConfig: runtime.RawExtension{Raw: []byte(wellKnownReplicasCountSet)}},
			}, status, nil, nil)

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.members, etcdcli.WithFakeClusterHealth(scenario.health))
			require.NoError(t, err)

			target := clusterMemberRemovalController{
				operatorClient:                    fakeOperatorClient,
				etcdClient:                        fakeEtcdClient,
				machineAPIChecker:                 fakeMachineAPIChecker,
				configMapLister:                   generalCMLister,
				configMapListerForTargetNamespace: targetCMLister,
				masterNodeSelector:                nodeSelector,
				masterNodeLister:                  nodeLister,
				masterMachineSelector:             machineSelector,
				masterMachineLister:               machineLister,
				networkLister:                     networkLister,
				infraLister:                       infraLister,
			}

			recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
			err = target.sync(context.TODO(), factory.NewSyncContext("test", recorder))
			if scenario.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			memberList, err := fakeEtcdClient.MemberList(context.TODO())
			require.NoError(t, err)
			gotIDs := sets.NewInt64()
			for _, m := range memberList {
				gotIDs.Insert(int64(m.ID))
			}
			wantIDs := sets.NewInt64()
			for _, id := range scenario.expectedRemainingIDs {
				wantIDs.Insert(int64(id))
			}
			if !gotIDs.Equal(wantIDs) {
				t.Errorf("unexpected remaining members: got IDs %v, want %v", gotIDs.List(), wantIDs.List())
			}
		})
	}
}

func TestAttemptToScaleDown(t *testing.T) {
	scenarios := []struct {
		name                                     string
		initialObjectsForMachineLister           []runtime.Object
		initialObservedConfigInput               string
		initialObjectsForConfigMapTargetNSLister []runtime.Object
		initialEtcdMemberList                    []*etcdserverpb.Member
		fakeEtcdClientOptions                    etcdcli.FakeClientOption
		indexerObjs                              []any
		validateFn                               func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
		expectedError                            error
	}{
		{
			name:                       "scale down by one machine",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 4, Unhealthy: 0}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 4 {
						t.Fatalf("expected the member: %v to be removed from the etcd cluster but it wasn't", member)
					}
				}
			},
		},
		{
			name:                       "scale down by one unhealthy machine pending deletion from 3 master nodes",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				return wellKnownEtcdMemberList()
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				m0 := machines[0].(*machinev1beta1.Machine)
				m0.DeletionTimestamp = &metav1.Time{}
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				return []runtime.Object{wellKnownEtcdEndpointsConfigMap()}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 2 {
					t.Errorf("expected exactly 2 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 1 {
						t.Fatalf("expected the member: %v to be removed from the etcd cluster but it wasn't", member)
					}
				}
			},
		},
		{
			name:                       "scale down by one unhealthy machine pending deletion from 3 master nodes",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				return wellKnownEtcdMemberList()
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				m0 := machines[0].(*machinev1beta1.Machine)
				m0.DeletionTimestamp = &metav1.Time{}
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				return []runtime.Object{wellKnownEtcdEndpointsConfigMap()}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 2, Unhealthy: 1}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 2 {
					t.Errorf("expected exactly 2 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 1 {
						t.Fatalf("expected the member: %v to be removed from the etcd cluster but it wasn't", member)
					}
				}
			},
		},
		{
			name:                       "skip scaling down, 3 master healthy machines from 3 master nodes", // one machine pending deletion
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				return wellKnownEtcdMemberList()
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				// set deletion ts
				m0 := machines[0].(*machinev1beta1.Machine)
				m0.DeletionTimestamp = &metav1.Time{}
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				return []runtime.Object{wellKnownEtcdEndpointsConfigMap()}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "skip scaling down by one unhealthy machine from 4 master nodes", // no machine pending deletion
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				machines = append(machines, machineFor("m-4", "10.0.139.81"))
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "scaling down by one unhealthy machine from 4 master nodes",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				m0 := machines[0].(*machinev1beta1.Machine)
				m0.DeletionTimestamp = &metav1.Time{}
				machines = append(machines, machineFor("m-4", "10.0.139.81"))
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 1 {
						t.Fatalf("expected the member: %v to be removed from the etcd cluster but it wasn't", member)
					}
				}
			},
		},
		{
			name:                                     "no excessive machine",
			initialObjectsForMachineLister:           wellKnownMasterMachines(),
			initialObservedConfigInput:               wellKnownReplicasCountSet,
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			indexerObjs:                              []any{bootstrapComplete},
		},
		{
			name:                       "excessive machine without the hooks",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			indexerObjs: []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "excessive machine without deletion ts set",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			indexerObjs: []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "member machine with deletion ts set",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList:      wellKnownEtcdMemberList(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				m0 := machines[0].(*machinev1beta1.Machine)
				m0.DeletionTimestamp = &metav1.Time{}
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			indexerObjs:                              []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "excessive machine that hasn't made to be a voting member",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{wellKnownEtcdEndpointsConfigMap()},
			indexerObjs:                              []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "mismatch of the number of members between the cache and the cluster",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				return []*etcdserverpb.Member{
					{
						Name:     "m-1",
						ID:       1,
						PeerURLs: []string{"https://10.0.139.78:1234"},
					},
					{
						Name:     "m-2",
						ID:       2,
						PeerURLs: []string{"https://10.0.139.79:1234"},
					},
				}
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m3 := machineWithHooksFor("m-3", "10.0.139.80")
				m3.DeletionTimestamp = &metav1.Time{}
				return []runtime.Object{machineWithHooksFor("m-1", "10.0.139.78"), machineWithHooksFor("m-2", "10.0.139.79"), m3}
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			indexerObjs: []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 2 {
					t.Errorf("expected exactly 2 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "scale down only by one machine at a time",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				}, &etcdserverpb.Member{
					Name:     "m-5",
					ID:       5,
					PeerURLs: []string{"https://10.0.139.82:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				m5 := machineWithHooksFor("m-5", "10.0.139.82")
				m5.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4, m5)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				cm.Data["m-5"] = "10.0.139.82"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 5, Unhealthy: 0}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}

				// depending on the machine ordering, it can be either m4 or m5 being deleted, but never both
				membersById := make(map[uint64]bool)
				for _, member := range memberList {
					membersById[member.ID] = true
				}

				if membersById[uint64(4)] == membersById[uint64(5)] {
					t.Errorf("neither 4 nor 5 were deleted, but got member list: %v", memberList)
				}
			},
		},
		{
			name:                       "member not removed when unhealthy members found",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1}),
			expectedError:         fmt.Errorf("cannot proceed with scaling down, unhealthy voting etcd members found: [https://10.0.139.78:1234] but none are pending deletion"),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "remove unhealthy voting member whose machine is pending deletion only if quorum is maintained",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				machines := wellKnownMasterMachines()
				// override the default health config for test scenario
				m, ok := machines[0].(*machinev1beta1.Machine)
				if !ok {
					t.Fatalf("expected type *machinev1beta1.Machine, but got %T instead", m)
				}
				m.DeletionTimestamp = &metav1.Time{}
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1}),
			indexerObjs:           []any{bootstrapComplete},
			expectedError:         nil,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 1 {
						t.Errorf("not expected member with id %d", member.ID)
					}
				}
			},
		},
		{
			name:                       "keep voting member whose machine is pending deletion if cluster is unhealthy",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m4.DeletionTimestamp = &metav1.Time{}
				machines := wellKnownMasterMachines()
				machines = append(machines, m4)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 1}),
			expectedError:         fmt.Errorf("cannot proceed with scaling down, unhealthy voting etcd members found: [https://10.0.139.78:1234] but none are pending deletion"),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                       "scale down only by one machine at a time when more than one machine pending deletion while quorum maintained",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				}, &etcdserverpb.Member{
					Name:     "m-5",
					ID:       5,
					PeerURLs: []string{"https://10.0.139.82:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				m4 := machineWithHooksFor("m-4", "10.0.139.81")
				m5 := machineWithHooksFor("m-5", "10.0.139.82")
				machines := wellKnownMasterMachines()
				// override the default health config for test scenario
				m1, ok := machines[0].(*machinev1beta1.Machine)
				if !ok {
					t.Fatalf("expected type *machinev1beta1.Machine, but got %T instead", m1)
				}
				m1.DeletionTimestamp = &metav1.Time{}
				m2, ok := machines[1].(*machinev1beta1.Machine)
				if !ok {
					t.Fatalf("expected type *machinev1beta1.Machine, but got %T instead", m2)
				}
				m2.DeletionTimestamp = &metav1.Time{}
				machines = append(machines, m4, m5)
				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-4"] = "10.0.139.81"
				cm.Data["m-5"] = "10.0.139.82"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 2}),
			indexerObjs:           []any{bootstrapComplete},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 4 {
					t.Errorf("expected exactly 4 members, got %v", len(memberList))
				}
				for _, member := range memberList {
					if member.ID == 1 {
						t.Errorf("not expected member with id %d", member.ID)
					}
				}
			},
		},
		{
			name:                       "excessive voting member with multiple pending deletions are removed in the corresponding failure domain",
			initialObservedConfigInput: wellKnownReplicasCountSet,
			initialEtcdMemberList: func() []*etcdserverpb.Member {
				members := append(wellKnownEtcdMemberList(), &etcdserverpb.Member{
					Name:     "m-4",
					ID:       4,
					PeerURLs: []string{"https://10.0.139.81:1234"},
				})
				return members
			}(),
			initialObjectsForMachineLister: func() []runtime.Object {
				ma1 := machineWithHooksFor("m-a-1", "10.0.139.81")

				machines := []runtime.Object{ma1}
				for _, m := range wellKnownMasterMachines() {
					machine := m.(*machinev1beta1.Machine)
					machine.DeletionTimestamp = &metav1.Time{}
					machines = append(machines, machine)
				}

				return machines
			}(),
			initialObjectsForConfigMapTargetNSLister: func() []runtime.Object {
				cm := wellKnownEtcdEndpointsConfigMap()
				cm.Data["m-a-1"] = "10.0.139.81"
				return []runtime.Object{cm}
			}(),
			fakeEtcdClientOptions: etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 4, Unhealthy: 0}),
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}

				if len(memberList) != 3 {
					t.Errorf("expected exactly 3 members, got %v", len(memberList))
				}

				for _, member := range memberList {
					if member.ID == 1 {
						t.Fatalf("expected the member: %v to be removed from the etcd cluster but it wasn't", member)
					}
				}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			eventRecorder := events.NewRecorder(fake.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-member-removal-controller", &corev1.ObjectReference{}, clock.RealClock{})
			configMapTargetNSIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForConfigMapTargetNSLister {
				configMapTargetNSIndexer.Add(initialObj)
			}
			configMapTargetNSLister := corev1listers.NewConfigMapLister(configMapTargetNSIndexer).ConfigMaps("openshift-etcd")

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}

			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.indexerObjs {
				require.NoError(t, indexer.Add(obj))
			}

			if scenario.fakeEtcdClientOptions == nil {
				scenario.fakeEtcdClientOptions = etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Unhealthy: 0})
			}
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList, scenario.fakeEtcdClientOptions)
			if err != nil {
				t.Fatal(err)
			}
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					ObservedConfig: runtime.RawExtension{Raw: []byte(scenario.initialObservedConfigInput)},
				},
			}, u.StaticPodOperatorStatus(), nil, nil)

			// act
			target := clusterMemberRemovalController{
				operatorClient:                    fakeOperatorClient,
				etcdClient:                        fakeEtcdClient,
				masterMachineLister:               machineLister,
				masterMachineSelector:             machineSelector,
				configMapListerForTargetNamespace: configMapTargetNSLister,
			}
			err = target.attemptToScaleDown(context.TODO(), eventRecorder)
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from attemptToScaleDown method")
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatal(err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient)
			}
		})
	}
}

func TestMemberOrderingEquals(t *testing.T) {
	left := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
		u.FakeEtcdMemberWithoutServer(3),
	}
	right := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
		u.FakeEtcdMemberWithoutServer(3),
	}
	require.True(t, membersEqual(left, right))

	// changed ordering
	right = []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(3),
		u.FakeEtcdMemberWithoutServer(2),
	}
	require.True(t, membersEqual(left, right))

	// just the same IDs, bogus names
	right = []*etcdserverpb.Member{{ID: 3, Name: "lol3"}, {ID: 1, Name: "lol1"}, {ID: 2, Name: "lol2"}}
	require.True(t, membersEqual(left, right))

	// different count
	right = []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(3),
		u.FakeEtcdMemberWithoutServer(2),
	}
	require.False(t, membersEqual(left, right))

}

func wellKnownEtcdEndpointsConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
		Data: map[string]string{
			"m-1": "10.0.139.78",
			"m-2": "10.0.139.79",
			"m-3": "10.0.139.80",
		},
	}
}

func wellKnownEtcdEndpointsConfigMapIpv6() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
		Data: map[string]string{
			"m-0": "fd2e:6f44:5dd8:c956::16",
		},
	}
}

func wellKnownEtcdMemberList() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{
			Name:     "m-1",
			ID:       1,
			PeerURLs: []string{"https://10.0.139.78:1234"},
		},
		{
			Name:     "m-2",
			ID:       2,
			PeerURLs: []string{"https://10.0.139.79:1234"},
		},
		{
			Name:     "m-3",
			ID:       3,
			PeerURLs: []string{"https://10.0.139.80:1234"},
		},
	}
}

func wellKnownEtcdMemberListIpv6() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{
			Name:     "m-0",
			ID:       8,
			PeerURLs: []string{"https://[fd2e:6f44:5dd8:c956::16]:1234"},
		},
	}
}

func wellKnownMasterNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "m-0", Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: "10.0.139.78",
			},
		}},
	}
}

func masterNodeFor(name, internalIP string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: internalIP,
			},
		}},
	}
}

func wellKnownMasterNodeIpv6() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "m-0", Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: "fd2e:6f44:5dd8:c956::16",
			},
		}},
	}
}

func wellKnownMasterMachine() *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m-0", Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status: machinev1beta1.MachineStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: "10.0.139.78",
			},
		}},
	}
}

func wellKnownMasterMachineIpv6() *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m-0", Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status: machinev1beta1.MachineStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: "fd2e:6f44:5dd8:c956::16",
			},
		}},
	}
}

type fakeMachineAPI struct {
	isMachineAPIFunctional func() (bool, error)
}

func (dm *fakeMachineAPI) IsFunctional() (bool, error) {
	return dm.isMachineAPIFunctional()
}

func (dm *fakeMachineAPI) IsEnabled() (bool, error) {
	return true, nil
}

func (dm *fakeMachineAPI) IsAvailable() (bool, error) {
	return true, nil
}

func machineFor(name, internalIP string) *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status: machinev1beta1.MachineStatus{Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeInternalIP,
				Address: internalIP,
			},
		}},
	}
}

func machineWithHooksFor(name, internalIP string) *machinev1beta1.Machine {
	m := machineFor(name, internalIP)
	m.Spec.LifecycleHooks.PreDrain = append(m.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: "EtcdQuorumOperator", Owner: "clusteroperator/etcd"})
	return m
}

func wellKnownMasterMachines() []runtime.Object {
	return []runtime.Object{
		machineWithHooksFor("m-1", "10.0.139.78"),
		machineWithHooksFor("m-2", "10.0.139.79"),
		machineWithHooksFor("m-3", "10.0.139.80"),
	}
}

// machinePendingDeletionFor builds a master machine with the etcd deletion hook and a set
// DeletionTimestamp, i.e. a tombstone whose finalization is blocked by the PreDrain hook until
// the etcd member leaves the cluster.
func machinePendingDeletionFor(name, internalIP string) *machinev1beta1.Machine {
	m := machineWithHooksFor(name, internalIP)
	m.DeletionTimestamp = &metav1.Time{}
	return m
}

func TestIsEtcdEndpointsUpdated(t *testing.T) {
	scenarios := []struct {
		name                                     string
		initialObjectsForConfigMapTargetNSLister []runtime.Object
		initialEtcdMemberList                    []*etcdserverpb.Member
		expectedResult                           bool
		expectError                              bool
		expectedErrorMsg                         string
	}{
		{
			name: "live membership equals configmap membership",
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
					Data: map[string]string{
						"m-1": "10.0.139.78",
						"m-2": "10.0.139.79",
						"m-3": "10.0.139.80",
					},
				},
			},
			initialEtcdMemberList: []*etcdserverpb.Member{
				{Name: "m-1", ID: 1, PeerURLs: []string{"https://10.0.139.78:1234"}},
				{Name: "m-2", ID: 2, PeerURLs: []string{"https://10.0.139.79:1234"}},
				{Name: "m-3", ID: 3, PeerURLs: []string{"https://10.0.139.80:1234"}},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name: "live membership has more members than configmap (scaling up)",
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
					Data: map[string]string{
						"m-1": "10.0.139.78",
						"m-2": "10.0.139.79",
						"m-3": "10.0.139.80",
					},
				},
			},
			initialEtcdMemberList: []*etcdserverpb.Member{
				{Name: "m-1", ID: 1, PeerURLs: []string{"https://10.0.139.78:1234"}},
				{Name: "m-2", ID: 2, PeerURLs: []string{"https://10.0.139.79:1234"}},
				{Name: "m-3", ID: 3, PeerURLs: []string{"https://10.0.139.80:1234"}},
				{Name: "m-4", ID: 4, PeerURLs: []string{"https://10.0.139.81:1234"}},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name: "live membership has fewer members than configmap (scaling down)",
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
					Data: map[string]string{
						"m-1": "10.0.139.78",
						"m-2": "10.0.139.79",
						"m-3": "10.0.139.80",
						"m-4": "10.0.139.81",
					},
				},
			},
			initialEtcdMemberList: []*etcdserverpb.Member{
				{Name: "m-1", ID: 1, PeerURLs: []string{"https://10.0.139.78:1234"}},
				{Name: "m-2", ID: 2, PeerURLs: []string{"https://10.0.139.79:1234"}},
				{Name: "m-3", ID: 3, PeerURLs: []string{"https://10.0.139.80:1234"}},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name: "live membership differs from configmap (different IPs)",
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
					Data: map[string]string{
						"m-1": "10.0.139.78",
						"m-2": "10.0.139.79",
						"m-3": "10.0.139.80",
					},
				},
			},
			initialEtcdMemberList: []*etcdserverpb.Member{
				{Name: "m-1", ID: 1, PeerURLs: []string{"https://10.0.139.78:1234"}},
				{Name: "m-2", ID: 2, PeerURLs: []string{"https://10.0.139.79:1234"}},
				{Name: "m-4", ID: 4, PeerURLs: []string{"https://10.0.139.81:1234"}},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                                     "configmap not found",
			initialObjectsForConfigMapTargetNSLister: []runtime.Object{},
			initialEtcdMemberList: []*etcdserverpb.Member{
				{Name: "m-1", ID: 1, PeerURLs: []string{"https://10.0.139.78:1234"}},
			},
			expectedResult:   false,
			expectError:      true,
			expectedErrorMsg: "failed to get etcd-endpoints configmap",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// setup test data
			configMapTargetNSIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForConfigMapTargetNSLister {
				configMapTargetNSIndexer.Add(initialObj)
			}
			configMapTargetNSLister := corev1listers.NewConfigMapLister(configMapTargetNSIndexer).ConfigMaps("openshift-etcd")

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
			require.NoError(t, err)

			// create controller instance
			target := clusterMemberRemovalController{
				etcdClient:                        fakeEtcdClient,
				configMapListerForTargetNamespace: configMapTargetNSLister,
			}

			// act
			result, err := target.isEtcdEndpointsUpdated(context.TODO())

			// assert
			if scenario.expectError {
				require.Error(t, err)
				if scenario.expectedErrorMsg != "" {
					require.Contains(t, err.Error(), scenario.expectedErrorMsg)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, scenario.expectedResult, result, "isEtcdEndpointsUpdated returned unexpected result")
			}
		})
	}
}

var wellKnownReplicasCountSet = `
{
 "controlPlane": {"replicas": 3}
}
`
