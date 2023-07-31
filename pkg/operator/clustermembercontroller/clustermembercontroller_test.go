package clustermembercontroller

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/events"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

type fakePodLister struct {
	client    kubernetes.Interface
	namespace string
}

func (f *fakePodLister) List(selector labels.Selector) (ret []*corev1.Pod, err error) {
	pods, err := f.client.CoreV1().Pods(f.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	ret = []*corev1.Pod{}
	for i := range pods.Items {
		ret = append(ret, &pods.Items[i])
	}
	return ret, nil
}

func (f *fakePodLister) Pods(namespace string) corev1listers.PodNamespaceLister {
	return &fakePodNamespacedLister{
		client:    f.client,
		namespace: f.namespace,
	}
}

type fakePodNamespacedLister struct {
	client    kubernetes.Interface
	namespace string
}

func (f *fakePodNamespacedLister) List(selector labels.Selector) (ret []*corev1.Pod, err error) {
	pods, err := f.client.CoreV1().Pods(f.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	ret = []*corev1.Pod{}
	for i := range pods.Items {
		ret = append(ret, &pods.Items[i])
	}
	return ret, nil
}

func (f *fakePodNamespacedLister) Get(name string) (*corev1.Pod, error) {
	pod, err := f.client.CoreV1().Pods(f.namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pod, nil
}

func TestGetEtcdPeerURLToAdd(t *testing.T) {
	alwaysTrueIsFunctionalMachineAPIFn := func() (bool, error) { return true, nil }
	containerRunning := &corev1.ContainerStateRunning{}
	scenarios := []struct {
		name                             string
		isFunctionalMachineAPIFn         func() (bool, error)
		initialEtcdMemberList            []*etcdserverpb.Member
		initialObjectsForMachineLister   []runtime.Object
		initialObjectsForPodLister       []runtime.Object
		initialObjectsForConfigmapLister []runtime.Object
		initialObjectsForNodeLister      []runtime.Object
		podLister                        corev1listers.PodLister
		expectedPeerURL                  string
		expectedError                    error
		validateFn                       func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
		serviceNetwork                   string
	}{
		{
			name:                     "No new node,machine,pod/no member added",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList: []*etcdserverpb.Member{
				etcdMemberFor(0, "m-0"),
			},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, true),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				u.EndpointsConfigMap(
					withEndpoint(0, "10.0.0.0"),
				),
			},
			expectedPeerURL: "",
			expectedError:   nil,
		},
		{
			name:                     "Node and machine with running etcd pod/new member peerURL returned",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				// Unready etcd container
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedPeerURL: "https://10.0.0.0:2380",
			expectedError:   nil,
		},
		{
			name:                           "Non-functional machine API/node with running etcd pod/new member peerURL returned",
			serviceNetwork:                 "172.30.0.0/16",
			isFunctionalMachineAPIFn:       func() (bool, error) { return false, nil },
			initialEtcdMemberList:          []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				// Unready etcd container
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedPeerURL: "https://10.0.0.0:2380",
			expectedError:   nil,
		},
		{
			name:                     "Node and machine without missing etcd pod/no peerURL returned",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				// No etcd pod running
				etcdPodFor("m-0", nil, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedPeerURL: "",
			expectedError:   nil,
		},
		{
			name:                     "Node and machine without running etcd container/no peerURL returned",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				// Not running etcd container
				etcdPodFor("m-0", nil, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedPeerURL: "",
			expectedError:   nil,
		},
		{
			name:                     "Node and running etcd pod without machine deletion hook/no peerURL returned",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				// No deletion hook
				machineFor("m-0", "10.0.0.0", false, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				// Running etcd container
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedPeerURL: "",
			expectedError:   nil,
		},

		{
			name:                     "Node and running etcd pod with machine pending deletion/no peerURL returned",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				// has deletion timestamp
				machineFor("m-0", "10.0.0.0", false, true),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				// Running etcd container
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedPeerURL: "",
			expectedError:   nil,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: scenario.isFunctionalMachineAPIFn}

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}
			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{scenario.serviceNetwork}}})
			networkLister := configv1listers.NewNetworkLister(networkIndexer)
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForNodeLister {
				nodeIndexer.Add(initialObj)
			}
			nodeLister := corev1listers.NewNodeLister(nodeIndexer)
			nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
			if err != nil {
				t.Fatal(err)
			}

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
			if err != nil {
				t.Fatal(err)
			}

			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.initialObjectsForConfigmapLister {
				if err := configMapIndexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("openshift-etcd")

			// act
			target := ClusterMemberController{
				etcdClient:            fakeEtcdClient,
				podLister:             &fakePodLister{fake.NewSimpleClientset(scenario.initialObjectsForPodLister...), "openshift-etcd"},
				masterMachineLister:   machineLister,
				masterMachineSelector: machineSelector,
				networkLister:         networkLister,
				masterNodeLister:      nodeLister,
				masterNodeSelector:    nodeSelector,
				machineAPIChecker:     fakeMachineAPIChecker,
				configMapLister:       configMapLister,
			}
			peerURL, err := target.getEtcdPeerURLToAdd(context.TODO())
			if err == nil && scenario.expectedError != nil {
				t.Fatalf("got no error, expected to get an error: (%v)", scenario.expectedError)
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if peerURL != scenario.expectedPeerURL {
				t.Fatalf("unexpected peerURL returned = %v, expected = %v", peerURL, scenario.expectedPeerURL)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient)
			}
		})
	}
}

func TestEnsureEtcdLearnerPromotion(t *testing.T) {
	alwaysTrueIsFunctionalMachineAPIFn := func() (bool, error) { return true, nil }
	containerRunning := &corev1.ContainerStateRunning{}
	scenarios := []struct {
		name                             string
		isFunctionalMachineAPIFn         func() (bool, error)
		initialEtcdMemberList            []*etcdserverpb.Member
		initialObjectsForMachineLister   []runtime.Object
		initialObjectsForPodLister       []runtime.Object
		initialObjectsForConfigmapLister []runtime.Object
		initialObjectsForNodeLister      []runtime.Object
		podLister                        corev1listers.PodLister
		expectedPeerURL                  string
		expectedError                    error
		validateFn                       func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient, numExpectedMembers int)
		serviceNetwork                   string
		numExpectedMembers               int
	}{
		{
			name:                     "No learner member/No promotion",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList: []*etcdserverpb.Member{
				etcdMemberFor(0, "m-0"),
			},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, true),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				u.EndpointsConfigMap(
					withEndpoint(0, "10.0.0.0"),
				),
			},
			expectedPeerURL:    "",
			expectedError:      nil,
			numExpectedMembers: 1,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient, numExpectedMembers int) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != numExpectedMembers {
					t.Errorf("expected exactly %v member, got %v", numExpectedMembers, len(memberList))
				}
			},
		},
		{
			name:                     "Learner member present with deletion hook/Promoted",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList: []*etcdserverpb.Member{
				etcdMemberFor(0, "m-0"),
				etcdLearnerMemberFor(1, "m-1"),
			},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
				machineFor("m-1", "10.0.0.1", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
				nodeFor("m-1", "10.0.0.1"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, true),
				etcdPodFor("m-1", containerRunning, true),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				u.EndpointsConfigMap(
					// Learner members not part of configmap
					withEndpoint(0, "10.0.0.0"),
				),
			},
			expectedPeerURL:    "",
			expectedError:      nil,
			numExpectedMembers: 2,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient, numExpectedMembers int) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				numVotingMembers := 0
				for _, m := range memberList {
					if !m.IsLearner {
						numVotingMembers++
					}
				}
				if numVotingMembers != numExpectedMembers {
					t.Errorf("expected exactly %v voting members, got %v", numExpectedMembers, numVotingMembers)
				}
			},
		},
		{
			name:                     "Learner member present with non-functional machine API/Promoted",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: func() (bool, error) { return false, nil },
			initialEtcdMemberList: []*etcdserverpb.Member{
				etcdMemberFor(0, "m-0"),
				etcdLearnerMemberFor(1, "m-1"),
			},
			initialObjectsForMachineLister: []runtime.Object{},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
				nodeFor("m-1", "10.0.0.1"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, true),
				etcdPodFor("m-1", containerRunning, true),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				u.EndpointsConfigMap(
					// Learner members not part of configmap
					withEndpoint(0, "10.0.0.0"),
				),
			},
			expectedPeerURL:    "",
			expectedError:      nil,
			numExpectedMembers: 2,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient, numExpectedMembers int) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				numVotingMembers := 0
				for _, m := range memberList {
					if !m.IsLearner {
						numVotingMembers++
					}
				}
				if numVotingMembers != numExpectedMembers {
					t.Errorf("expected exactly %v voting members, got %v", numExpectedMembers, numVotingMembers)
				}
			},
		},
		{
			name:                     "Learner member present without deletion hook/Not promoted",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList: []*etcdserverpb.Member{
				etcdMemberFor(0, "m-0"),
				etcdLearnerMemberFor(1, "m-1"),
			},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
				// Learning member machine without deletion hook
				machineFor("m-1", "10.0.0.1", false, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
				nodeFor("m-1", "10.0.0.1"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, true),
				etcdPodFor("m-1", containerRunning, true),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				u.EndpointsConfigMap(
					// Learner members not part of configmap
					withEndpoint(0, "10.0.0.0"),
				),
			},
			expectedPeerURL:    "",
			expectedError:      nil,
			numExpectedMembers: 1,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient, numExpectedMembers int) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				numVotingMembers := 0
				for _, m := range memberList {
					if !m.IsLearner {
						numVotingMembers++
					}
				}
				if numVotingMembers != numExpectedMembers {
					t.Errorf("expected exactly %v voting members, got %v", numExpectedMembers, numVotingMembers)
				}
			},
		},

		{
			name:                     "Learner member present with machine pending deletion/Not promoted",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList: []*etcdserverpb.Member{
				etcdMemberFor(0, "m-0"),
				etcdLearnerMemberFor(1, "m-1"),
			},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
				// Learning member machine pending deletion
				machineFor("m-1", "10.0.0.1", true, true),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
				nodeFor("m-1", "10.0.0.1"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, true),
				etcdPodFor("m-1", containerRunning, true),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				u.EndpointsConfigMap(
					// Learner members not part of configmap
					withEndpoint(0, "10.0.0.0"),
				),
			},
			expectedPeerURL:    "",
			expectedError:      nil,
			numExpectedMembers: 1,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient, numExpectedMembers int) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				numVotingMembers := 0
				for _, m := range memberList {
					if !m.IsLearner {
						numVotingMembers++
					}
				}
				if numVotingMembers != numExpectedMembers {
					t.Errorf("expected exactly %v voting members, got %v", numExpectedMembers, numVotingMembers)
				}
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: scenario.isFunctionalMachineAPIFn}
			eventRecorder := events.NewRecorder(fake.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-member-controller", &corev1.ObjectReference{})
			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}
			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{scenario.serviceNetwork}}})
			networkLister := configv1listers.NewNetworkLister(networkIndexer)
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForNodeLister {
				nodeIndexer.Add(initialObj)
			}
			nodeLister := corev1listers.NewNodeLister(nodeIndexer)
			nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
			if err != nil {
				t.Fatal(err)
			}

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
			if err != nil {
				t.Fatal(err)
			}

			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.initialObjectsForConfigmapLister {
				if err := configMapIndexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("openshift-etcd")

			// act
			target := ClusterMemberController{
				etcdClient:            fakeEtcdClient,
				podLister:             &fakePodLister{fake.NewSimpleClientset(scenario.initialObjectsForPodLister...), "openshift-etcd"},
				masterMachineLister:   machineLister,
				masterMachineSelector: machineSelector,
				networkLister:         networkLister,
				masterNodeLister:      nodeLister,
				masterNodeSelector:    nodeSelector,
				machineAPIChecker:     fakeMachineAPIChecker,
				configMapLister:       configMapLister,
			}
			err = target.ensureEtcdLearnerPromotion(context.TODO(), eventRecorder)
			if err == nil && scenario.expectedError != nil {
				t.Fatalf("got no error, expected to get an error: (%v)", scenario.expectedError)
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if scenario.validateFn != nil {
				scenario.validateFn(t, fakeEtcdClient, scenario.numExpectedMembers)
			}
		})
	}
}

func TestReconcileMembers(t *testing.T) {
	alwaysTrueIsFunctionalMachineAPIFn := func() (bool, error) { return true, nil }
	containerRunning := &corev1.ContainerStateRunning{}

	scenarios := []struct {
		name                             string
		isFunctionalMachineAPIFn         func() (bool, error)
		initialEtcdMemberList            []*etcdserverpb.Member
		initialObjectsForMachineLister   []runtime.Object
		initialObjectsForPodLister       []runtime.Object
		initialObjectsForConfigmapLister []runtime.Object
		initialObjectsForNodeLister      []runtime.Object
		podLister                        corev1listers.PodLister
		expectedError                    error
		validateFn                       func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
		serviceNetwork                   string
	}{
		{
			name:                     "member is added, pod not ready, machine not pending deletion",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly 1 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                     "ipv6 member is added, pod not ready, machine not pending deletion",
			serviceNetwork:           "fd02::/112",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:    []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "fd2e:6f44:5dd8:c956::16", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "fd2e:6f44:5dd8:c956::16"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly 1 members, got %v", len(memberList))
				}
				if len(memberList[0].PeerURLs) != 1 {
					t.Errorf("expected exactly 1 PeerURLs, got %v", len(memberList[0].PeerURLs))
				}
				expectedPeerURL := "https://[fd2e:6f44:5dd8:c956::16]:2380"
				if memberList[0].PeerURLs[0] != expectedPeerURL {
					t.Errorf("expected PeerURL %s got %s", expectedPeerURL, memberList[0].PeerURLs)
				}
			},
		},
		{
			name:                     "member not added, machine pending deletion",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,

			initialEtcdMemberList: []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				// Machine pending deletion
				machineFor("m-0", "10.0.0.0", true, true),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 0 {
					t.Errorf("expected exactly 0 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                     "member is added, pod not ready, machine pending deletion, machine api off",
			serviceNetwork:           "172.30.0.0/16",
			isFunctionalMachineAPIFn: func() (bool, error) { return false, nil },

			initialEtcdMemberList: []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				machineFor("m-0", "10.0.0.0", true, false),
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 1 {
					t.Errorf("expected exactly 1 members, got %v", len(memberList))
				}
			},
		},
		{
			name:                           "member not added, machine not found",
			serviceNetwork:                 "172.30.0.0/16",
			isFunctionalMachineAPIFn:       alwaysTrueIsFunctionalMachineAPIFn,
			initialEtcdMemberList:          []*etcdserverpb.Member{},
			initialObjectsForMachineLister: []runtime.Object{
				// No machine present for node
			},
			initialObjectsForNodeLister: []runtime.Object{
				nodeFor("m-0", "10.0.0.0"),
			},
			initialObjectsForPodLister: []runtime.Object{
				etcdPodFor("m-0", containerRunning, false),
			},
			initialObjectsForConfigmapLister: []runtime.Object{
				// No voting member present
				u.EndpointsConfigMap(),
			},
			expectedError: nil,
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 0 {
					t.Errorf("expected exactly 0 members, got %v", len(memberList))
				}
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			eventRecorder := events.NewRecorder(fake.NewSimpleClientset().CoreV1().Events("operator"), "test-cluster-member-controller", &corev1.ObjectReference{})
			fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: scenario.isFunctionalMachineAPIFn}

			machineIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForMachineLister {
				machineIndexer.Add(initialObj)
			}
			machineLister := machinelistersv1beta1.NewMachineLister(machineIndexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}
			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{scenario.serviceNetwork}}})
			networkLister := configv1listers.NewNetworkLister(networkIndexer)
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForNodeLister {
				nodeIndexer.Add(initialObj)
			}
			nodeLister := corev1listers.NewNodeLister(nodeIndexer)
			nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
			if err != nil {
				t.Fatal(err)
			}

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
			if err != nil {
				t.Fatal(err)
			}

			configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range scenario.initialObjectsForConfigmapLister {
				if err := configMapIndexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			configMapLister := corev1listers.NewConfigMapLister(configMapIndexer).ConfigMaps("openshift-etcd")

			// act
			target := ClusterMemberController{
				etcdClient:            fakeEtcdClient,
				podLister:             &fakePodLister{fake.NewSimpleClientset(scenario.initialObjectsForPodLister...), "openshift-etcd"},
				machineAPIChecker:     fakeMachineAPIChecker,
				masterMachineLister:   machineLister,
				masterMachineSelector: machineSelector,
				networkLister:         networkLister,
				masterNodeLister:      nodeLister,
				masterNodeSelector:    nodeSelector,
				configMapLister:       configMapLister,
			}
			err = target.reconcileMembers(context.TODO(), eventRecorder)
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from reconcileMembers() method")
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

func machineFor(name, internalIP string, hasDeletionHook bool, hasDeletionTimestamp bool) *machinev1beta1.Machine {
	phaseRunning := "Running"
	m := &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"}},
		Status: machinev1beta1.MachineStatus{
			Phase: &phaseRunning,
			Addresses: []corev1.NodeAddress{
				{
					Type:    corev1.NodeInternalIP,
					Address: internalIP,
				},
			}},
	}
	if hasDeletionHook {
		addPreDrainHook(m)
	}
	if hasDeletionTimestamp {
		m.DeletionTimestamp = &metav1.Time{}
	}
	return m
}

func nodeFor(name, internalIP string) *corev1.Node {
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

func etcdMemberFor(id uint64, name string) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:     name,
		PeerURLs: []string{fmt.Sprintf("https://10.0.0.%d:2380", id)},
		ID:       id,
	}
}

func etcdLearnerMemberFor(id uint64, name string) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:      name,
		PeerURLs:  []string{fmt.Sprintf("https://10.0.0.%d:2380", id)},
		ID:        id,
		IsLearner: true,
	}
}

func etcdPodFor(nodeName string, containerStateRunning *corev1.ContainerStateRunning, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("etcd-%v", nodeName),
			Namespace: "openshift-etcd",
			Labels:    labels.Set{"app": "etcd"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "etcd",
					Ready: ready,
					State: corev1.ContainerState{
						Running: containerStateRunning,
					},
				},
			},
		},
	}
}

func addPreDrainHook(machine *machinev1beta1.Machine) {
	machine.Spec.LifecycleHooks.PreDrain = append(machine.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: ceohelpers.MachineDeletionHookName, Owner: ceohelpers.MachineDeletionHookOwner})
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

func withEndpoint(memberID uint64, ip string) func(*corev1.ConfigMap) {
	return func(endpoints *corev1.ConfigMap) {
		endpoints.Data[fmt.Sprintf("%016x", memberID)] = ip
	}
}
