package clustermembercontroller

import (
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"testing"
)

type fakePodLister struct {
	client    kubernetes.Interface
	namespace string
}

func (f *fakePodLister) List(selector labels.Selector) (ret []*corev1.Pod, err error) {
	pods, err := f.client.CoreV1().Pods(f.namespace).List(metav1.ListOptions{LabelSelector: selector.String()})
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
		etcdClient etcdcli.EtcdClient
		podLister  corev1lister.PodLister
	}
	tests := []struct {
		name    string
		fields  fields
		want    *corev1.Pod
		wantErr bool
	}{
		{
			name: "test upgrade race",
			fields: fields{
				etcdClient: etcdcli.NewFakeEtcdClient([]*etcdserverpb.Member{
					{
						Name: "etcd-member-node-a",
					},
					{
						Name: "etcd-member-node-b",
					},
					{
						Name: "node-c",
					},
				}),
				podLister: &fakePodLister{fake.NewSimpleClientset(&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						// this will be skipped
						Name:      "etcd-member-node-a",
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
								Name:  "etcd-member",
								Ready: true,
							},
						},
					},
				},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "etcd-node-b",
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
									Ready: false,
								},
							},
						},
					},
					&corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							// this will be skipped
							Name:      "etcd-node-c",
							Namespace: "openshift-etcd",
							Labels:    labels.Set{"app": "etcd"},
						},
						Status: corev1.PodStatus{
							Phase: "Running",
							ContainerStatuses: []corev1.ContainerStatus{
								{
									Name:  "etcd",
									Ready: true,
								},
							},
						},
					}), "openshift-etcd"},
			},
			want: nil,
		},
		{
			name: "test pods with init container failed",
			fields: fields{
				etcdClient: etcdcli.NewFakeEtcdClient([]*etcdserverpb.Member{
					{
						Name: "etcd-a",
					},
				}),
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
				etcdClient: etcdcli.NewFakeEtcdClient([]*etcdserverpb.Member{
					{
						Name: "etcd-a",
					},
				}),
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
				etcdClient: etcdcli.NewFakeEtcdClient([]*etcdserverpb.Member{
					{
						Name: "etcd-a",
					},
				}),
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
			c := &ClusterMemberController{
				etcdClient: tt.fields.etcdClient,
				podLister:  tt.fields.podLister,
			}
			got, err := c.getEtcdPodToAddToMembership()
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
