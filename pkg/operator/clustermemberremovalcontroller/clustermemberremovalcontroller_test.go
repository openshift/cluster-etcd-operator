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
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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
		validateFn                       func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
		serviceNetwork                   string
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
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
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
			err = target.removeMemberWithoutMachine(context.TODO())
			if err != nil {
				t.Fatal(err)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient)
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
				require.NoError(t, configMapTargetNSIndexer.Add(initialObj))
				snapshot := initialObj.(*corev1.ConfigMap).DeepCopy()
				snapshot.Name = "etcd-endpoints-24"
				require.NoError(t, configMapTargetNSIndexer.Add(snapshot))
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
			}, u.StaticPodOperatorStatus(u.WithLatestRevision(24), u.WithNodeStatusAtCurrentRevision(24)), nil, nil)

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

func TestSyncWaitsForEndpointRevision(t *testing.T) {
	ctx := context.Background()
	configMaps := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, configMaps.Add(bootstrapComplete.DeepCopy()))
	machines := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	nodes := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	network := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, network.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: configv1.NetworkStatus{ServiceNetwork: []string{"172.30.0.0/16"}}}))
	infra := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, infra.Add(&configv1.Infrastructure{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Status: configv1.InfrastructureStatus{ControlPlaneTopology: configv1.HighlyAvailableTopologyMode}}))
	status := &operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 24}}
	endpoints := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: operatorclient.TargetNamespace}, Data: map[string]string{}}
	var members []*etcdserverpb.Member
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("master-%d", i)
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		machine := machineWithHooksFor(name, ip)
		machine.Status.NodeRef = &corev1.ObjectReference{Name: name}
		if i < 3 {
			deletionTime := metav1.Now()
			machine.DeletionTimestamp = &deletionTime
		}
		require.NoError(t, machines.Add(machine))
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"node-role.kubernetes.io/master": ""}}, Status: corev1.NodeStatus{Addresses: machine.Status.Addresses, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		require.NoError(t, nodes.Add(node))
		status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{NodeName: name, CurrentRevision: 24})
		members = append(members, &etcdserverpb.Member{Name: name, ID: uint64(i + 1), PeerURLs: []string{fmt.Sprintf("https://%s:2380", ip)}})
		endpoints.Data[fmt.Sprintf("%x", i+1)] = ip
	}
	require.NoError(t, configMaps.Add(endpoints))
	snapshot := endpoints.DeepCopy()
	snapshot.Name = "etcd-endpoints-24"
	require.NoError(t, configMaps.Add(snapshot))
	etcdClient, err := etcdcli.NewFakeEtcdClient(members, etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 6}))
	require.NoError(t, err)
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ObservedConfig: runtime.RawExtension{Raw: []byte(wellKnownReplicasCountSet)}}}, status, nil, nil)
	target := &clusterMemberRemovalController{
		operatorClient:                    operatorClient,
		etcdClient:                        etcdClient,
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
	syncContext := factory.NewSyncContext("test", events.NewInMemoryRecorder("test", clock.RealClock{}))
	syncAndExpectMembers := func(want int, message string) {
		t.Helper()
		require.NoError(t, target.sync(ctx, syncContext))
		currentMembers, err := etcdClient.MemberList(ctx)
		require.NoError(t, err)
		require.Len(t, currentMembers, want, message)
		t.Logf("%s: %d members", message, len(currentMembers))
	}
	syncAndExpectMembers(5, "the first removal must proceed on a stable revision")
	liveIPs, err := ceohelpers.VotingMemberIPListSet(ctx, etcdClient)
	require.NoError(t, err)
	removedNodeName := ""
	for _, member := range members {
		ip, err := ceohelpers.MemberToNodeInternalIP(member)
		require.NoError(t, err)
		if !liveIPs.Has(ip) {
			removedNodeName = member.Name
		}
	}
	require.NotEmpty(t, removedNodeName)
	endpoints = endpoints.DeepCopy()
	for id, ip := range endpoints.Data {
		if !liveIPs.Has(ip) {
			delete(endpoints.Data, id)
		}
	}
	require.NoError(t, configMaps.Update(endpoints))
	syncAndExpectMembers(5, "mutable endpoints catching up must not allow a second removal before the revision is published")
	snapshot = endpoints.DeepCopy()
	snapshot.Name = "etcd-endpoints-25"
	require.NoError(t, configMaps.Add(snapshot))
	_, _, err = v1helpers.UpdateStaticPodStatus(ctx, operatorClient, func(status *operatorv1.StaticPodOperatorStatus) error {
		status.LatestAvailableRevision = 25
		return nil
	})
	require.NoError(t, err)
	syncAndExpectMembers(5, "publishing the endpoint revision must not bypass rollout")
	_, _, err = v1helpers.UpdateStaticPodStatus(ctx, operatorClient, func(status *operatorv1.StaticPodOperatorStatus) error {
		for i := range status.NodeStatuses {
			if status.NodeStatuses[i].NodeName != removedNodeName {
				status.NodeStatuses[i].CurrentRevision = 25
			}
		}
		return nil
	})
	require.NoError(t, err)
	syncAndExpectMembers(5, "the removed member node still holds the revision rollout until deletion")
	removedNode, exists, err := nodes.GetByKey(removedNodeName)
	require.NoError(t, err)
	require.True(t, exists)
	require.NoError(t, nodes.Delete(removedNode))
	removedMachine, exists, err := machines.GetByKey(removedNodeName)
	require.NoError(t, err)
	require.True(t, exists)
	require.NoError(t, machines.Delete(removedMachine))
	_, _, err = v1helpers.UpdateStaticPodStatus(ctx, operatorClient, func(status *operatorv1.StaticPodOperatorStatus) error {
		var remaining []operatorv1.NodeStatus
		for _, nodeStatus := range status.NodeStatuses {
			if nodeStatus.NodeName != removedNodeName {
				remaining = append(remaining, nodeStatus)
			}
		}
		status.NodeStatuses = remaining
		return nil
	})
	require.NoError(t, err)
	syncAndExpectMembers(4, "the next removal may proceed after node deletion and endpoint revision rollout")
}

func TestIsEtcdEndpointRevisionUpdated(t *testing.T) {
	for _, tc := range []struct {
		name            string
		snapshotIPs     map[string]string
		missingSnapshot bool
		nodeStatuses    []operatorv1.NodeStatus
		ipv6            bool
		want            bool
		wantError       string
	}{
		{name: "stable snapshot matches", snapshotIPs: map[string]string{"member": "10.0.139.78"}, nodeStatuses: []operatorv1.NodeStatus{{CurrentRevision: 24}}, want: true},
		{name: "snapshot still includes removed member", snapshotIPs: map[string]string{"member": "10.0.139.78", "removed": "10.0.139.79"}, nodeStatuses: []operatorv1.NodeStatus{{CurrentRevision: 24}}},
		{name: "snapshot missing", missingSnapshot: true, nodeStatuses: []operatorv1.NodeStatus{{CurrentRevision: 24}}, wantError: "failed to get etcd-endpoints-24 configmap"},
		{name: "revision rollout pending", snapshotIPs: map[string]string{"member": "10.0.139.78"}, nodeStatuses: []operatorv1.NodeStatus{{CurrentRevision: 23, TargetRevision: 24}}, wantError: "revision rollout in progress"},
		{name: "target revision advances before latest available", snapshotIPs: map[string]string{"member": "10.0.139.78"}, nodeStatuses: []operatorv1.NodeStatus{{CurrentRevision: 24, TargetRevision: 25}}, wantError: "revision rollout in progress"},
		{name: "no node status", wantError: "no node status"},
		{name: "IPv6 voting membership ignores learner", ipv6: true, snapshotIPs: map[string]string{"member": "fd2e:6f44:5dd8:c956::16"}, nodeStatuses: []operatorv1.NodeStatus{{CurrentRevision: 24}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := "10.0.139.78"
			peerURL := "https://10.0.139.78:2380"
			if tc.ipv6 {
				ip = "fd2e:6f44:5dd8:c956::16"
				peerURL = "https://[fd2e:6f44:5dd8:c956::16]:2380"
			}
			members := []*etcdserverpb.Member{{Name: "member", ID: 1, PeerURLs: []string{peerURL}}, {Name: "learner", ID: 2, IsLearner: true, PeerURLs: []string{"https://10.0.139.80:2380"}}}
			etcdClient, err := etcdcli.NewFakeEtcdClient(members)
			require.NoError(t, err)
			configMaps := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, configMaps.Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: operatorclient.TargetNamespace}, Data: map[string]string{"member": ip}}))
			if !tc.missingSnapshot {
				require.NoError(t, configMaps.Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints-24", Namespace: operatorclient.TargetNamespace}, Data: tc.snapshotIPs}))
			}
			target := &clusterMemberRemovalController{
				operatorClient:                    v1helpers.NewFakeStaticPodOperatorClient(nil, &operatorv1.StaticPodOperatorStatus{OperatorStatus: operatorv1.OperatorStatus{LatestAvailableRevision: 24}, NodeStatuses: tc.nodeStatuses}, nil, nil),
				etcdClient:                        etcdClient,
				configMapListerForTargetNamespace: corev1listers.NewConfigMapLister(configMaps).ConfigMaps(operatorclient.TargetNamespace),
			}
			updated, err := target.isEtcdEndpointRevisionUpdated(context.Background())
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, updated)
		})
	}
}
