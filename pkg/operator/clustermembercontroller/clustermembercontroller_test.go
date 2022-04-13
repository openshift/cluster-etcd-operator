package clustermembercontroller

import (
	"context"
	"reflect"
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/events"
	corev1listers "k8s.io/client-go/listers/core/v1"
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

func (f *fakePodLister) Pods(namespace string) corev1lister.PodNamespaceLister {
	panic("implement me")
}

func TestClusterMemberController_getEtcdPodToAddToMembership(t *testing.T) {
	type fields struct {
		podLister corev1lister.PodLister
	}
	tests := []struct {
		name    string
		fields  fields
		want    *corev1.Pod
		wantErr bool
	}{
		{
			name: "test pods with init container failed",
			fields: fields{
				podLister: &fakePodLister{fake.NewSimpleClientset(&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						// this will be skipped
						Name:      "etcd-a",
						Namespace: "openshift-etcd",
						Labels:    labels.Set{"app": "etcd"},
					},
					Status: corev1.PodStatus{
						Phase: "Running",
						InitContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "etcd-ensure-env",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										ExitCode: 0,
									},
								},
							},
							{
								Name: "etcd-resources-copy",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										ExitCode: 0,
									},
								},
							},
						},
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "etcd",
								Ready: true,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "etcd-b",
							Namespace: "openshift-etcd",
							Labels:    labels.Set{"app": "etcd"},
						},
						Spec: corev1.PodSpec{
							NodeName: "node-b",
						},
						Status: corev1.PodStatus{
							Phase: "Running",
							InitContainerStatuses: []corev1.ContainerStatus{
								{
									Name: "etcd-ensure-env",
									State: corev1.ContainerState{
										Terminated: &corev1.ContainerStateTerminated{
											ExitCode: 1,
										},
									},
								},
								{
									Name: "etcd-resources-copy",
									State: corev1.ContainerState{
										Terminated: nil,
									},
								},
							},
							ContainerStatuses: []corev1.ContainerStatus{
								{
									Name:  "etcd",
									Ready: true,
									State: corev1.ContainerState{
										Waiting: &corev1.ContainerStateWaiting{
											Reason: "WaitingOnInit",
										},
										Running:    nil,
										Terminated: nil,
									},
								},
							},
						},
					}), "openshift-etcd"},
			},
			want: nil,
		},
		{
			name: "test pods with no container state set",
			fields: fields{
				podLister: &fakePodLister{fake.NewSimpleClientset(&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						// this will be skipped
						Name:      "etcd-a",
						Namespace: "openshift-etcd",
						Labels:    labels.Set{"app": "etcd"},
					},
					Status: corev1.PodStatus{
						Phase: "Running",
						InitContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "etcd-ensure-env",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										ExitCode: 0,
									},
								},
							},
							{
								Name: "etcd-resources-copy",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										ExitCode: 0,
									},
								},
							},
						},
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "etcd",
							},
						},
					},
				},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "etcd-b",
							Namespace: "openshift-etcd",
							Labels:    labels.Set{"app": "etcd"},
						},
						Spec: corev1.PodSpec{
							NodeName: "node-b",
						},
						Status: corev1.PodStatus{
							Phase: "Running",
							InitContainerStatuses: []corev1.ContainerStatus{
								{
									Name: "etcd-ensure-env",
									State: corev1.ContainerState{
										Terminated: &corev1.ContainerStateTerminated{
											ExitCode: 1,
										},
									},
								},
								{
									Name: "etcd-resources-copy",
									State: corev1.ContainerState{
										Terminated: nil,
									},
								},
							},
							ContainerStatuses: []corev1.ContainerStatus{
								{
									Name: "etcd",
								},
							},
						},
					}), "openshift-etcd"},
			},
			want: nil,
		},
		{
			name: "test pods with no status",
			fields: fields{
				podLister: &fakePodLister{fake.NewSimpleClientset(&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						// this will be skipped
						Name:      "etcd-a",
						Namespace: "openshift-etcd",
						Labels:    labels.Set{"app": "etcd"},
					},
				},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "etcd-b",
							Namespace: "openshift-etcd",
							Labels:    labels.Set{"app": "etcd"},
						},
						Spec: corev1.PodSpec{
							NodeName: "node-b",
						},
					}), "openshift-etcd"},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient([]*etcdserverpb.Member{
				{
					Name: "etcd-a",
				},
			})
			c := &ClusterMemberController{
				etcdClient: fakeEtcdClient,
				podLister:  tt.fields.podLister,
			}
			got, err := c.getEtcdPodToAddToMembership(context.TODO())
			if (err != nil) != tt.wantErr {
				t.Errorf("getEtcdPodToAddToMembership() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getEtcdPodToAddToMembership() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReconcileMembers(t *testing.T) {
	alwaysTrueIsFunctionalMachineAPIFn := func() (bool, error) { return true, nil }

	scenarios := []struct {
		name                           string
		isFunctionalMachineAPIFn       func() (bool, error)
		initialEtcdMemberList          []*etcdserverpb.Member
		initialObjectsForMachineLister []runtime.Object
		initialObjectsForPodLister     []runtime.Object
		initialObjectsForNodeLister    []runtime.Object
		podLister                      corev1lister.PodLister
		expectedError                  error
		validateFn                     func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient)
	}{
		{
			name:                           "member is added, pod not ready, machine not pending deletion",
			isFunctionalMachineAPIFn:       alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForMachineLister: []runtime.Object{wellKnownMasterMachine()},
			initialObjectsForNodeLister:    []runtime.Object{wellKnownMasterNode()},
			initialObjectsForPodLister: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "etcd-a",
						Namespace: "openshift-etcd",
						Labels:    labels.Set{"app": "etcd"},
					},
					Spec: corev1.PodSpec{
						NodeName: "m-0",
					},
					Status: corev1.PodStatus{
						Phase: "Running",
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "etcd",
								Ready: false,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				}},
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
		// TODO (polynomial) this fails on ipv6
		/*{
			name:                     "member not added, machine pending deletion",
			isFunctionalMachineAPIFn: alwaysTrueIsFunctionalMachineAPIFn,
			initialObjectsForMachineLister: func() []runtime.Object {
				machine := wellKnownMasterMachine()
				machine.DeletionTimestamp = &metav1.Time{}
				return []runtime.Object{machine}
			}(),
			initialObjectsForNodeLister: []runtime.Object{wellKnownMasterNode()},
			initialObjectsForPodLister: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "etcd-a",
						Namespace: "openshift-etcd",
						Labels:    labels.Set{"app": "etcd"},
					},
					Spec: corev1.PodSpec{
						NodeName: "m-0",
					},
					Status: corev1.PodStatus{
						Phase: "Running",
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "etcd",
								Ready: false,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				}},
			validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
				memberList, err := fakeEtcdClient.MemberList(context.TODO())
				if err != nil {
					t.Fatal(err)
				}
				if len(memberList) != 0 {
					t.Errorf("expected exactly 0 members, got %v", len(memberList))
				}
			},
		},*/
		{
			name:                     "member is added, pod not ready, machine pending deletion, machine api off",
			isFunctionalMachineAPIFn: func() (bool, error) { return false, nil },
			initialObjectsForMachineLister: func() []runtime.Object {
				machine := wellKnownMasterMachine()
				machine.DeletionTimestamp = &metav1.Time{}
				return []runtime.Object{machine}
			}(),
			initialObjectsForNodeLister: []runtime.Object{wellKnownMasterNode()},
			initialObjectsForPodLister: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "etcd-a",
						Namespace: "openshift-etcd",
						Labels:    labels.Set{"app": "etcd"},
					},
					Spec: corev1.PodSpec{
						NodeName: "m-0",
					},
					Status: corev1.PodStatus{
						Phase: "Running",
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "etcd",
								Ready: false,
								State: corev1.ContainerState{
									Running: &corev1.ContainerStateRunning{},
								},
							},
						},
					},
				}},
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

		// TODO (polynomial) this fails on ipv6
		/*
			{
				name:                        "member not added, machine not found",
				isFunctionalMachineAPIFn:    alwaysTrueIsFunctionalMachineAPIFn,
				initialObjectsForNodeLister: []runtime.Object{wellKnownMasterNode()},
				initialObjectsForPodLister: []runtime.Object{
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "etcd-a",
							Namespace: "openshift-etcd",
							Labels:    labels.Set{"app": "etcd"},
						},
						Spec: corev1.PodSpec{
							NodeName: "m-0",
						},
						Status: corev1.PodStatus{
							Phase: "Running",
							ContainerStatuses: []corev1.ContainerStatus{
								{
									Name:  "etcd",
									Ready: false,
									State: corev1.ContainerState{
										Running: &corev1.ContainerStateRunning{},
									},
								},
							},
						},
					}},
				expectedError: fmt.Errorf("unable to find machine for member: 10.0.139.78"),
				validateFn: func(t *testing.T, fakeEtcdClient etcdcli.EtcdClient) {
					memberList, err := fakeEtcdClient.MemberList(context.TODO())
					if err != nil {
						t.Fatal(err)
					}
					if len(memberList) != 0 {
						t.Errorf("expected exactly 0 members, got %v", len(memberList))
					}
				},
			},*/
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
			networkIndexer.Add(&configv1.Network{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}, Spec: configv1.NetworkSpec{ServiceNetwork: []string{"172.30.0.0/16"}}})
			networkLister := configv1listers.NewNetworkLister(networkIndexer)
			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, initialObj := range scenario.initialObjectsForNodeLister {
				nodeIndexer.Add(initialObj)
			}
			nodeLister := corev1listers.NewNodeLister(nodeIndexer)

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.initialEtcdMemberList)
			if err != nil {
				t.Fatal(err)
			}

			// act
			target := ClusterMemberController{
				etcdClient:            fakeEtcdClient,
				podLister:             &fakePodLister{fake.NewSimpleClientset(scenario.initialObjectsForPodLister...), "openshift-etcd"},
				machineAPIChecker:     fakeMachineAPIChecker,
				masterMachineLister:   machineLister,
				masterMachineSelector: machineSelector,
				networkLister:         networkLister,
				nodeLister:            nodeLister,
			}
			err = target.reconcileMembers(context.TODO(), eventRecorder)
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from sync() method")
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

func wellKnownMasterMachine() *machinev1beta1.Machine {
	return machineFor("m-0", "10.0.139.78")
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

type fakeMachineAPI struct {
	isMachineAPIFunctional func() (bool, error)
}

func (dm *fakeMachineAPI) IsFunctional() (bool, error) {
	return dm.isMachineAPIFunctional()
}
