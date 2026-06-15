package etcdendpointscontroller

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/v3/mock/mockserver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
	"k8s.io/utils/diff"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

func TestBootstrapAnnotationRemoval(t *testing.T) {
	mockEtcd, err := mockserver.StartMockServers(3)
	if err != nil {
		t.Fatalf("failed to start mock servers: %s", err)
	}
	defer mockEtcd.Stop()

	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMember(0, mockEtcd.Servers),
		u.FakeEtcdMember(1, mockEtcd.Servers),
		u.FakeEtcdMember(2, mockEtcd.Servers),
	}

	defaultObjects := []runtime.Object{
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		&configv1.Infrastructure{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name: ceohelpers.InfrastructureClusterName,
			},
			Status: configv1.InfrastructureStatus{
				ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
		},
	}

	scenarios := []struct {
		name            string
		objects         []runtime.Object
		staticPodStatus *operatorv1.StaticPodOperatorStatus
		etcdMembers     []*etcdserverpb.Member
		expectBootstrap bool
		validateFunc    func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action)
		expectedErr     error
	}{
		{
			// The etcd-endpoint configmap should be created properly if it is missing.
			name: "NewConfigMapAfterDeletion",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: false,
			etcdMembers:     etcdMembers,
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("create", "configmaps") {
						createAction := action.(clientgotesting.CreateAction)
						actual := createAction.GetObject().(*corev1.ConfigMap)
						expected := u.EndpointsConfigMap(endpoints...)
						if !equality.Semantic.DeepEqual(actual, expected) {
							ts.Errorf("%s", diff.ObjectDiff(expected, actual))
						}
					}
				}
			},
		},
		{
			// The bootstrap IP should be deleted because bootstrap reports complete
			// and all nodes have converged on a revision.
			name: "NewClusterBootstrapRemoval",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
				u.EndpointsConfigMap(
					u.WithBootstrapIP("192.0.2.1"),
					u.WithEndpoint(etcdMembers[0].ID, etcdMembers[0].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[1].ID, etcdMembers[1].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[2].ID, etcdMembers[2].PeerURLs[0]),
				),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: false,
			etcdMembers:     etcdMembers,
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				wasValidated := false
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						expected := u.EndpointsConfigMap(endpoints...)
						if !equality.Semantic.DeepEqual(actual, expected) {
							ts.Errorf("%s", diff.ObjectDiff(expected, actual))
						}
						wasValidated = true
						break
					}
				}
				if !wasValidated {
					ts.Errorf("the endpoints configmap wasn't validated")
				}
			},
		},
		{
			// The configmap should remain intact because although bootstrapping
			// reports complete, the nodes are still progressing towards a revision.
			name: "NewClusterBootstrapNodesProgressing",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
				u.EndpointsConfigMap(
					u.WithBootstrapIP("192.0.2.1"),
					u.WithEndpoint(etcdMembers[0].ID, etcdMembers[0].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[1].ID, etcdMembers[1].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[2].ID, etcdMembers[2].PeerURLs[0]),
				),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(2),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: true,
			etcdMembers:     etcdMembers,
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						ts.Errorf("unexpected configmap update: %#v", actual)
					}
				}
			},
		},
		{
			// The configmap should remain intact because although nodes appear
			// to have converged on a revision, bootstrap reports incomplete.
			name: "NewClusterBootstrapProgressing",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("progressing")),
				u.EndpointsConfigMap(
					u.WithBootstrapIP("192.0.2.1"),
					u.WithEndpoint(etcdMembers[0].ID, etcdMembers[0].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[1].ID, etcdMembers[1].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[2].ID, etcdMembers[2].PeerURLs[0]),
				),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: false,
			etcdMembers:     etcdMembers,
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						ts.Errorf("unexpected configmap update: %#v", actual)
					}
				}
			},
		},
		{
			// The configmap should remain intact because there are no changes.
			name: "NewClusterSteadyState",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
				u.EndpointsConfigMap(
					u.WithEndpoint(etcdMembers[0].ID, etcdMembers[0].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[1].ID, etcdMembers[1].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[2].ID, etcdMembers[2].PeerURLs[0]),
				),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: false,
			etcdMembers:     etcdMembers,
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						ts.Errorf("unexpected configmap update: %#v", actual)
					}
				}
			},
		},
		{
			// The configmap should update based on the change in membership
			name: "ClusterUpdateWithMemberChange",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
				u.EndpointsConfigMap(
					u.WithEndpoint(etcdMembers[0].ID, etcdMembers[0].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[1].ID, etcdMembers[1].PeerURLs[0]),
					u.WithEndpoint(etcdMembers[2].ID, etcdMembers[2].PeerURLs[0]),
				),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: false,
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMember(0, mockEtcd.Servers),
				u.FakeEtcdMember(1, mockEtcd.Servers),
				u.FakeEtcdMember(2, mockEtcd.Servers),
				u.FakeEtcdMemberWithoutServer(3),
			},
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				wasValidated := false
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						expected := u.EndpointsConfigMap(endpoints...)
						if !equality.Semantic.DeepEqual(actual, expected) {
							ts.Errorf("%s", diff.ObjectDiff(expected, actual))
						}
						wasValidated = true
						break
					}
				}
				if !wasValidated {
					ts.Errorf("the endpoints configmap wasn't validated")
				}
			},
		},
		{
			// The configmap should be created without a bootstrap IP because the
			// only time the configmap won't already exist is when we've upgraded
			// from a pre-configmap cluster (in which case bootstrapping already
			// happened) or someone has deleted the configmap (which also implies
			// bootstrap already happened). An edge case not specifically accounted
			// for is the configmap being deleted before bootstrapping is complete.
			name: "UpgradedClusterCreateConfigmap",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			expectBootstrap: false,
			etcdMembers:     etcdMembers,
			validateFunc: func(ts *testing.T, endpoints []func(*corev1.ConfigMap), actions []clientgotesting.Action) {
				wasValidated := false
				for _, action := range actions {
					if action.Matches("create", "configmaps") {
						createAction := action.(clientgotesting.CreateAction)
						actual := createAction.GetObject().(*corev1.ConfigMap)
						expected := u.EndpointsConfigMap(endpoints...)
						if !equality.Semantic.DeepEqual(actual, expected) {
							ts.Errorf("%s", diff.ObjectDiff(expected, actual))
						}
						wasValidated = true
						break
					}
				}
				if !wasValidated {
					ts.Errorf("the endpoints configmap wasn't validated")
				}
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				scenario.staticPodStatus,
				nil,
				nil,
			)

			fakeKubeClient := fake.NewSimpleClientset(scenario.objects...)
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}
			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace), "test-etcdendpointscontroller", &corev1.ObjectReference{}, clock.RealClock{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

			for _, obj := range defaultObjects {
				require.NoError(t, indexer.Add(obj))
			}
			for _, obj := range scenario.objects {
				require.NoError(t, indexer.Add(obj))
			}

			controller := &EtcdEndpointsController{
				operatorClient:  fakeOperatorClient,
				etcdClient:      fakeEtcdClient,
				configmapLister: corev1listers.NewConfigMapLister(indexer),
				configmapClient: fakeKubeClient.CoreV1(),
			}

			err = controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			assert.Equal(t, scenario.expectedErr, err)

			var endpoints []func(*corev1.ConfigMap)
			for _, member := range scenario.etcdMembers {
				endpoints = append(endpoints, u.WithEndpoint(member.ID, member.PeerURLs[0]))
			}
			if scenario.expectBootstrap {
				endpoints = append(endpoints, u.WithBootstrapIP("192.0.2.1"))
			}
			if scenario.validateFunc != nil {
				scenario.validateFunc(t, endpoints, fakeKubeClient.Actions())
			}
		})
	}
}

func TestMemberListFallbackToNodeIPs(t *testing.T) {
	network := &configv1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: configv1.NetworkStatus{
			ServiceNetwork: []string{"10.0.0.0/16"},
		},
	}

	masterNodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "master-0",
				Labels: map[string]string{"node-role.kubernetes.io/master": ""},
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "master-1",
				Labels: map[string]string{"node-role.kubernetes.io/master": ""},
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.2"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "master-2",
				Labels: map[string]string{"node-role.kubernetes.io/master": ""},
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.3"},
				},
			},
		},
	}

	scenarios := []struct {
		name           string
		memberListErr  error
		nodes          []*corev1.Node
		expectErr      bool
		expectedData   map[string]string
		expectFallback bool
	}{
		{
			name:          "MemberListFails_FallbackToNodeIPs",
			memberListErr: fmt.Errorf("context deadline exceeded"),
			nodes:         masterNodes,
			expectedData: map[string]string{
				"master-0": "10.0.0.1",
				"master-1": "10.0.0.2",
				"master-2": "10.0.0.3",
			},
			expectFallback: true,
		},
		{
			name:          "MemberListFails_NoNodes_BothFail",
			memberListErr: fmt.Errorf("connection refused"),
			nodes:         []*corev1.Node{},
			expectErr:     true,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				u.StaticPodOperatorStatus(
					u.WithLatestRevision(3),
					u.WithNodeStatusAtCurrentRevision(3),
					u.WithNodeStatusAtCurrentRevision(3),
					u.WithNodeStatusAtCurrentRevision(3),
				),
				nil,
				nil,
			)

			objects := []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			}
			fakeKubeClient := fake.NewSimpleClientset(objects...)

			var opts []etcdcli.FakeClientOption
			if scenario.memberListErr != nil {
				opts = append(opts, etcdcli.WithMemberListError(scenario.memberListErr))
			}
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(nil, opts...)
			require.NoError(t, err)

			coreIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, coreIndexer.Add(&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
			}))
			for _, obj := range objects {
				require.NoError(t, coreIndexer.Add(obj))
			}

			nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, node := range scenario.nodes {
				require.NoError(t, nodeIndexer.Add(node))
			}

			networkIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			require.NoError(t, networkIndexer.Add(network))

			eventRecorder := events.NewRecorder(
				fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-etcdendpointscontroller", &corev1.ObjectReference{}, clock.RealClock{},
			)

			controller := &EtcdEndpointsController{
				operatorClient:  fakeOperatorClient,
				etcdClient:      fakeEtcdClient,
				configmapLister: corev1listers.NewConfigMapLister(coreIndexer),
				configmapClient: fakeKubeClient.CoreV1(),
				nodeLister:      corev1listers.NewNodeLister(nodeIndexer),
				networkLister:   configv1listers.NewNetworkLister(networkIndexer),
			}

			syncErr := controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			if scenario.expectErr {
				require.Error(t, syncErr)
				return
			}
			require.NoError(t, syncErr)

			if scenario.expectFallback {
				for _, action := range fakeKubeClient.Actions() {
					if action.Matches("create", "configmaps") {
						createAction := action.(clientgotesting.CreateAction)
						actual := createAction.GetObject().(*corev1.ConfigMap)
						assert.Equal(t, scenario.expectedData, actual.Data,
							"configmap data should contain node IPs as fallback")
						return
					}
				}
				t.Error("expected configmap create action not found")
			}
		})
	}
}
