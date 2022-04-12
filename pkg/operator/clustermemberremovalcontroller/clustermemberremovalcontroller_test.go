package clustermemberremovalcontroller

import (
	"context"
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

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
	}{
		// scenario 1
		{
			name:                             "happy path: an etcd member has a corresponding machine and node resources",
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

		// scenario 2
		{
			name:                             "an etcd member doesn't have a corresponding machine nor node resource and it is removed",
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

		// scenario 3
		{
			name:                             "an etcd member with only a corresponding machine resource is not removed",
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

		// scenario 4
		{
			name:                             "an etcd member with only a corresponding node resource is not removed",
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
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			eventRecorder := events.NewRecorder(k8sfakeclient.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-member-removal-controller", &corev1.ObjectReference{})
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
			networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{"172.30.0.0/16"}}})
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
			err = target.sync(context.TODO(), factory.NewSyncContext("test-cluster-member-removal-controller", eventRecorder))
			if err != nil {
				t.Fatal(err)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient)
			}
		})
	}
}

func wellKnownEtcdEndpointsConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd-endpoints", Namespace: "openshift-etcd"},
		Data: map[string]string{
			"m-0": "10.0.139.78",
		},
	}
}

func wellKnownEtcdMemberList() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{
			Name:     "m-0",
			ID:       8,
			PeerURLs: []string{"https://10.0.139.78:1234"},
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

type fakeMachineAPI struct {
	isMachineAPIFunctional func() (bool, error)
}

func (dm *fakeMachineAPI) IsFunctional() (bool, error) {
	return dm.isMachineAPIFunctional()
}
